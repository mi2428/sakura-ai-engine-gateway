// Package gateway implements the authenticated Sakura AI Engine API gateway.
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddress          = ":8080"
	defaultUpstreamURL            = "https://api.ai.sakura.ad.jp"
	defaultMaxRequestBytes        = 64 << 20
	defaultMaxConcurrent          = 24
	defaultMaxConcurrentPerClient = 24
	defaultMaxRetries             = 5
	defaultBaseBackoff            = 10 * time.Second
	defaultMaxBackoff             = 120 * time.Second
	defaultJitter                 = time.Second
	defaultRetryBudget            = 3200 * time.Second
	defaultUpstreamIdleTimeout    = 420 * time.Second
	retrySafetyMargin             = 300 * time.Second
	minimumClientTokenBytes       = 32
	maximumRetryCount             = 20
	maximumGatewayConcurrency     = 1024
	maximumConfiguredClients      = 64
	maximumRequestBytes           = 256 << 20
	maximumBufferedRequestBytes   = 1536 << 20 // Requests are buffered for safe replay on retry.
	maximumConfiguredDuration     = 24 * time.Hour
)

var clientIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Config contains validated runtime configuration. Secrets are never formatted
// into errors or logs.
type Config struct {
	ListenAddress          string
	UpstreamURL            *url.URL
	AccountTokens          []string
	ClientTokens           map[string]string
	MaxRequestBytes        int64
	MaxConcurrent          int
	MaxConcurrentPerClient int
	MaxRetries             int
	BaseBackoff            time.Duration
	MaxBackoff             time.Duration
	Jitter                 time.Duration
	RetryBudget            time.Duration
	UpstreamIdleTimeout    time.Duration
}

// LoadConfig reads and validates all configuration from the environment.
func LoadConfig() (Config, error) {
	listenAddress := envOr("GATEWAY_LISTEN_ADDRESS", defaultListenAddress)
	upstreamURL, err := parseUpstreamURL(envOr("SAKURA_AI_ENGINE_API_URL", defaultUpstreamURL))
	if err != nil {
		return Config{}, err
	}

	accountTokens, err := parseAccountTokens(os.Getenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS"))
	if err != nil {
		return Config{}, err
	}
	clientTokens, err := parseClientTokens(os.Getenv("GATEWAY_CLIENT_TOKENS"))
	if err != nil {
		return Config{}, err
	}

	maxRequestBytes, err := int64Env("GATEWAY_MAX_REQUEST_BYTES", defaultMaxRequestBytes)
	if err != nil {
		return Config{}, err
	}
	maxConcurrent, err := intEnv("GATEWAY_MAX_CONCURRENT_REQUESTS", defaultMaxConcurrent)
	if err != nil {
		return Config{}, err
	}
	maxConcurrentPerClient, err := intEnv("GATEWAY_MAX_CONCURRENT_REQUESTS_PER_CLIENT", defaultMaxConcurrentPerClient)
	if err != nil {
		return Config{}, err
	}
	maxRetries, err := intEnv("SAKURA_RETRY_MAX", defaultMaxRetries)
	if err != nil {
		return Config{}, err
	}
	baseBackoff, err := durationEnv("SAKURA_RETRY_BASE_INTERVAL", defaultBaseBackoff)
	if err != nil {
		return Config{}, err
	}
	maxBackoff, err := durationEnv("SAKURA_RETRY_MAX_INTERVAL", defaultMaxBackoff)
	if err != nil {
		return Config{}, err
	}
	jitter, err := durationEnv("SAKURA_RETRY_JITTER", defaultJitter)
	if err != nil {
		return Config{}, err
	}
	retryBudget, err := durationEnv("SAKURA_RETRY_BUDGET", defaultRetryBudget)
	if err != nil {
		return Config{}, err
	}
	upstreamIdleTimeout, err := durationEnv("SAKURA_UPSTREAM_IDLE_TIMEOUT", defaultUpstreamIdleTimeout)
	if err != nil {
		return Config{}, err
	}

	config := Config{
		ListenAddress:          listenAddress,
		UpstreamURL:            upstreamURL,
		AccountTokens:          accountTokens,
		ClientTokens:           clientTokens,
		MaxRequestBytes:        maxRequestBytes,
		MaxConcurrent:          maxConcurrent,
		MaxConcurrentPerClient: maxConcurrentPerClient,
		MaxRetries:             maxRetries,
		BaseBackoff:            baseBackoff,
		MaxBackoff:             maxBackoff,
		Jitter:                 jitter,
		RetryBudget:            retryBudget,
		UpstreamIdleTimeout:    upstreamIdleTimeout,
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate rejects incomplete or internally inconsistent configuration.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddress) == "" {
		return errors.New("GATEWAY_LISTEN_ADDRESS must not be empty")
	}
	if c.UpstreamURL == nil || c.UpstreamURL.Hostname() == "" || c.UpstreamURL.User != nil || c.UpstreamURL.RawQuery != "" || c.UpstreamURL.Fragment != "" || c.UpstreamURL.Path != "" {
		return errors.New("SAKURA_AI_ENGINE_API_URL must be an absolute HTTP(S) URL")
	}
	if !slices.Contains([]string{"http", "https"}, c.UpstreamURL.Scheme) {
		return errors.New("SAKURA_AI_ENGINE_API_URL must use HTTP or HTTPS")
	}
	if len(c.AccountTokens) == 0 {
		return errors.New("SAKURA_AI_ENGINE_ACCOUNT_TOKENS must contain at least one token")
	}
	if len(c.ClientTokens) == 0 {
		return errors.New("GATEWAY_CLIENT_TOKENS must contain at least one client token")
	}
	if len(c.ClientTokens) > maximumConfiguredClients {
		return errors.New("GATEWAY_CLIENT_TOKENS exceeds the supported client count")
	}
	if c.MaxRequestBytes <= 0 || c.MaxRequestBytes > maximumRequestBytes || c.MaxConcurrent <= 0 || c.MaxConcurrent > maximumGatewayConcurrency || c.MaxConcurrentPerClient <= 0 || c.MaxConcurrentPerClient > c.MaxConcurrent || c.MaxRequestBytes > maximumBufferedRequestBytes/int64(c.MaxConcurrent) || c.MaxRetries < 0 || c.MaxRetries > maximumRetryCount {
		return errors.New("gateway request and concurrency limits exceed safe bounds")
	}
	if c.BaseBackoff < 0 || c.MaxBackoff < c.BaseBackoff || c.Jitter < 0 || c.RetryBudget <= 0 || c.UpstreamIdleTimeout <= 0 || c.MaxBackoff > maximumConfiguredDuration || c.Jitter > maximumConfiguredDuration || c.RetryBudget > maximumConfiguredDuration || c.UpstreamIdleTimeout > maximumConfiguredDuration {
		return errors.New("retry and upstream timeout settings are inconsistent")
	}
	seenAccounts := make(map[string]struct{}, len(c.AccountTokens))
	for _, token := range c.AccountTokens {
		if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\t ") {
			return errors.New("SAKURA_AI_ENGINE_ACCOUNT_TOKENS must not contain empty or whitespace-padded tokens")
		}
		if _, exists := seenAccounts[token]; exists {
			return errors.New("SAKURA_AI_ENGINE_ACCOUNT_TOKENS must not contain duplicates")
		}
		seenAccounts[token] = struct{}{}
	}
	seenClientTokens := make(map[string]struct{}, len(c.ClientTokens))
	for clientID, token := range c.ClientTokens {
		if !clientIDPattern.MatchString(clientID) || len(token) < minimumClientTokenBytes || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\t ") {
			return errors.New("GATEWAY_CLIENT_TOKENS contains an invalid client ID or token")
		}
		if _, exists := seenClientTokens[token]; exists {
			return errors.New("GATEWAY_CLIENT_TOKENS must use a unique token for each client")
		}
		seenClientTokens[token] = struct{}{}
	}
	if c.RetryBudget < minimumRetryBudget(c) {
		return errors.New("SAKURA_RETRY_BUDGET is too short for the configured attempts and backoff")
	}
	return nil
}

func minimumRetryBudget(c Config) time.Duration {
	minimum := time.Duration(c.MaxRetries+1)*c.UpstreamIdleTimeout + retrySafetyMargin
	for attempt := range c.MaxRetries {
		minimum += retryBackoff(c.BaseBackoff, c.MaxBackoff, attempt) + c.Jitter
	}
	return minimum
}

func parseUpstreamURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("SAKURA_AI_ENGINE_API_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("SAKURA_AI_ENGINE_API_URL must not include a path")
	}
	parsed.Path = ""
	return parsed, nil
}

func parseAccountTokens(raw string) ([]string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var tokens []string
	if err := decoder.Decode(&tokens); err != nil {
		return nil, errors.New("SAKURA_AI_ENGINE_ACCOUNT_TOKENS must be a JSON array of tokens")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("SAKURA_AI_ENGINE_ACCOUNT_TOKENS must contain exactly one JSON array")
	}
	if len(tokens) == 0 {
		return nil, errors.New("SAKURA_AI_ENGINE_ACCOUNT_TOKENS must contain at least one token")
	}
	seen := make(map[string]struct{})
	for _, token := range tokens {
		if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\t ") {
			return nil, errors.New("SAKURA_AI_ENGINE_ACCOUNT_TOKENS must not contain empty or whitespace-padded tokens")
		}
		if _, exists := seen[token]; exists {
			return nil, errors.New("SAKURA_AI_ENGINE_ACCOUNT_TOKENS must not contain duplicates")
		}
		seen[token] = struct{}{}
	}
	return tokens, nil
}

func parseClientTokens(raw string) (map[string]string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("GATEWAY_CLIENT_TOKENS must be a JSON object of client IDs to tokens")
	}
	tokens := make(map[string]string)
	seenTokens := make(map[string]struct{}, maximumConfiguredClients)
	for decoder.More() {
		key, err := decoder.Token()
		clientID, ok := key.(string)
		if err != nil || !ok {
			return nil, errors.New("GATEWAY_CLIENT_TOKENS must use string client IDs")
		}
		if _, exists := tokens[clientID]; exists {
			return nil, errors.New("GATEWAY_CLIENT_TOKENS must not repeat client IDs")
		}
		var token string
		if err := decoder.Decode(&token); err != nil {
			return nil, errors.New("GATEWAY_CLIENT_TOKENS values must be strings")
		}
		if !clientIDPattern.MatchString(clientID) {
			return nil, fmt.Errorf("GATEWAY_CLIENT_TOKENS contains invalid client ID %q", clientID)
		}
		if len(token) < minimumClientTokenBytes {
			return nil, fmt.Errorf("GATEWAY_CLIENT_TOKENS token for %q must be at least %d bytes", clientID, minimumClientTokenBytes)
		}
		if _, exists := seenTokens[token]; exists {
			return nil, errors.New("GATEWAY_CLIENT_TOKENS must use a unique token for each client")
		}
		tokens[clientID] = token
		seenTokens[token] = struct{}{}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, errors.New("GATEWAY_CLIENT_TOKENS contains an incomplete JSON object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("GATEWAY_CLIENT_TOKENS must contain exactly one JSON object")
	}
	if len(tokens) == 0 {
		return nil, errors.New("GATEWAY_CLIENT_TOKENS must contain at least one client token")
	}
	return tokens, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func intEnv(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func int64Env(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid Go duration", name)
	}
	return parsed, nil
}
