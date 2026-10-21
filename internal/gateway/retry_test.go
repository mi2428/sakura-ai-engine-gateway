package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestClassifyResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		body   string
		want   retryReason
	}{
		{name: "rate limit status", status: http.StatusTooManyRequests, want: retryRateLimit},
		{name: "timeout status", status: http.StatusGatewayTimeout, want: retryTimeout},
		{name: "server status", status: http.StatusServiceUnavailable, want: retryServerError},
		{name: "provider timeout event", status: http.StatusOK, body: `data: {"error":{"code":"timeout"}}`, want: retryTimeout},
		{name: "provider rate limit event", status: http.StatusOK, body: `data: {"error":{"code":"rate_limit_exceeded"}}`, want: retryRateLimit},
		{name: "provider server error", status: http.StatusOK, body: `{"error":{"type":"internal_server_error"}}`, want: retryServerError},
		{name: "retryable plain message", status: http.StatusOK, body: "Internal server error.", want: retryServerError},
		{name: "client error", status: http.StatusUnauthorized, body: `{"error":{"code":"invalid_api_key"}}`},
		{name: "successful response", status: http.StatusOK, body: `{"choices":[]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyResponse(test.status, []byte(test.body)); got != test.want {
				t.Fatalf("classifyResponse() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRetryAfterAndBackoff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	if got := retryAfter("2.5", now); got != 2500*time.Millisecond {
		t.Fatalf("numeric Retry-After = %s, want 2.5s", got)
	}
	if got := retryAfter(now.Add(3*time.Second).Format(http.TimeFormat), now); got != 3*time.Second {
		t.Fatalf("date Retry-After = %s, want 3s", got)
	}
	if got := retryAfter("invalid", now); got != 0 {
		t.Fatalf("invalid Retry-After = %s, want 0", got)
	}

	config := testConfig(nil)
	config.BaseBackoff = 10 * time.Second
	config.MaxBackoff = 120 * time.Second
	config.Jitter = 0
	got := []time.Duration{
		retryDelay(config, 0, 0),
		retryDelay(config, 1, 0),
		retryDelay(config, 2, 0),
		retryDelay(config, 3, 0),
		retryDelay(config, 4, 0),
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 120 * time.Second}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("retry delay %d = %s, want %s", index, got[index], want[index])
		}
	}
	if got := retryDelay(config, 0, 1000*time.Second); got != 1000*time.Second {
		t.Fatalf("provider Retry-After = %s, want 1000s", got)
	}
}

func TestTimeoutRetryBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantEffort string
		wantChange bool
	}{
		{name: "high effort", body: `{"model":"x","reasoning_effort":"high"}`, wantEffort: "low", wantChange: true},
		{name: "medium effort", body: `{"reasoning_effort":"medium"}`, wantEffort: "low", wantChange: true},
		{name: "max effort remains", body: `{"reasoning_effort":"max"}`, wantEffort: "max"},
		{name: "unspecified remains", body: `{"model":"x"}`},
		{name: "invalid JSON remains", body: "not-json"},
		{name: "non-object remains", body: `[]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, changed := timeoutRetryBody([]byte(test.body))
			if changed != test.wantChange {
				t.Fatalf("changed = %t, want %t", changed, test.wantChange)
			}
			if test.wantChange {
				var payload map[string]string
				if err := json.Unmarshal(got, &payload); err != nil {
					t.Fatal(err)
				}
				if payload["reasoning_effort"] != test.wantEffort {
					t.Fatalf("reasoning_effort = %q, want %q", payload["reasoning_effort"], test.wantEffort)
				}
			} else if string(got) != test.body {
				t.Fatalf("unchanged body = %q, want %q", got, test.body)
			}
		})
	}
}

func TestTimeoutRetryBodyRewritesOnlyTopLevelEffortInPlace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "preserve payload bytes and nested fields",
			body: `{"messages":[{"content":"<&>"}],"reasoning_effort":"high","metadata":{"reasoning_effort":"medium"}}`,
			want: `{"messages":[{"content":"<&>"}],"reasoning_effort":"low","metadata":{"reasoning_effort":"medium"}}`,
		},
		{
			name: "decode escaped field name without normalizing it",
			body: `{"reasoning\u005feffort" : "medium"}`,
			want: `{"reasoning\u005feffort" : "low"}`,
		},
		{
			name: "last duplicate field wins",
			body: `{"reasoning_effort":"high","reasoning_effort":"medium"}`,
			want: `{"reasoning_effort":"high","reasoning_effort":"low"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := []byte(test.body)
			original := &body[0]
			got, changed := timeoutRetryBody(body)
			if !changed {
				t.Fatal("timeoutRetryBody() did not lower reasoning_effort")
			}
			if &got[0] != original {
				t.Fatal("timeoutRetryBody() allocated a replacement request body")
			}
			if string(got) != test.want {
				t.Fatalf("rewritten body = %s, want %s", got, test.want)
			}
		})
	}
}

func TestTimeoutRetryBodyRejectsMalformedTrailingJSONWithoutMutation(t *testing.T) {
	t.Parallel()
	body := []byte(`{"reasoning_effort":"high"} {}`)
	original := string(body)
	got, changed := timeoutRetryBody(body)
	if changed || string(got) != original {
		t.Fatalf("malformed request changed: changed=%t body=%s", changed, got)
	}
}

func TestTimeoutRetryBodyDoesNotExpandMaximumRequest(t *testing.T) {
	t.Parallel()
	const limit = 64 << 20
	// A large unknown key was enough to make the old JSON marshal expand '<'
	// into six bytes each, exceeding the container limit under concurrency.
	key := bytes.Repeat([]byte{'<'}, limit-128)
	body := make([]byte, 0, limit)
	body = append(body, `{"`...)
	body = append(body, key...)
	body = append(body, `":1,"reasoning_effort":"high"}`...)
	if len(body) > limit {
		t.Fatalf("test request exceeds configured limit: %d", len(body))
	}
	original := &body[0]
	updated, changed := timeoutRetryBody(body)
	if !changed || &updated[0] != original || len(updated) > len(body) {
		t.Fatalf("maximum-sized rewrite allocated or expanded: changed=%t before=%d after=%d", changed, len(body), len(updated))
	}
	if !bytes.Equal(updated[:len(key)+2], body[:len(key)+2]) || !bytes.HasSuffix(updated, []byte(`"reasoning_effort":"low"}`)) {
		t.Fatal("rewrite modified unrelated request content")
	}
}
