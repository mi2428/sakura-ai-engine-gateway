package gateway

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testConfig(upstream *url.URL) Config {
	if upstream == nil {
		upstream, _ = url.Parse("https://api.ai.sakura.ad.jp")
	}
	return Config{
		ListenAddress:          ":8080",
		UpstreamURL:            upstream,
		AccountTokens:          []string{"account-token-a", "account-token-b"},
		ClientTokens:           map[string]string{"hermes": strings.Repeat("h", 32), "open-webui": strings.Repeat("w", 32), "opencode": strings.Repeat("o", 32)},
		MaxRequestBytes:        64 << 20,
		MaxConcurrent:          24,
		MaxConcurrentPerClient: 24,
		MaxRetries:             5,
		BaseBackoff:            10 * time.Second,
		MaxBackoff:             120 * time.Second,
		Jitter:                 time.Second,
		RetryBudget:            3200 * time.Second,
		UpstreamIdleTimeout:    420 * time.Second,
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS", `["provider-a","provider-b"]`)
	t.Setenv("GATEWAY_CLIENT_TOKENS", clientTokenJSON(t, map[string]string{
		"open-webui": strings.Repeat("w", 32),
		"hermes":     strings.Repeat("h", 32),
		"opencode":   strings.Repeat("o", 32),
	}))
	t.Setenv("SAKURA_RETRY_MAX", "2")
	t.Setenv("SAKURA_RETRY_BASE_INTERVAL", "15ms")
	t.Setenv("SAKURA_RETRY_MAX_INTERVAL", "1s")
	t.Setenv("SAKURA_RETRY_JITTER", "0s")
	t.Setenv("SAKURA_RETRY_BUDGET", "3200s")
	t.Setenv("SAKURA_UPSTREAM_IDLE_TIMEOUT", "420s")

	config, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.AccountTokens) != 2 || config.AccountTokens[0] != "provider-a" {
		t.Fatalf("account tokens = %q", config.AccountTokens)
	}
	if config.ClientTokens["open-webui"] != strings.Repeat("w", 32) {
		t.Fatal("client token map was not loaded")
	}
	if config.MaxRetries != 2 || config.BaseBackoff != 15*time.Millisecond {
		t.Fatalf("retry config = (%d, %s)", config.MaxRetries, config.BaseBackoff)
	}
	if config.MaxConcurrent != 24 || config.MaxConcurrentPerClient != 24 || config.MaxRequestBytes != 64<<20 {
		t.Fatalf("concurrency/request defaults = (%d, %d, %d)", config.MaxConcurrent, config.MaxConcurrentPerClient, config.MaxRequestBytes)
	}
}

func TestLoadConfigRejectsInvalidCredentialsAndBudget(t *testing.T) {
	tests := []struct {
		name         string
		upstreamJSON string
		clients      map[string]string
		clientsJSON  string
		retryBudget  string
	}{
		{name: "missing accounts", clients: map[string]string{"opencode": strings.Repeat("o", 32)}},
		{name: "duplicate accounts", upstreamJSON: `["provider-a","provider-a"]`, clients: map[string]string{"opencode": strings.Repeat("o", 32)}},
		{name: "short client key", upstreamJSON: `["provider-a"]`, clientsJSON: `{"opencode":"short"}`},
		{name: "duplicate client key", upstreamJSON: `["provider-a"]`, clients: map[string]string{"hermes": strings.Repeat("x", 32), "opencode": strings.Repeat("x", 32)}},
		{name: "missing client", upstreamJSON: `["provider-a"]`},
		{name: "insufficient retry budget", upstreamJSON: `["provider-a"]`, clients: map[string]string{"opencode": strings.Repeat("o", 32)}, retryBudget: "1s"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS", test.upstreamJSON)
			clientTokens := test.clientsJSON
			if test.clients != nil {
				clientTokens = clientTokenJSON(t, test.clients)
			}
			t.Setenv("GATEWAY_CLIENT_TOKENS", clientTokens)
			if test.retryBudget != "" {
				t.Setenv("SAKURA_RETRY_BUDGET", test.retryBudget)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() succeeded with invalid settings")
			}
		})
	}
}

func clientTokenJSON(t *testing.T, clients map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(clients)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestConfigRejectsInvalidUpstreamAndLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{name: "upstream path", change: func(config *Config) { config.UpstreamURL.Path = "/v1" }},
		{name: "no account token", change: func(config *Config) { config.AccountTokens = nil }},
		{name: "no client token", change: func(config *Config) { config.ClientTokens = nil }},
		{name: "zero concurrency", change: func(config *Config) { config.MaxConcurrent = 0 }},
		{name: "client concurrency exceeds global", change: func(config *Config) { config.MaxConcurrentPerClient = config.MaxConcurrent + 1 }},
		{name: "unbounded buffered body memory", change: func(config *Config) { config.MaxRequestBytes = 128 << 20 }},
		{name: "negative retry count", change: func(config *Config) { config.MaxRetries = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig(nil)
			test.change(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("Validate() succeeded with invalid configuration")
			}
		})
	}
}

func TestConfigAcceptsMoreThanFormerAccountTokenCap(t *testing.T) {
	t.Parallel()
	config := testConfig(nil)
	config.AccountTokens = make([]string, 129)
	for index := range config.AccountTokens {
		config.AccountTokens[index] = "upstream-" + strconv.Itoa(index)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() rejected configured tokens: %v", err)
	}
}

func TestClientTokenParserRejectsDuplicateIDsAndTrailingJSON(t *testing.T) {
	t.Parallel()
	token := strings.Repeat("x", 32)
	duplicateID := `{"opencode":"` + token + `","opencode":"` + strings.Repeat("y", 32) + `"}`
	for name, raw := range map[string]string{
		"duplicate ID":     duplicateID,
		"trailing JSON":    `{"opencode":"` + token + `"}{}`,
		"non-string value": `{"opencode":12345678901234567890123456789012}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseClientTokens(raw); err == nil {
				t.Fatal("parseClientTokens() accepted invalid JSON")
			}
		})
	}
}
