package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGatewayExportsUnauthenticatedUsageByAccountSlot(t *testing.T) {
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"resolved-model","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
	})
	server, proxy := startTestGateway(t, testConfig(nil), upstream)
	for range 2 {
		response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("h", 32), strings.NewReader(`{"model":"preview/Kimi-K2.7-Code"}`), nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("completion status = %d, body = %s", response.StatusCode, response.Body)
		}
	}

	response := sendRequest(t, server.Client(), http.MethodGet, server.URL+"/metrics", "", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics response = %d, content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	for _, sample := range []string{
		`sakura_ai_engine_gateway_chat_completion_requests_total{model="preview/Kimi-K2.7-Code"} 2`,
		`sakura_ai_engine_gateway_upstream_attempts_total{account_slot="1",model="preview/Kimi-K2.7-Code",status_code="200"} 1`,
		`sakura_ai_engine_gateway_upstream_attempts_total{account_slot="2",model="preview/Kimi-K2.7-Code",status_code="200"} 1`,
		`sakura_ai_engine_gateway_usage_tokens_total{account_slot="1",model="preview/Kimi-K2.7-Code",token_type="input"} 12`,
		`sakura_ai_engine_gateway_usage_tokens_total{account_slot="1",model="preview/Kimi-K2.7-Code",token_type="output"} 3`,
		`sakura_ai_engine_gateway_usage_tokens_total{account_slot="2",model="preview/Kimi-K2.7-Code",token_type="total"} 15`,
		`sakura_ai_engine_gateway_usage_responses_total{account_slot="1",model="preview/Kimi-K2.7-Code"} 1`,
		`sakura_ai_engine_gateway_in_flight_requests 0`,
	} {
		if !strings.Contains(string(response.Body), sample) {
			t.Errorf("metrics omitted %q:\n%s", sample, response.Body)
		}
	}
	for _, secret := range []string{"account-token-a", "account-token-b", strings.Repeat("h", 32), "hermes"} {
		if strings.Contains(string(response.Body), secret) {
			t.Errorf("metrics exposed a credential: %q", secret)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}

	request := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	result := httptest.NewRecorder()
	proxy.ServeHTTP(result, request)
	if result.Code != http.StatusMethodNotAllowed || result.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST /metrics = %d Allow=%q", result.Code, result.Header().Get("Allow"))
	}
}

func TestGatewayMetricsExposeRateLimitRetriesAndCooldown(t *testing.T) {
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer account-token-a" {
			writer.Header().Set("Retry-After", "30")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("h", 32), strings.NewReader(`{"model":"chat-model"}`), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("completion status = %d, body = %s", response.StatusCode, response.Body)
	}

	response = sendRequest(t, server.Client(), http.MethodGet, server.URL+"/metrics", "", nil, nil)
	body := string(response.Body)
	for _, sample := range []string{
		`sakura_ai_engine_gateway_chat_completion_requests_total{model="chat-model"} 1`,
		`sakura_ai_engine_gateway_upstream_attempts_total{account_slot="1",model="chat-model",status_code="429"} 1`,
		`sakura_ai_engine_gateway_upstream_attempts_total{account_slot="2",model="chat-model",status_code="200"} 1`,
		`sakura_ai_engine_gateway_upstream_retries_scheduled_total{account_slot="1",model="chat-model",reason="rate_limit"} 1`,
		`sakura_ai_engine_gateway_account_rate_limit_streak{account_slot="1"} 1`,
	} {
		if !strings.Contains(body, sample) {
			t.Errorf("metrics omitted %q:\n%s", sample, body)
		}
	}
	if !strings.Contains(body, `sakura_ai_engine_gateway_account_cooldown_seconds{account_slot="1"} 29.`) {
		t.Errorf("metrics omitted active cooldown:\n%s", body)
	}
}

func TestMetricsModelLabelsAreBoundedAndSafe(t *testing.T) {
	metrics := newGatewayMetrics()
	if got := metrics.model("invalid\nmodel"); got != unknownModel {
		t.Fatalf("model with a control character = %q, want %q", got, unknownModel)
	}
	if got, want := label("model", `quote"and\slash`), `model="quote\"and\\slash"`; got != want {
		t.Fatalf("escaped model label = %q, want %q", got, want)
	}
	for index := range maxTrackedModels {
		metrics.model("model-" + strconv.Itoa(index))
	}
	if got := metrics.model("overflow-model"); got != otherModel {
		t.Fatalf("model beyond the cardinality limit = %q, want %q", got, otherModel)
	}
}

func TestGatewaySuppressesCredentialValuesFromModelLabels(t *testing.T) {
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"choices":[]}`)
	})
	server, gateway := startTestGateway(t, testConfig(nil), upstream)
	clientToken := strings.Repeat("h", 32)
	clientTokenHash := sha256.Sum256([]byte(clientToken))
	accountTokenHash := sha256.Sum256([]byte("account-token-a"))
	for _, credential := range []string{
		"account-token-a",
		hex.EncodeToString(accountTokenHash[:]),
		"hermes",
		clientToken,
		hex.EncodeToString(clientTokenHash[:]),
	} {
		if got := gateway.metricModel(credential); got != unknownModel {
			t.Errorf("credential-like model label = %q, want %q", got, unknownModel)
		}
	}
	if got := gateway.metricModel("preview/Kimi-K2.7-Code"); got != "preview/Kimi-K2.7-Code" {
		t.Fatalf("model label = %q", got)
	}
	response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", clientToken, strings.NewReader(`{"model":"account-token-a"}`), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("completion status = %d, body = %s", response.StatusCode, response.Body)
	}
	response = sendRequest(t, server.Client(), http.MethodGet, server.URL+"/metrics", "", nil, nil)
	if !strings.Contains(string(response.Body), `sakura_ai_engine_gateway_chat_completion_requests_total{model="unknown"} 1`) || strings.Contains(string(response.Body), "account-token-a") {
		t.Fatalf("credential-like model was not suppressed from metrics:\n%s", response.Body)
	}
}

func TestGatewayReportsPartialAndMissingUsageWithoutInventingCounts(t *testing.T) {
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(writer, `{"choices":[],"usage":{"total_tokens":3}}`)
			return
		}
		_, _ = io.WriteString(writer, `{"choices":[],"usage":{}}`)
	})
	server, _ := startTestGateway(t, testConfig(nil), upstream)
	for range 2 {
		response := sendRequest(t, server.Client(), http.MethodPost, server.URL+"/v1/chat/completions", strings.Repeat("h", 32), strings.NewReader(`{"model":"partial-model"}`), nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("completion status = %d, body = %s", response.StatusCode, response.Body)
		}
	}
	response := sendRequest(t, server.Client(), http.MethodGet, server.URL+"/metrics", "", nil, nil)
	body := string(response.Body)
	for _, sample := range []string{
		`sakura_ai_engine_gateway_usage_tokens_total{account_slot="1",model="partial-model",token_type="total"} 3`,
		`sakura_ai_engine_gateway_usage_responses_total{account_slot="1",model="partial-model"} 1`,
		`sakura_ai_engine_gateway_usage_incomplete_total{account_slot="1",model="partial-model"} 1`,
		`sakura_ai_engine_gateway_usage_incomplete_total{account_slot="2",model="partial-model"} 1`,
	} {
		if !strings.Contains(body, sample) {
			t.Errorf("metrics omitted %q:\n%s", sample, body)
		}
	}
	for _, invented := range []string{
		`sakura_ai_engine_gateway_usage_tokens_total{account_slot="1",model="partial-model",token_type="input"}`,
		`sakura_ai_engine_gateway_usage_tokens_total{account_slot="1",model="partial-model",token_type="output"}`,
		`sakura_ai_engine_gateway_usage_tokens_total{account_slot="2",model="partial-model"}`,
	} {
		if strings.Contains(body, invented) {
			t.Errorf("metrics invented an unreported count: %s", invented)
		}
	}
}

func TestUsageParserDistinguishesMissingAndExplicitZeroCounts(t *testing.T) {
	if usage := usageFromValue([]byte(`{}`)); usage.found {
		t.Fatalf("empty usage object was treated as observed usage: %+v", usage)
	}
	usage := usageFromValue([]byte(`{"prompt_tokens":0,"total_tokens":0}`))
	if !usage.found || !usage.inputKnown || usage.outputKnown || !usage.totalKnown || usage.inputTokens != 0 || usage.totalTokens != 0 {
		t.Fatalf("explicit partial zero usage = %+v", usage)
	}
}

func TestResponseUsageObserverHandlesJSONAndSSE(t *testing.T) {
	t.Run("large JSON response", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"content":"\"usage\": {\"prompt_tokens\":999} ` + strings.Repeat("x", maxUsageJSONBytes/8) + `"}}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
		observer := newResponseUsageObserver(false)
		for start := 0; start < len(body); start += 7919 {
			observer.observe(body[start:min(start+7919, len(body))])
		}
		usage := observer.finish()
		if !usage.found || usage.inputTokens != 12 || usage.outputTokens != 3 || usage.totalTokens != 15 {
			t.Fatalf("observed usage = %+v", usage)
		}
	})

	t.Run("oversized JSON response is bounded", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"content":"` + strings.Repeat("x", maxUsageJSONBytes) + `"}}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
		observer := newResponseUsageObserver(false)
		for start := 0; start < len(body); start += 32 << 10 {
			observer.observe(body[start:min(start+(32<<10), len(body))])
		}
		if usage := observer.finish(); usage.found {
			t.Fatalf("oversized response unexpectedly yielded usage: %+v", usage)
		}
	})

	t.Run("streamed usage event", func(t *testing.T) {
		body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
		observer := newResponseUsageObserver(true)
		for start := 0; start < len(body); start += 3 {
			observer.observe(body[start:min(start+3, len(body))])
		}
		usage := observer.finish()
		if !usage.found || usage.inputTokens != 5 || usage.outputTokens != 7 || usage.totalTokens != 12 {
			t.Fatalf("observed usage = %+v", usage)
		}
	})
}
