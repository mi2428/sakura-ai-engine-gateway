package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testClientToken = "client-token-for-searchtool-tests-0001"

func newTestRouter(t *testing.T) *Router {
	t.Helper()
	keys := make(map[string]string, len(providerSpecs))
	for _, spec := range providerSpecs {
		keys[spec.keyName] = "test-" + spec.name + "-key"
	}
	router, err := New(testClientToken, keys)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestNewRequiresEveryCredential(t *testing.T) {
	keys := make(map[string]string, len(providerSpecs))
	for _, spec := range providerSpecs {
		keys[spec.keyName] = "test-key"
	}
	delete(keys, EnvExaAPIKey)
	if _, err := New(testClientToken, keys); err == nil {
		t.Fatal("New() accepted an incomplete provider pool")
	}
}

func TestServeHTTPHealthAuthenticationAndValidation(t *testing.T) {
	router := newTestRouter(t)
	for _, test := range []struct {
		name   string
		method string
		path   string
		auth   string
		body   string
		want   int
	}{
		{name: "health", method: http.MethodGet, path: "/health", want: http.StatusOK},
		{name: "unauthorized", method: http.MethodPost, path: "/search", body: `{"query":"test"}`, want: http.StatusUnauthorized},
		{name: "invalid query", method: http.MethodPost, path: "/search", auth: "Bearer " + testClientToken, body: `{"query":" "}`, want: http.StatusBadRequest},
		{name: "wrong method", method: http.MethodGet, path: "/search", want: http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.auth != "" {
				request.Header.Set("Authorization", test.auth)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestServeHTTPRejectsWhenAtCapacity(t *testing.T) {
	router := newTestRouter(t)
	for range maximumConcurrent {
		router.permits <- struct{}{}
	}
	request := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"query":"test"}`))
	request.Header.Set("Authorization", "Bearer "+testClientToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("status = %d, Retry-After = %q", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestServeHTTPReturnsPayloadTooLarge(t *testing.T) {
	router := newTestRouter(t)
	for _, body := range []string{
		`{"query":"` + strings.Repeat("x", maximumRequestBytes) + `"}`,
		`{"query":"ok"}` + strings.Repeat(" ", maximumRequestBytes),
	} {
		request := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testClientToken)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d for %d-byte body, want 413", response.Code, len(body))
		}
	}
}

func TestProviderResponsesNormalize(t *testing.T) {
	tests := []struct {
		name     string
		response string
		snippet  string
	}{
		{name: "brave", response: `{"web":{"results":[{"title":"Brave","url":"https://example.com/brave","description":"brave snippet"}]}}`, snippet: "brave snippet"},
		{name: "tavily", response: `{"results":[{"title":"Tavily","url":"https://example.com/tavily","content":"tavily snippet"}]}`, snippet: "tavily snippet"},
		{name: "exa", response: `{"results":[{"title":"Exa","url":"https://example.com/exa","highlights":["exa","snippet"]}]}`, snippet: "exa snippet"},
		{name: "firecrawl", response: `{"data":{"web":[{"title":"Firecrawl","url":"https://example.com/firecrawl","description":"firecrawl snippet"}]}}`, snippet: "firecrawl snippet"},
		{name: "parallel", response: `{"results":[{"title":"Parallel","url":"https://example.com/parallel","excerpts":["parallel","snippet"]}]}`, snippet: "parallel snippet"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.response))
			}))
			defer server.Close()
			router := newTestRouter(t)
			router.providers[index].endpoint = server.URL
			router.next = index
			results, err := router.search(context.Background(), "query", 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Snippet != test.snippet {
				t.Fatalf("results = %#v", results)
			}
		})
	}
}

func TestExaSearchRequestsHighlights(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := jsonDecode(request, &payload); err != nil {
			t.Errorf("decode Exa request: %v", err)
		}
		contents, ok := payload["contents"].(map[string]any)
		highlights, highlightsOK := contents["highlights"].(map[string]any)
		if payload["query"] != "test query" || payload["type"] != "lite" || payload["numResults"] != float64(3) || request.Header.Get("Authorization") != "Bearer test-exa-key" || !ok || !highlightsOK || highlights["query"] != "test query" {
			t.Errorf("unexpected Exa request: %#v", payload)
		}
		_, _ = writer.Write([]byte(`{"results":[{"title":"Exa","url":"https://example.com","highlights":["found"]}]}`))
	}))
	defer server.Close()
	router := newTestRouter(t)
	router.providers[2].endpoint = server.URL
	router.next = 2
	results, err := router.search(context.Background(), "test query", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Snippet != "found" {
		t.Fatalf("results = %#v", results)
	}
}

func TestDecodeResultsRejectsMissingOrNullRows(t *testing.T) {
	tests := []struct {
		name     string
		response string
	}{
		{name: "brave missing rows", response: `{"web":{}}`},
		{name: "brave null rows", response: `{"web":{"results":null}}`},
		{name: "tavily missing rows", response: `{}`},
		{name: "exa null rows", response: `{"results":null}`},
		{name: "firecrawl missing rows", response: `{"data":{}}`},
		{name: "firecrawl null rows", response: `{"data":{"web":null}}`},
		{name: "parallel null item", response: `{"results":[null]}`},
	}
	for _, test := range tests {
		providerName, _, _ := strings.Cut(test.name, " ")
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeResults(providerName, []byte(test.response), 5); err == nil {
				t.Fatal("decodeResults() accepted a malformed result array")
			}
		})
	}
}

func TestDecodeResultsAcceptsEmptyRows(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
	}{
		{name: "brave", response: `{"web":{"results":[]}}`},
		{name: "tavily", response: `{"results":[]}`},
		{name: "exa", response: `{"results":[]}`},
		{name: "firecrawl", response: `{"data":{"web":[]}}`},
		{name: "parallel", response: `{"results":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			results, err := decodeResults(test.name, []byte(test.response), 5)
			if err != nil || len(results) != 0 {
				t.Fatalf("results = %#v, err = %v", results, err)
			}
		})
	}
}

func TestRoundRobinAndRateLimitFallback(t *testing.T) {
	var braveCalls atomic.Int32
	var tavilyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/brave":
			braveCalls.Add(1)
			if request.Method != http.MethodGet || request.URL.Query().Get("q") != "test query" || request.Header.Get("X-Subscription-Token") != "test-brave-key" {
				t.Errorf("unexpected Brave request: %s %s", request.Method, request.URL)
			}
			writer.Header().Set("Retry-After", "30")
			writer.WriteHeader(http.StatusTooManyRequests)
		case "/tavily":
			tavilyCalls.Add(1)
			var payload map[string]any
			if err := jsonDecode(request, &payload); err != nil {
				t.Errorf("decode Tavily request: %v", err)
			}
			if payload["query"] != "test query" || payload["max_results"] != float64(3) || request.Header.Get("Authorization") != "Bearer test-tavily-key" {
				t.Errorf("unexpected Tavily request: %#v", payload)
			}
			_, _ = writer.Write([]byte(`{"results":[{"title":"Tavily","url":"https://example.com","content":"found"}]}`))
		default:
			t.Errorf("unexpected provider path: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	router := newTestRouter(t)
	router.providers[0].endpoint = server.URL + "/brave"
	router.providers[1].endpoint = server.URL + "/tavily"
	results, err := router.search(context.Background(), "test query", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "Tavily" || braveCalls.Load() != 1 || tavilyCalls.Load() != 1 {
		t.Fatalf("results = %#v, Brave calls = %d, Tavily calls = %d", results, braveCalls.Load(), tavilyCalls.Load())
	}
	router.next = 0
	if _, err := router.search(context.Background(), "test query", 3); err != nil {
		t.Fatal(err)
	}
	if braveCalls.Load() != 1 || tavilyCalls.Load() != 2 {
		t.Fatalf("cooldown calls = (Brave %d, Tavily %d)", braveCalls.Load(), tavilyCalls.Load())
	}
}

func TestSuccessfulSearchesRotateProviders(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls = append(calls, strings.TrimPrefix(request.URL.Path, "/"))
		response := map[string]string{
			"brave":     `{"web":{"results":[{"title":"Brave","url":"https://example.com","description":"found"}]}}`,
			"tavily":    `{"results":[{"title":"Tavily","url":"https://example.com","content":"found"}]}`,
			"exa":       `{"results":[{"title":"Exa","url":"https://example.com","highlights":["found"]}]}`,
			"firecrawl": `{"data":{"web":[{"title":"Firecrawl","url":"https://example.com","description":"found"}]}}`,
			"parallel":  `{"results":[{"title":"Parallel","url":"https://example.com","excerpts":["found"]}]}`,
		}[strings.TrimPrefix(request.URL.Path, "/")]
		_, _ = writer.Write([]byte(response))
	}))
	defer server.Close()
	router := newTestRouter(t)
	for index := range router.providers {
		router.providers[index].endpoint = server.URL + "/" + router.providers[index].name
	}
	for range 2 {
		if _, err := router.search(context.Background(), "query", 5); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 2 || calls[0] != "brave" || calls[1] != "tavily" {
		t.Fatalf("provider calls = %v, want [brave tavily]", calls)
	}
}

func TestSearchFailsOverOnServerErrorsAndTimeouts(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		serverWait time.Duration
		flushFirst bool
	}{
		{name: "server error", status: http.StatusBadGateway},
		{name: "provider timeout status", status: http.StatusRequestTimeout},
		{name: "timeout", serverWait: 350 * time.Millisecond},
		{name: "response body timeout", serverWait: 350 * time.Millisecond, flushFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var tavilyCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/brave" {
					if test.flushFirst {
						writer.WriteHeader(http.StatusOK)
						writer.(http.Flusher).Flush()
						time.Sleep(test.serverWait)
						_, _ = writer.Write([]byte(`{"web":{"results":[]}}`))
						return
					}
					if test.serverWait > 0 {
						time.Sleep(test.serverWait)
					}
					if test.status != 0 {
						writer.WriteHeader(test.status)
						return
					}
					return
				}
				tavilyCalls.Add(1)
				_, _ = writer.Write([]byte(`{"results":[{"title":"Tavily","url":"https://example.com","content":"found"}]}`))
			}))
			defer server.Close()
			router := newTestRouter(t)
			router.client = &http.Client{Timeout: 200 * time.Millisecond, CheckRedirect: rejectRedirects}
			router.providers[0].endpoint = server.URL + "/brave"
			router.providers[1].endpoint = server.URL + "/tavily"
			results, err := router.search(context.Background(), "query", 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || tavilyCalls.Load() != 1 {
				t.Fatalf("results = %#v, Tavily calls = %d", results, tavilyCalls.Load())
			}
		})
	}
}

func TestLongQueriesSkipFirecrawl(t *testing.T) {
	for _, test := range []struct {
		name        string
		query       string
		firecrawlOK bool
	}{
		{name: "500 ASCII characters", query: strings.Repeat("x", 500), firecrawlOK: true},
		{name: "501 ASCII characters", query: strings.Repeat("x", 501)},
		{name: "501 multibyte characters", query: strings.Repeat("あ", 501)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var firecrawlCalls atomic.Int32
			var parallelCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/firecrawl":
					firecrawlCalls.Add(1)
					_, _ = writer.Write([]byte(`{"data":{"web":[{"title":"Firecrawl","url":"https://example.com","description":"found"}]}}`))
				case "/parallel":
					parallelCalls.Add(1)
					_, _ = writer.Write([]byte(`{"results":[{"title":"Parallel","url":"https://example.com","excerpts":["found"]}]}`))
				default:
					t.Errorf("unexpected provider request: %s", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			router := newTestRouter(t)
			router.providers[3].endpoint = server.URL + "/firecrawl"
			router.providers[4].endpoint = server.URL + "/parallel"
			router.next = 3
			results, err := router.search(context.Background(), test.query, 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || (firecrawlCalls.Load() == 1) != test.firecrawlOK || (parallelCalls.Load() == 1) == test.firecrawlOK {
				t.Fatalf("results = %#v, Firecrawl calls = %d, Parallel calls = %d", results, firecrawlCalls.Load(), parallelCalls.Load())
			}
		})
	}
}

func TestSearchDoesNotFailOverOnAuthenticationErrors(t *testing.T) {
	var tavilyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/brave" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		tavilyCalls.Add(1)
	}))
	defer server.Close()
	router := newTestRouter(t)
	router.providers[0].endpoint = server.URL + "/brave"
	router.providers[1].endpoint = server.URL + "/tavily"
	if _, err := router.search(context.Background(), "query", 5); err == nil {
		t.Fatal("search() succeeded after an upstream authentication error")
	}
	if tavilyCalls.Load() != 0 {
		t.Fatalf("Tavily calls = %d, want no fallback", tavilyCalls.Load())
	}
}

func TestAuthenticationDoesNotAcceptWrongToken(t *testing.T) {
	router := newTestRouter(t)
	if router.authenticate("Bearer " + strings.Repeat("x", len(testClientToken))) {
		t.Fatal("authenticate accepted a different token")
	}
	if router.authenticate("Bearer " + testClientToken + " ") {
		t.Fatal("authenticate accepted whitespace in a token")
	}
}

func jsonDecode(request *http.Request, target any) error {
	defer func() { _ = request.Body.Close() }()
	return json.NewDecoder(request.Body).Decode(target)
}
