// Package search implements the authenticated multi-provider web search API.
package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// EnvClientToken is the client bearer-token environment variable.
const EnvClientToken = "SEARCHTOOL_CLIENT_TOKEN"

// EnvListenAddress is the HTTP listen-address environment variable.
const EnvListenAddress = "SEARCHTOOL_LISTEN_ADDRESS"

// EnvBraveAPIKey is the Brave API key environment variable.
const EnvBraveAPIKey = "SEARCHTOOL_BRAVE_API_KEY" // #nosec G101 -- environment variable name, not a key value.

// EnvTavilyAPIKey is the Tavily API key environment variable.
const EnvTavilyAPIKey = "SEARCHTOOL_TAVILY_API_KEY" // #nosec G101 -- environment variable name, not a key value.

// EnvExaAPIKey is the Exa API key environment variable.
const EnvExaAPIKey = "SEARCHTOOL_EXA_API_KEY" // #nosec G101 -- environment variable name, not a key value.

// EnvFirecrawlAPIKey is the Firecrawl API key environment variable.
const EnvFirecrawlAPIKey = "SEARCHTOOL_FIRECRAWL_API_KEY" // #nosec G101 -- environment variable name, not a key value.

// EnvParallelAPIKey is the Parallel API key environment variable.
const EnvParallelAPIKey = "SEARCHTOOL_PARALLEL_API_KEY" // #nosec G101 -- environment variable name, not a key value.

const (
	minimumTokenBytes          = 32
	maximumQueryBytes          = 2048
	maximumFirecrawlQueryRunes = 500
	maximumRequestBytes        = 8192
	maximumResponseBytes       = 2 << 20
	maximumResults             = 20
	maximumConcurrent          = 8
	maximumTitleBytes          = 512
	maximumSnippetBytes        = 4096
	defaultCooldown            = time.Minute
	maximumCooldown            = time.Hour
)

type providerSpec struct {
	name     string
	keyName  string
	endpoint string
}

var providerSpecs = []providerSpec{
	{name: "brave", keyName: EnvBraveAPIKey, endpoint: "https://api.search.brave.com/res/v1/web/search"},
	{name: "tavily", keyName: EnvTavilyAPIKey, endpoint: "https://api.tavily.com/search"},
	{name: "exa", keyName: EnvExaAPIKey, endpoint: "https://api.exa.ai/search"},
	{name: "firecrawl", keyName: EnvFirecrawlAPIKey, endpoint: "https://api.firecrawl.dev/v2/search"},
	{name: "parallel", keyName: EnvParallelAPIKey, endpoint: "https://api.parallel.ai/v1/search"},
}

type provider struct {
	name     string
	key      string
	endpoint string
}

// Router handles authenticated searches and keeps provider cooldowns in memory.
type Router struct {
	client          *http.Client
	clientTokenHash [sha256.Size]byte
	providers       []provider
	permits         chan struct{}
	mu              sync.Mutex
	next            int
	blockedUntil    map[string]time.Time
}

type searchRequest struct {
	Query string `json:"query"`
	Count int    `json:"count"`
}

// Result is a provider-neutral search result.
type Result struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	Snippet string `json:"snippet"`
}

type searchError struct {
	status     int
	code       string
	message    string
	retryAfter string
}

type providerFailure struct {
	retry      bool
	status     int
	retryAfter string
}

func (failure *searchError) Error() string {
	return failure.message
}

// New validates and builds a router with a fixed, non-configurable provider list.
func New(clientToken string, keys map[string]string) (*Router, error) {
	if len(clientToken) < minimumTokenBytes || strings.TrimSpace(clientToken) != clientToken || strings.ContainsAny(clientToken, "\r\n\t ") {
		return nil, errors.New(EnvClientToken + " must be at least 32 bytes without whitespace")
	}
	providers := make([]provider, 0, len(providerSpecs))
	for _, spec := range providerSpecs {
		key := keys[spec.keyName]
		if key == "" || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\r\n\t ") {
			return nil, errors.New(spec.keyName + " must be set without whitespace")
		}
		providers = append(providers, provider{name: spec.name, key: key, endpoint: spec.endpoint})
	}
	return &Router{
		client:          &http.Client{Timeout: 20 * time.Second, CheckRedirect: rejectRedirects},
		clientTokenHash: sha256.Sum256([]byte(clientToken)),
		providers:       providers,
		permits:         make(chan struct{}, maximumConcurrent),
		blockedUntil:    make(map[string]time.Time, len(providers)),
	}, nil
}

// ServeHTTP exposes health checks and authenticated POST /search requests.
func (router *Router) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/health", "/readyz":
		if request.Method != http.MethodGet {
			writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	case "/search":
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "")
			return
		}
	default:
		writeError(writer, http.StatusNotFound, "not_found", "not found", "")
		return
	}
	if !router.authenticate(request.Header.Get("Authorization")) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeError(writer, http.StatusUnauthorized, "unauthorized", "unauthorized", "")
		return
	}
	select {
	case router.permits <- struct{}{}:
		defer func() { <-router.permits }()
	default:
		writeError(writer, http.StatusServiceUnavailable, "busy", "search router is at capacity", "1")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maximumRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input searchRequest
	if err := decoder.Decode(&input); err != nil {
		writeRequestDecodeError(writer, err, "request must contain a valid JSON query")
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeRequestDecodeError(writer, err, "request must contain exactly one JSON object")
		return
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len(input.Query) > maximumQueryBytes || input.Count < 0 || input.Count > maximumResults {
		writeError(writer, http.StatusBadRequest, "invalid_request", "query must be non-empty and count must be between 0 and 20", "")
		return
	}
	if input.Count == 0 {
		input.Count = 5
	}
	results, err := router.search(request.Context(), input.Query, input.Count)
	if err != nil {
		if failure, ok := errors.AsType[*searchError](err); ok {
			writeError(writer, failure.status, failure.code, failure.message, failure.retryAfter)
			return
		}
		writeError(writer, http.StatusBadGateway, "upstream_error", "search provider failed", "")
		return
	}
	writeJSON(writer, http.StatusOK, map[string][]Result{"results": results})
}

func writeRequestDecodeError(writer http.ResponseWriter, err error, message string) {
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large", "")
		return
	}
	writeError(writer, http.StatusBadRequest, "invalid_request", message, "")
}

func (router *Router) authenticate(header string) bool {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(digest[:], router.clientTokenHash[:]) == 1
}

func (router *Router) search(ctx context.Context, query string, count int) ([]Result, error) {
	providers := router.availableProviders()
	if len(providers) == 0 {
		return nil, &searchError{status: http.StatusServiceUnavailable, code: "providers_cooling_down", message: "all search providers are cooling down", retryAfter: router.retryAfter()}
	}
	for _, provider := range providers {
		if provider.name == "firecrawl" && utf8.RuneCountInString(query) > maximumFirecrawlQueryRunes {
			// Firecrawl rejects queries over 500 characters; leave them to the other providers.
			continue
		}
		results, failure := router.searchProvider(ctx, provider, query, count)
		if failure == nil {
			return results, nil
		}
		if failure.status == http.StatusTooManyRequests {
			router.cooldown(provider.name, retryDelay(failure.retryAfter, time.Now()))
		}
		if !failure.retry {
			return nil, &searchError{status: http.StatusBadGateway, code: "upstream_error", message: "search provider failed"}
		}
	}
	return nil, &searchError{status: http.StatusServiceUnavailable, code: "providers_unavailable", message: "all search providers are temporarily unavailable", retryAfter: router.retryAfter()}
}

func (router *Router) availableProviders() []provider {
	router.mu.Lock()
	defer router.mu.Unlock()
	start := router.next
	router.next = (router.next + 1) % len(router.providers)
	now := time.Now()
	available := make([]provider, 0, len(router.providers))
	for offset := range len(router.providers) {
		candidate := router.providers[(start+offset)%len(router.providers)]
		if !now.Before(router.blockedUntil[candidate.name]) {
			available = append(available, candidate)
		}
	}
	return available
}

func (router *Router) cooldown(name string, delay time.Duration) {
	router.mu.Lock()
	defer router.mu.Unlock()
	until := time.Now().Add(delay)
	if until.After(router.blockedUntil[name]) {
		router.blockedUntil[name] = until
	}
}

func (router *Router) retryAfter() string {
	router.mu.Lock()
	defer router.mu.Unlock()
	var wait time.Duration
	for _, until := range router.blockedUntil {
		if remaining := time.Until(until); remaining > 0 && (wait == 0 || remaining < wait) {
			wait = remaining
		}
	}
	if wait == 0 {
		return ""
	}
	seconds := int64(wait / time.Second)
	if wait%time.Second != 0 {
		seconds++
	}
	return strconv.FormatInt(max(seconds, 1), 10)
}

func (router *Router) searchProvider(ctx context.Context, provider provider, query string, count int) ([]Result, *providerFailure) {
	var body io.Reader
	method := http.MethodPost
	endpoint := provider.endpoint
	if provider.name == "brave" {
		method = http.MethodGet
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, &providerFailure{}
		}
		values := parsed.Query()
		values.Set("q", query)
		values.Set("count", strconv.Itoa(count))
		parsed.RawQuery = values.Encode()
		endpoint = parsed.String()
	} else {
		payload := map[string]any{"query": query}
		switch provider.name {
		case "tavily":
			payload["max_results"] = count
			payload["search_depth"] = "basic"
			payload["include_raw_content"] = false
			payload["include_images"] = false
		case "exa":
			payload["type"] = "lite"
			payload["numResults"] = count
			payload["contents"] = map[string]any{"highlights": map[string]string{"query": query}}
		case "firecrawl":
			payload["limit"] = count
		case "parallel":
			payload["objective"] = query
			payload["search_queries"] = []string{query}
			payload["mode"] = "turbo"
			payload["advanced_settings"] = map[string]int{"max_results": count}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, &providerFailure{}
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, &providerFailure{}
	}
	request.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	switch provider.name {
	case "brave":
		request.Header.Set("X-Subscription-Token", provider.key)
	case "parallel":
		request.Header.Set("x-api-key", provider.key)
	default:
		request.Header.Set("Authorization", "Bearer "+provider.key)
	}
	response, err := router.client.Do(request)
	if err != nil {
		if isTimeout(err) {
			return nil, &providerFailure{retry: true}
		}
		return nil, &providerFailure{}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, &providerFailure{retry: true, status: response.StatusCode, retryAfter: response.Header.Get("Retry-After")}
	}
	if response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= http.StatusInternalServerError {
		return nil, &providerFailure{retry: true, status: response.StatusCode}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &providerFailure{status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return nil, &providerFailure{retry: isTimeout(err)}
	}
	if len(data) > maximumResponseBytes {
		return nil, &providerFailure{}
	}
	results, err := decodeResults(provider.name, data, count)
	if err != nil {
		return nil, &providerFailure{}
	}
	return results, nil
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout()
}

func decodeResults(name string, data []byte, count int) ([]Result, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("invalid search response")
	}
	if _, hasError := fields["error"]; hasError {
		return nil, errors.New("search response contains an error")
	}
	if raw, ok := fields["success"]; ok {
		var success bool
		if json.Unmarshal(raw, &success) != nil || !success {
			return nil, errors.New("search response was unsuccessful")
		}
	}
	type hit struct {
		Title       string   `json:"title"`
		URL         string   `json:"url"`
		Description string   `json:"description"`
		Content     string   `json:"content"`
		Highlights  []string `json:"highlights"`
		Excerpts    []string `json:"excerpts"`
	}
	var rows json.RawMessage
	switch name {
	case "brave":
		var response struct {
			Web struct {
				Results json.RawMessage `json:"results"`
			} `json:"web"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, err
		}
		rows = response.Web.Results
	case "firecrawl":
		var response struct {
			Data struct {
				Web json.RawMessage `json:"web"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, err
		}
		rows = response.Data.Web
	default:
		var response struct {
			Results json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, err
		}
		rows = response.Results
	}
	trimmedRows := bytes.TrimSpace(rows)
	if len(trimmedRows) == 0 || bytes.Equal(trimmedRows, []byte("null")) {
		return nil, errors.New("search response has no result array")
	}
	var hits []*hit
	if err := json.Unmarshal(rows, &hits); err != nil || hits == nil {
		return nil, errors.New("search response has invalid results")
	}
	results := make([]Result, 0, min(count, len(hits)))
	for _, item := range hits {
		if item == nil {
			return nil, errors.New("search response has a null result")
		}
		if !validResultURL(item.URL) {
			continue
		}
		snippet := item.Description
		switch name {
		case "tavily":
			snippet = item.Content
		case "exa":
			snippet = strings.Join(item.Highlights, " ")
		case "parallel":
			snippet = strings.Join(item.Excerpts, " ")
		}
		results = append(results, Result{
			Title:   limitText(item.Title, maximumTitleBytes),
			Link:    item.URL,
			Snippet: limitText(snippet, maximumSnippetBytes),
		})
		if len(results) == count {
			break
		}
	}
	return results, nil
}

func validResultURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.User == nil && parsed.Hostname() != "" && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
}

func limitText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func retryDelay(value string, now time.Time) time.Duration {
	delay := defaultCooldown
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds >= int64(maximumCooldown/time.Second) {
			return maximumCooldown
		}
		delay = time.Duration(seconds) * time.Second
	} else if retryAt, err := http.ParseTime(value); err == nil {
		delay = retryAt.Sub(now)
	}
	if delay < time.Second {
		return time.Second
	}
	return min(delay, maximumCooldown)
}

func rejectRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code, message, retryAfter string) {
	if retryAfter != "" {
		writer.Header().Set("Retry-After", retryAfter)
	}
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
