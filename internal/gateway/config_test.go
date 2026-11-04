package gateway

import (
	"net/url"
	"strconv"
	"testing"
	"time"
)

func testConfig(upstream *url.URL) Config {
	if upstream == nil {
		upstream, _ = url.Parse("https://api.ai.sakura.ad.jp")
	}
	return Config{
		ListenAddress:       ":8080",
		UpstreamURL:         upstream,
		AccountTokens:       []string{"account-token-a", "account-token-b"},
		MaxRequestBytes:     64 << 20,
		MemoryCgroupPath:    defaultMemoryCgroupPath,
		MaxQueuedRequests:   64,
		QueueTimeout:        time.Minute,
		MaxRetries:          5,
		BaseBackoff:         10 * time.Second,
		MaxBackoff:          120 * time.Second,
		Jitter:              time.Second,
		RetryBudget:         3200 * time.Second,
		UpstreamIdleTimeout: 420 * time.Second,
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS", `["provider-a","provider-b"]`)
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
	if config.MaxRetries != 2 || config.BaseBackoff != 15*time.Millisecond {
		t.Fatalf("retry config = (%d, %s)", config.MaxRetries, config.BaseBackoff)
	}
	if config.MaxQueuedRequests != 64 || config.QueueTimeout != time.Minute || config.MaxRequestBytes != 64<<20 || config.MemoryCgroupPath != defaultMemoryCgroupPath {
		t.Fatalf("request/queue defaults = %+v", config)
	}
}

func TestLoadConfigRejectsInvalidCredentialsAndBudget(t *testing.T) {
	tests := []struct {
		name         string
		upstreamJSON string
		retryBudget  string
	}{
		{name: "missing accounts"},
		{name: "duplicate accounts", upstreamJSON: `["provider-a","provider-a"]`},
		{name: "insufficient retry budget", upstreamJSON: `["provider-a"]`, retryBudget: "1s"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS", test.upstreamJSON)
			if test.retryBudget != "" {
				t.Setenv("SAKURA_RETRY_BUDGET", test.retryBudget)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() succeeded with invalid settings")
			}
		})
	}
}

func TestConfigRejectsInvalidUpstreamAndLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{name: "upstream path", change: func(config *Config) { config.UpstreamURL.Path = "/v1" }},
		{name: "no account token", change: func(config *Config) { config.AccountTokens = nil }},
		{name: "relative cgroup path", change: func(config *Config) { config.MemoryCgroupPath = "relative" }},
		{name: "zero queue capacity", change: func(config *Config) { config.MaxQueuedRequests = 0 }},
		{name: "excessive queue capacity", change: func(config *Config) { config.MaxQueuedRequests = maximumQueuedRequests + 1 }},
		{name: "zero queue timeout", change: func(config *Config) { config.QueueTimeout = 0 }},
		{name: "excessive body size", change: func(config *Config) { config.MaxRequestBytes = maximumRequestBytes + 1 }},
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
