package gateway

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

type retryReason string

const (
	retryRateLimit   retryReason = "rate_limit"
	retryTimeout     retryReason = "timeout"
	retryServerError retryReason = "server_error"
	retryUpstream    retryReason = "upstream_error"
)

var retryableErrorCodes = map[string]struct{}{
	"internal_error": {}, "internal_server_error": {}, "overloaded_error": {},
	"rate_limit_exceeded": {}, "server_error": {}, "timeout": {},
}

var retryableErrorMessages = map[string]struct{}{
	"internal server error": {}, "request timed out": {}, "server error": {}, "upstream timeout": {},
}

func classifyResponse(status int, prefix []byte) retryReason {
	switch {
	case status == http.StatusTooManyRequests:
		return retryRateLimit
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return retryTimeout
	case status == http.StatusConflict || status >= 500 && status < 600:
		return retryServerError
	}

	payload := strings.TrimSpace(string(prefix))
	// SSE metadata (event:, id:, comments) can precede data: in the first
	// event. Only classify its data; never retry an event after it is forwarded.
	var data strings.Builder
	for line := range strings.SplitSeq(payload, "\n") {
		if content, found := strings.CutPrefix(line, "data:"); found {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(content, " "))
		}
	}
	if data.Len() > 0 {
		payload = strings.TrimSpace(data.String())
	}
	var decoded struct {
		Error any `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		if _, retry := retryableErrorMessages[strings.ToLower(strings.TrimSuffix(payload, "."))]; retry {
			return retryServerError
		}
		return ""
	}

	code, message := errorCodeAndMessage(decoded.Error)
	code = strings.ToLower(code)
	message = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(message), "."))
	switch {
	case code == "timeout" || message == "request timed out" || message == "upstream timeout":
		return retryTimeout
	case code == "rate_limit_exceeded":
		return retryRateLimit
	}
	if _, retry := retryableErrorCodes[code]; retry {
		return retryServerError
	}
	if _, retry := retryableErrorMessages[message]; retry {
		return retryServerError
	}
	return ""
}

func errorCodeAndMessage(value any) (string, string) {
	switch typed := value.(type) {
	case map[string]any:
		code, _ := typed["code"].(string)
		if code == "" {
			code, _ = typed["type"].(string)
		}
		message, _ := typed["message"].(string)
		return code, message
	case string:
		return "", typed
	default:
		return "", ""
	}
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil {
		return max(0, seconds)
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now))
	}
	return 0
}

func retryBackoff(base, maximum time.Duration, attempt int) time.Duration {
	if base <= 0 || maximum <= 0 {
		return 0
	}
	delay := min(base, maximum)
	for range attempt {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	return min(delay, maximum)
}

func retryDelay(config Config, attempt int, providerDelay time.Duration) time.Duration {
	delay := max(retryBackoff(config.BaseBackoff, config.MaxBackoff, attempt), max(0, providerDelay))
	if config.Jitter > 0 {
		// Jitter only spreads retries; it is not used for security decisions.
		// #nosec G404
		jitter := time.Duration(rand.Int64N(int64(config.Jitter) + 1))
		const maximumDuration = time.Duration(1<<63 - 1)
		if delay > maximumDuration-jitter {
			return maximumDuration
		}
		delay += jitter
	}
	return delay
}

// timeoutRetryBody patches only a top-level reasoning_effort string in place.
// Re-marshalling RawMessage can HTML-escape large prompts and multiply memory use.
func timeoutRetryBody(body []byte) ([]byte, bool) {
	if !json.Valid(body) {
		return body, false
	}
	i := skipJSONWhitespace(body, 0)
	if i == len(body) || body[i] != '{' {
		return body, false
	}
	var valueStart, valueEnd int
	shouldLower := false
	for i = skipJSONWhitespace(body, i+1); body[i] != '}'; {
		keyStart := i
		i = jsonStringEnd(body, i)
		key := body[keyStart:i]
		i = skipJSONWhitespace(body, i) + 1 // json.Valid guaranteed the colon.
		start := skipJSONWhitespace(body, i)
		end := jsonValueEnd(body, start)
		if isReasoningKey(key) {
			// Duplicate keys follow encoding/json's last-value-wins behavior.
			shouldLower = false
			if end-start <= 32 {
				var effort string
				if json.Unmarshal(body[start:end], &effort) == nil {
					shouldLower = effort == "medium" || effort == "high"
				}
			}
			if shouldLower {
				valueStart, valueEnd = start, end
			}
		}
		i = skipJSONWhitespace(body, end)
		if body[i] == ',' {
			i = skipJSONWhitespace(body, i+1)
		}
	}
	if !shouldLower {
		return body, false
	}

	const replacement = `"low"`
	newLength := len(body) - (valueEnd - valueStart) + len(replacement)
	copy(body[valueStart+len(replacement):newLength], body[valueEnd:])
	copy(body[valueStart:valueStart+len(replacement)], replacement)
	return body[:newLength], true
}

func isReasoningKey(key []byte) bool {
	if string(key) == `"reasoning_effort"` {
		return true
	}
	if len(key) > 128 {
		return false
	}
	var decoded string
	return json.Unmarshal(key, &decoded) == nil && decoded == "reasoning_effort"
}

func skipJSONWhitespace(body []byte, i int) int {
	for i < len(body) && (body[i] == ' ' || body[i] == '\n' || body[i] == '\r' || body[i] == '\t') {
		i++
	}
	return i
}

// jsonStringEnd skips escapes without allocating the (possibly 64-MiB) string.
func jsonStringEnd(body []byte, i int) int {
	for i++; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(body)
}

// jsonValueEnd skips nested values; json.Valid has already checked syntax.
func jsonValueEnd(body []byte, i int) int {
	depth := 0
	for ; i < len(body); i++ {
		switch body[i] {
		case '"':
			i = jsonStringEnd(body, i) - 1
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return i
			}
			depth--
		case ',':
			if depth == 0 {
				return i
			}
		}
	}
	return i
}
