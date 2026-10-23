package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startTestGateway(t *testing.T, config Config, upstream http.Handler) (*httptest.Server, *Gateway) {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	config.UpstreamURL, _ = url.Parse(upstreamServer.URL)
	config.UpstreamIdleTimeout = 100 * time.Millisecond
	config.MaxRetries = 2
	config.BaseBackoff = time.Millisecond
	config.MaxBackoff = 5 * time.Millisecond
	config.Jitter = 0
	config.RetryBudget = 3200 * time.Second
	proxy, err := New(config, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(func() {
		server.Close()
		proxy.Close()
	})
	return server, proxy
}

type testResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func sendRequest(t *testing.T, client *http.Client, method, endpoint, token string, body io.Reader, headers http.Header) testResponse {
	t.Helper()
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeError != nil {
		t.Errorf("close response body: %v", closeError)
	}
	return testResponse{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: responseBody}
}

func TestGatewayHealthAndClientAuthentication(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamRequests.Add(1)
		if request.URL.Path != "/v1/models" || request.URL.RawQuery != "limit=2" {
			t.Errorf("upstream URL = %s?%s", request.URL.Path, request.URL.RawQuery)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer account-token-a" {
			t.Errorf("upstream Authorization = %q", got)
		}
		if request.Header.Get("X-OpenWebUI-Mode") != "" {
			t.Error("Open WebUI control header leaked upstream")
		}
		if request.Header.Get("X-Client-Hop") != "" {
			t.Error("Connection-nominated header leaked upstream")
		}
		for _, name := range []string{"Cookie", "Tailscale-User-Login", "X-Forwarded-For"} {
			if request.Header.Get(name) != "" {
				t.Errorf("untrusted client header %q leaked upstream", name)
			}
		}
		writer.Header().Set("Connection", "X-Upstream-Hop")
		writer.Header().Set("X-Upstream-Hop", "must-not-pass")
		writer.Header().Set("X-Sakura-Retry-Count", "spoofed")
		writer.Header().Add("Set-Cookie", "session=must-not-pass")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"data":[]}`)
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	client := server.Client()

	response := sendRequest(t, client, http.MethodGet, server.URL+"/health", "", nil, nil)
	if response.StatusCode != http.StatusOK || string(response.Body) != `{"status":"ok"}` {
		t.Fatalf("health response = %d %s", response.StatusCode, response.Body)
	}
	response = sendRequest(t, client, http.MethodGet, server.URL+"/v1/models", "wrong-token", nil, nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.StatusCode)
	}
	if upstreamRequests.Load() != 0 {
		t.Fatalf("unauthorized request reached upstream %d times", upstreamRequests.Load())
	}

	headers := http.Header{
		"Connection":           {"X-Client-Hop"},
		"X-Client-Hop":         {"must-not-pass"},
		"X-OpenWebUI-Mode":     {"true"},
		"Cookie":               {"session=secret"},
		"Tailscale-User-Login": {"user@example.invalid"},
		"X-Forwarded-For":      {"192.0.2.1"},
	}
	response = sendRequest(t, client, http.MethodGet, server.URL+"/v1/models?limit=2", strings.Repeat("o", 32), nil, headers)
	if response.StatusCode != http.StatusOK || string(response.Body) != `{"data":[]}` {
		t.Fatalf("authorized response = %d %s", response.StatusCode, response.Body)
	}
	if response.Header.Get("X-Upstream-Hop") != "" || response.Header.Get("X-Sakura-Retry-Count") != "" || response.Header.Get("Set-Cookie") != "" {
		t.Fatalf("untrusted response headers were forwarded: %v", response.Header)
	}
	if upstreamRequests.Load() != 1 {
		t.Fatalf("upstream request count = %d, want 1", upstreamRequests.Load())
	}
}

func TestGatewayStoresOnlyCallerTokenHashes(t *testing.T) {
	config := testConfig(nil)
	gateway, err := New(config, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	if gateway.config.ClientTokens != nil || gateway.pool.config.ClientTokens != nil {
		t.Fatal("gateway retained plaintext caller tokens")
	}
}

func TestGatewayAllowsOnlyModelsAndChatCompletions(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		writer.WriteHeader(http.StatusOK)
	})
	_, proxy := startTestGateway(t, testConfig(nil), upstream)

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantAllow  string
	}{
		{name: "responses API is not exposed", method: http.MethodPost, path: "/v1/responses", body: `{}`, wantStatus: http.StatusNotFound},
		{name: "models is read-only", method: http.MethodPost, path: "/v1/models", body: `{}`, wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodGet},
		{name: "chat completions is write-only", method: http.MethodGet, path: "/v1/chat/completions", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodPost},
		{name: "unknown v1 path is not proxied", method: http.MethodGet, path: "/v1/admin", wantStatus: http.StatusNotFound},
		{name: "models rejects request bodies", method: http.MethodGet, path: "/v1/models", body: `{}`, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.ContentLength = int64(len(test.body))
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("o", 32))
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != test.wantStatus || response.Header().Get("Allow") != test.wantAllow {
				t.Fatalf("response = %d Allow=%q, want %d Allow=%q", response.Code, response.Header().Get("Allow"), test.wantStatus, test.wantAllow)
			}
		})
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("unsupported requests reached upstream %d times", got)
	}
}

func TestKimiReasoningDefaultPreservesCallerOverrides(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"omitted", `{"model":"preview/Kimi-K2.7-Code","messages":[{"role":"user","content":"test"}]}`, `{"model":"preview/Kimi-K2.7-Code","messages":[{"role":"user","content":"test"}],"reasoning_effort":"max"}`},
		{"nested only", `{"model":"preview/Kimi-K2.7-Code","messages":[{"role":"user","content":"reasoning_effort: low"}]}`, `{"model":"preview/Kimi-K2.7-Code","messages":[{"role":"user","content":"reasoning_effort: low"}],"reasoning_effort":"max"}`},
		{"whitespace", " {\"model\":\"preview/Kimi-K2.7-Code\"} \n", `{"model":"preview/Kimi-K2.7-Code","reasoning_effort":"max"}`},
		{"explicit low", `{"model":"preview/Kimi-K2.7-Code","reasoning_effort":"low"}`, `{"model":"preview/Kimi-K2.7-Code","reasoning_effort":"low"}`},
		{"explicit null", `{"model":"preview/Kimi-K2.7-Code","reasoning_effort":null}`, `{"model":"preview/Kimi-K2.7-Code","reasoning_effort":null}`},
		{"another model", `{"model":"preview/Kimi-K2.6"}`, `{"model":"preview/Kimi-K2.6"}`},
		{"malformed", `{"model":"preview/Kimi-K2.7-Code"`, `{"model":"preview/Kimi-K2.7-Code"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil || string(body) != test.want {
					t.Errorf("upstream body = %q, error = %v; want %q", body, err, test.want)
				}
				_, _ = io.WriteString(writer, `{"ok":true}`)
			})
			server, _ := startTestGateway(t, testConfig(nil), upstream)
			response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("h", 32), strings.NewReader(test.body), nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}
}

func TestGatewayRotatesRateLimitedAccountAndPreservesCallerIdentity(t *testing.T) {
	var attempts atomic.Int32
	var accountHeaders []string
	var mutex sync.Mutex
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		accountHeaders = append(accountHeaders, request.Header.Get("Authorization"))
		mutex.Unlock()
		if request.Header.Get("Authorization") == "Bearer account-token-a" && attempts.Add(1) == 1 {
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"ok":true}`)
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("h", 32), strings.NewReader(`{"model":"test"}`), nil)
	if response.StatusCode != http.StatusOK || string(response.Body) != `{"ok":true}` {
		t.Fatalf("gateway response = %d %s", response.StatusCode, response.Body)
	}
	if response.Header.Get("X-Sakura-Retry-Count") != "1" || response.Header.Get("X-Sakura-Retry-Reason") != string(retryRateLimit) {
		t.Fatalf("retry headers = %v", response.Header)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(accountHeaders) != 2 || accountHeaders[0] != "Bearer account-token-a" || accountHeaders[1] != "Bearer account-token-b" {
		t.Fatalf("upstream token sequence = %v", accountHeaders)
	}
}

func TestGatewayAllows24ConcurrentRequestsWithOneUpstreamToken(t *testing.T) {
	const concurrentRequests = 24
	started := make(chan struct{}, concurrentRequests)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Error("upstream response does not support streaming")
			return
		}
		_, _ = io.WriteString(writer, "data: {\"choices\":[]}\n\n")
		flusher.Flush()
		started <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)
	config := testConfig(nil)
	config.UpstreamURL, _ = url.Parse(upstream.URL)
	config.AccountTokens = []string{"single-upstream-token"}
	config.MaxRequestBytes = 64 << 20
	config.MaxConcurrent = concurrentRequests
	config.MaxConcurrentPerClient = concurrentRequests
	config.UpstreamIdleTimeout = 5 * time.Second
	proxy, err := New(config, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(func() { server.Close(); proxy.Close() })

	type result struct {
		status int
		body   []byte
		err    error
	}
	responses := make(chan result, concurrentRequests)
	for range concurrentRequests {
		go func() {
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{}`))
			if err != nil {
				responses <- result{err: err}
				return
			}
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("o", 32))
			response, err := server.Client().Do(request)
			if err != nil {
				responses <- result{err: err}
				return
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			responses <- result{status: response.StatusCode, body: body, err: errors.Join(readError, closeError)}
		}()
	}

	for range concurrentRequests {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("all 24 requests did not reach the upstream with one token")
		}
	}
	releaseOnce.Do(func() { close(release) })
	for range concurrentRequests {
		result := <-responses
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.status != http.StatusOK || !strings.Contains(string(result.body), "[DONE]") {
			t.Errorf("stream did not complete: status=%d body=%s", result.status, result.body)
		}
	}
}

func TestGatewayHonorsRetryAfterOnServiceUnavailable(t *testing.T) {
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writer.Header().Set("Retry-After", "1")
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"ok":true}`)
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	started := time.Now()
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("o", 32), strings.NewReader(`{}`), nil)
	if response.StatusCode != http.StatusOK || string(response.Body) != `{"ok":true}` {
		t.Fatalf("retried response = %d %s", response.StatusCode, response.Body)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream attempts = %d, want 2", calls.Load())
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("retried before Retry-After: elapsed=%s", elapsed)
	}
}

func TestGatewayDoesNotWaitPastBudgetForHugeRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Retry-After", "9223372036")
		writer.WriteHeader(http.StatusServiceUnavailable)
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	started := time.Now()
	client := &http.Client{Timeout: time.Second}
	response := sendRequest(t, client, http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("o", 32), strings.NewReader(`{}`), nil)
	if response.StatusCode != http.StatusServiceUnavailable || attempts.Load() != 1 {
		t.Fatalf("response = %d, upstream attempts = %d; should not retry beyond budget", response.StatusCode, attempts.Load())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("retry exceeded deadline instead of returning provider error: %s", elapsed)
	}
}

func TestGatewayDowngradesReasoningBeforeStreamingRetry(t *testing.T) {
	var attempts atomic.Int32
	var retryEffort string
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, "data: {\"error\":{\"code\":\"timeout\"}}\n\n")
			return
		}
		var payload map[string]string
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode retry body: %v", err)
		}
		retryEffort = payload["reasoning_effort"]
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Error("upstream response does not support streaming")
			return
		}
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		flusher.Flush()
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("w", 32), strings.NewReader(`{"reasoning_effort":"high"}`), http.Header{openWebUIModeHeader: {"true"}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	if retryEffort != "low" {
		t.Fatalf("retry reasoning_effort = %q, want low", retryEffort)
	}
	if response.Header.Get("Content-Length") != "" {
		t.Fatalf("injected SSE event retained Content-Length %q", response.Header.Get("Content-Length"))
	}
	if !strings.Contains(string(response.Body), `"description":"Retried with low reasoning effort"`) || !strings.Contains(string(response.Body), `"content":"ok"`) {
		t.Fatalf("stream body omitted retry status or provider data: %s", response.Body)
	}
	if response.Header.Get("X-Sakura-Effective-Reasoning-Effort") != "low" {
		t.Fatalf("effective reasoning header = %q", response.Header.Get("X-Sakura-Effective-Reasoning-Effort"))
	}
}

func TestGatewayRetriesFirstSSEEventWithMetadataBeforeData(t *testing.T) {
	var attempts atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(writer, "event: error\ndata: {\"error\":{\"code\":\"timeout\"}}\n\n")
			return
		}
		_, _ = io.WriteString(writer, "event: message\ndata: {\"choices\":[]}\n\n")
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("o", 32), strings.NewReader(`{}`), nil)
	if response.StatusCode != http.StatusOK || attempts.Load() != 2 || strings.Contains(string(response.Body), "event: error") || !strings.Contains(string(response.Body), "event: message") {
		t.Fatalf("SSE error was not retried: status=%d attempts=%d body=%s", response.StatusCode, attempts.Load(), response.Body)
	}
}

func TestGatewayRetriesMultilineProviderJSONError(t *testing.T) {
	var attempts atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(writer, "{\n  \"error\": {\"code\": \"timeout\"}\n}")
			return
		}
		_, _ = io.WriteString(writer, `{"choices":[]}`)
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("o", 32), strings.NewReader(`{}`), nil)
	if response.StatusCode != http.StatusOK || attempts.Load() != 2 || string(response.Body) != `{"choices":[]}` {
		t.Fatalf("multiline error was not retried: status=%d attempts=%d body=%s", response.StatusCode, attempts.Load(), response.Body)
	}
}

func TestGatewayInspectsGzip200ErrorDespiteClientAcceptEncoding(t *testing.T) {
	var attempts atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if encoding := request.Header.Get("Accept-Encoding"); encoding != "identity" {
			t.Errorf("upstream encoding = %q; gateway must request inspectable response bytes", encoding)
		}
		writer.Header().Set("Content-Encoding", "gzip")
		writer.Header().Set("Content-Type", "application/json")
		compressor := gzip.NewWriter(writer)
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(compressor, `{"error":{"code":"timeout"}}`)
		} else {
			_, _ = io.WriteString(compressor, `{"choices":[]}`)
		}
		if err := compressor.Close(); err != nil {
			t.Errorf("close upstream gzip stream: %v", err)
		}
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("o", 32), strings.NewReader(`{}`), http.Header{"Accept-Encoding": {"gzip"}})
	if response.StatusCode != http.StatusOK || attempts.Load() != 2 || string(response.Body) != `{"choices":[]}` || response.Header.Get("Content-Encoding") != "" {
		t.Fatalf("gzip error was not retried and decoded: status=%d attempts=%d encoding=%q body=%s", response.StatusCode, attempts.Load(), response.Header.Get("Content-Encoding"), response.Body)
	}
}

func TestErrorPrefixLimitPreservesEntireStream(t *testing.T) {
	t.Parallel()
	body := []byte("data: " + strings.Repeat("x", maxErrorPrefixBytes+1024) + "\n\ndata: [DONE]\n\n")
	reader, prefix, err := readErrorPrefix(bytes.NewReader(body), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix) != maxErrorPrefixBytes {
		t.Fatalf("prefix length = %d, want %d", len(prefix), maxErrorPrefixBytes)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(prefix, rest...), body) {
		t.Fatal("bounded inspection lost or duplicated upstream bytes")
	}
}

func TestGatewayForwardsSSEHeartbeatBeforeFirstEvent(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, ": ping\n")
		writer.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		_, _ = io.WriteString(writer, "data: {\"choices\":[]}\n\n")
	}))
	t.Cleanup(upstream.Close)
	config := testConfig(nil)
	config.UpstreamURL, _ = url.Parse(upstream.URL)
	config.UpstreamIdleTimeout = 5 * time.Second
	proxy, err := New(config, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(func() { server.Close(); proxy.Close() })
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("o", 32))
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("upstream heartbeat did not reach the client before the first event: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	select {
	case <-started:
	default:
		t.Fatal("upstream heartbeat was not sent")
	}
	releaseOnce.Do(func() { close(release) })
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), ": ping\n") || !strings.Contains(string(body), `"choices"`) {
		t.Fatalf("stream lost heartbeat or completion: status=%d body=%q", response.StatusCode, body)
	}
}

func TestGatewayRetryBudgetAndNonRetryableStatus(t *testing.T) {
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"error":{"code":"invalid_api_key"}}`)
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("o", 32), strings.NewReader(`{}`), nil)
	if response.StatusCode != http.StatusUnauthorized || calls.Load() != 1 || !strings.Contains(string(response.Body), "invalid_api_key") {
		t.Fatalf("non-retryable response = %d, calls=%d body=%s", response.StatusCode, calls.Load(), response.Body)
	}
}

func TestUpstreamAttemptRetryDeadlineEndsAtFirstResponseLine(t *testing.T) {
	t.Run("timeout before first response line", func(t *testing.T) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			close(started)
			<-request.Context().Done()
			close(canceled)
		})
		_, proxy := startTestGateway(t, testConfig(nil), upstream)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		upstreamRequest, err := proxy.newUpstreamRequest(request, []byte(`{}`), "provider-token")
		if err != nil {
			t.Fatal(err)
		}
		_, err = proxy.performUpstreamAttempt(request, upstreamRequest, time.Now().Add(50*time.Millisecond))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt error = %v, want retry-budget deadline", err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("upstream attempt did not start")
		}
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("retry-budget deadline did not cancel upstream work")
		}
	})

	t.Run("stream continues after first response line", func(t *testing.T) {
		prefixSent := make(chan struct{})
		finish := make(chan struct{})
		canceled := make(chan struct{})
		upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, "data: {\"choices\":[]}\n\n")
			writer.(http.Flusher).Flush()
			close(prefixSent)
			select {
			case <-finish:
				_, _ = io.WriteString(writer, "data: [DONE]\n\n")
			case <-request.Context().Done():
				close(canceled)
			}
		})
		_, proxy := startTestGateway(t, testConfig(nil), upstream)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		upstreamRequest, err := proxy.newUpstreamRequest(request, []byte(`{}`), "provider-token")
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := proxy.performUpstreamAttempt(request, upstreamRequest, time.Now().Add(50*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		defer attempt.cancel()
		select {
		case <-prefixSent:
		case <-time.After(time.Second):
			t.Fatal("upstream response prefix was not sent")
		}
		time.Sleep(75 * time.Millisecond)
		select {
		case <-canceled:
			t.Fatal("retry-budget deadline canceled a stream after its first response line")
		default:
		}
		close(finish)
		body, err := io.ReadAll(attempt.reader)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "[DONE]") {
			t.Fatalf("stream body = %q, want completion after retry deadline", body)
		}
		if err := attempt.response.Body.Close(); err != nil {
			t.Errorf("close streamed response: %v", err)
		}
	})

	t.Run("gzip header blocked until retry deadline", func(t *testing.T) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Encoding", "gzip")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			close(started)
			<-request.Context().Done()
			close(canceled)
		})
		_, proxy := startTestGateway(t, testConfig(nil), upstream)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		upstreamRequest, err := proxy.newUpstreamRequest(request, []byte(`{}`), "provider-token")
		if err != nil {
			t.Fatal(err)
		}
		_, err = proxy.performUpstreamAttempt(request, upstreamRequest, time.Now().Add(50*time.Millisecond))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gzip header failure = %v; want retry-budget deadline", err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("upstream did not send gzip response headers")
		}
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("gzip header wait did not cancel upstream work")
		}
	})
}

func TestGatewayStopsUpstreamAndReleasesLeaseWhenClientCancels(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Has("slow") {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			flusher, ok := writer.(http.Flusher)
			if !ok {
				t.Error("upstream response does not support streaming")
				return
			}
			_, _ = io.WriteString(writer, "data: {\"choices\":[]}\n\n")
			flusher.Flush()
			close(started)
			<-request.Context().Done()
			close(canceled)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"ok":true}`)
	})
	config := testConfig(nil)
	config.AccountTokens = []string{"one-upstream-token"}
	config.MaxConcurrent = 1
	config.MaxConcurrentPerClient = 1
	server, _ := startTestGateway(t, config, upstream)

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions?slow=1", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("o", 32))
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		_ = response.Body.Close()
		t.Fatal("stream did not reach upstream")
	}
	cancel()
	if err := response.Body.Close(); err != nil {
		t.Errorf("close canceled response: %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not stop the upstream request")
	}

	second := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("h", 32), strings.NewReader(`{}`), nil)
	if second.StatusCode != http.StatusOK || string(second.Body) != `{"ok":true}` {
		t.Fatalf("token lease was not released after cancellation: %d %s", second.StatusCode, second.Body)
	}
}

func TestRetryBudgetRejectsUnfittingAttempt(t *testing.T) {
	config := testConfig(nil)
	config.MaxRetries = 1
	config.UpstreamIdleTimeout = time.Second
	config.RetryBudget = 3200 * time.Second
	proxy, err := New(config, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if proxy.canRetry(0, 0, time.Now().Add(time.Second)) {
		t.Fatal("retry was allowed without enough budget for an upstream attempt")
	}
	if proxy.canRetry(1, 0, time.Now().Add(time.Minute)) {
		t.Fatal("retry was allowed after reaching the retry limit")
	}
	proxy.Close()
}

func TestGatewayBoundsRequestBodyAndConcurrency(t *testing.T) {
	t.Run("body limit", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			writer.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)
		config := testConfig(nil)
		config.UpstreamURL, _ = url.Parse(upstream.URL)
		config.MaxRequestBytes = 4
		proxy, err := New(config, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(proxy)
		t.Cleanup(func() { server.Close(); proxy.Close() })
		response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("w", 32), strings.NewReader("12345"), nil)
		if response.StatusCode != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
			t.Fatalf("oversized request = %d, upstream calls=%d", response.StatusCode, calls.Load())
		}
	})

	t.Run("concurrency cap", func(t *testing.T) {
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			started <- struct{}{}
			select {
			case <-release:
			case <-request.Context().Done():
			}
			writer.WriteHeader(http.StatusOK)
		})
		config := testConfig(nil)
		config.MaxConcurrent = 1
		config.MaxConcurrentPerClient = 1
		server, _ := startTestGateway(t, config, upstream)
		firstDone := make(chan error, 1)
		go func() {
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{}`))
			if err != nil {
				firstDone <- err
				return
			}
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("w", 32))
			response, err := server.Client().Do(request)
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			firstDone <- err
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("first request did not reach upstream")
		}
		response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("o", 32), strings.NewReader(`{}`), nil)
		if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" {
			t.Fatalf("busy response = %d, Retry-After=%q", response.StatusCode, response.Header.Get("Retry-After"))
		}
		close(release)
		select {
		case err := <-firstDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("first request did not finish")
		}
	})

	t.Run("per-client fairness", func(t *testing.T) {
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			started <- struct{}{}
			select {
			case <-release:
			case <-request.Context().Done():
			}
			writer.WriteHeader(http.StatusOK)
		})
		config := testConfig(nil)
		config.MaxConcurrent = 2
		config.MaxConcurrentPerClient = 1
		server, _ := startTestGateway(t, config, upstream)
		requestDone := func(clientToken string) <-chan error {
			done := make(chan error, 1)
			go func() {
				request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{}`))
				if err != nil {
					done <- err
					return
				}
				request.Header.Set("Authorization", "Bearer "+clientToken)
				response, err := server.Client().Do(request)
				if err == nil {
					_, err = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
					if response.StatusCode != http.StatusOK {
						err = fmt.Errorf("unexpected status %d", response.StatusCode)
					}
				}
				done <- err
			}()
			return done
		}

		firstDone := requestDone(strings.Repeat("w", 32))
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("first client did not reach upstream")
		}
		busy := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("w", 32), strings.NewReader(`{}`), nil)
		if busy.StatusCode != http.StatusServiceUnavailable || busy.Header.Get("Retry-After") != "1" {
			t.Fatalf("same-client limit response = %d, Retry-After=%q", busy.StatusCode, busy.Header.Get("Retry-After"))
		}
		secondDone := requestDone(strings.Repeat("h", 32))
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("a separate client was blocked by the first client's limit")
		}
		releaseOnce.Do(func() { close(release) })
		for _, done := range []<-chan error{firstDone, secondDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("concurrent request did not finish")
			}
		}
	})
}

func TestGatewayDoesNotLogCallerOrUpstreamSecrets(t *testing.T) {
	logs := &lockedBuffer{written: make(chan struct{})}
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"ok":true}`)
	})
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	config := testConfig(nil)
	config.UpstreamURL, _ = url.Parse(upstreamServer.URL)
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	proxy, err := New(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(func() { server.Close(); proxy.Close() })
	callerToken := config.ClientTokens["opencode"]
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", callerToken, strings.NewReader(`{"prompt":"do not log this"}`), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	select {
	case <-logs.written:
	case <-time.After(time.Second):
		t.Fatal("request log was not written")
	}
	for _, secret := range []string{callerToken, "account-token-a", "do not log this"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain sensitive content %q", secret)
		}
	}
	if !strings.Contains(logs.String(), `"client":"opencode"`) {
		t.Fatalf("logs do not identify the caller: %s", logs.String())
	}
}

type lockedBuffer struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	written chan struct{}
	once    sync.Once
}

func (buffer *lockedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	count, err := buffer.buffer.Write(value)
	buffer.once.Do(func() { close(buffer.written) })
	return count, err
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
