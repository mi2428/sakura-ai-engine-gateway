// Package gateway implements the Sakura AI Engine API gateway.
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddress       = ":8080"
	defaultUpstreamURL         = "https://api.ai.sakura.ad.jp"
	defaultMaxRequestBytes     = 64 << 20
	defaultMemoryCgroupPath    = "/sys/fs/cgroup"
	defaultMaxQueuedRequests   = 64
	defaultQueueTimeout        = time.Minute
	defaultMaxRetries          = 5
	defaultBaseBackoff         = 10 * time.Second
	defaultMaxBackoff          = 120 * time.Second
	defaultJitter              = time.Second
	defaultRetryBudget         = 3200 * time.Second
	defaultUpstreamIdleTimeout = 420 * time.Second
	retrySafetyMargin          = 300 * time.Second
	maximumRetryCount          = 20
	maximumQueuedRequests      = 1024
	maximumRequestBytes        = 256 << 20
	maximumConfiguredDuration  = 24 * time.Hour
)

// Config contains validated runtime configuration. Secrets are never formatted
// into errors or logs.
type Config struct {
	ListenAddress       string
	UpstreamURL         *url.URL
	AccountTokens       []string
	MaxRequestBytes     int64
	MemoryCgroupPath    string
	MaxQueuedRequests   int
	QueueTimeout        time.Duration
	MaxRetries          int
	BaseBackoff         time.Duration
	MaxBackoff          time.Duration
	Jitter              time.Duration
	RetryBudget         time.Duration
	UpstreamIdleTimeout time.Duration
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
	maxRequestBytes, err := int64Env("GATEWAY_MAX_REQUEST_BYTES", defaultMaxRequestBytes)
	if err != nil {
		return Config{}, err
	}
	maxQueuedRequests, err := intEnv("GATEWAY_MAX_QUEUED_REQUESTS", defaultMaxQueuedRequests)
	if err != nil {
		return Config{}, err
	}
	queueTimeout, err := durationEnv("GATEWAY_QUEUE_TIMEOUT", defaultQueueTimeout)
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
		ListenAddress:       listenAddress,
		UpstreamURL:         upstreamURL,
		AccountTokens:       accountTokens,
		MaxRequestBytes:     maxRequestBytes,
		MemoryCgroupPath:    envOr("GATEWAY_MEMORY_CGROUP_PATH", defaultMemoryCgroupPath),
		MaxQueuedRequests:   maxQueuedRequests,
		QueueTimeout:        queueTimeout,
		MaxRetries:          maxRetries,
		BaseBackoff:         baseBackoff,
		MaxBackoff:          maxBackoff,
		Jitter:              jitter,
		RetryBudget:         retryBudget,
		UpstreamIdleTimeout: upstreamIdleTimeout,
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
	if !filepath.IsAbs(c.MemoryCgroupPath) {
		return errors.New("GATEWAY_MEMORY_CGROUP_PATH must be an absolute cgroup v2 directory")
	}
	if c.MaxRequestBytes <= 0 || c.MaxRequestBytes > maximumRequestBytes || c.MaxQueuedRequests <= 0 || c.MaxQueuedRequests > maximumQueuedRequests || c.QueueTimeout <= 0 || c.QueueTimeout > maximumConfiguredDuration || c.MaxRetries < 0 || c.MaxRetries > maximumRetryCount {
		return errors.New("gateway request and queue limits exceed safe bounds")
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
