package gateway

import (
	"io"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	maxTrackedModels = 128
	maxModelBytes    = 128
	unknownModel     = "unknown"
	otherModel       = "other"
)

type gatewayMetrics struct {
	mu              sync.Mutex
	models          map[string]struct{}
	chatRequests    map[string]uint64
	attempts        map[attemptMetricKey]uint64
	retries         map[retryMetricKey]uint64
	usage           map[usageMetricKey]tokenUsageTotals
	usageResponses  map[usageMetricKey]uint64
	usageIncomplete map[usageMetricKey]uint64
}

type attemptMetricKey struct {
	accountSlot int
	model       string
	statusCode  int
}

type retryMetricKey struct {
	accountSlot int
	model       string
	reason      string
}

type usageMetricKey struct {
	accountSlot int
	model       string
}

type tokenUsageTotals struct {
	input       uint64
	output      uint64
	total       uint64
	inputKnown  bool
	outputKnown bool
	totalKnown  bool
}

func newGatewayMetrics() *gatewayMetrics {
	return &gatewayMetrics{
		models:          make(map[string]struct{}),
		chatRequests:    make(map[string]uint64),
		attempts:        make(map[attemptMetricKey]uint64),
		retries:         make(map[retryMetricKey]uint64),
		usage:           make(map[usageMetricKey]tokenUsageTotals),
		usageResponses:  make(map[usageMetricKey]uint64),
		usageIncomplete: make(map[usageMetricKey]uint64),
	}
}

func (metrics *gatewayMetrics) model(value string) string {
	if value == "" || len(value) > maxModelBytes || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return unknownModel
	}

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if _, exists := metrics.models[value]; exists {
		return value
	}
	if len(metrics.models) >= maxTrackedModels {
		return otherModel
	}
	metrics.models[value] = struct{}{}
	return value
}

func (metrics *gatewayMetrics) recordAttempt(accountSlot int, model string, statusCode int) {
	metrics.mu.Lock()
	metrics.attempts[attemptMetricKey{accountSlot: accountSlot + 1, model: model, statusCode: statusCode}]++
	metrics.mu.Unlock()
}

func (metrics *gatewayMetrics) recordChatRequest(model string) {
	metrics.mu.Lock()
	metrics.chatRequests[model]++
	metrics.mu.Unlock()
}

func (metrics *gatewayMetrics) recordRetry(accountSlot int, model, reason string) {
	metrics.mu.Lock()
	metrics.retries[retryMetricKey{accountSlot: accountSlot + 1, model: model, reason: reason}]++
	metrics.mu.Unlock()
}

func (metrics *gatewayMetrics) recordUsage(accountSlot int, model string, usage observedUsage, complete bool) {
	key := usageMetricKey{accountSlot: accountSlot + 1, model: model}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if usage.found {
		metrics.usageResponses[key]++
		totals := metrics.usage[key]
		if usage.inputKnown {
			totals.input += usage.inputTokens
			totals.inputKnown = true
		}
		if usage.outputKnown {
			totals.output += usage.outputTokens
			totals.outputKnown = true
		}
		if usage.totalKnown {
			totals.total += usage.totalTokens
			totals.totalKnown = true
		}
		metrics.usage[key] = totals
	}
	if complete && (!usage.inputKnown || !usage.outputKnown) {
		metrics.usageIncomplete[key]++
	}
}

func (metrics *gatewayMetrics) serveHTTP(writer http.ResponseWriter, pool *tokenPool, inFlight int) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")

	metrics.mu.Lock()
	attempts := maps.Clone(metrics.attempts)
	chatRequests := maps.Clone(metrics.chatRequests)
	retries := maps.Clone(metrics.retries)
	usage := maps.Clone(metrics.usage)
	usageResponses := maps.Clone(metrics.usageResponses)
	usageIncomplete := maps.Clone(metrics.usageIncomplete)
	metrics.mu.Unlock()

	var output strings.Builder
	appendHeader(&output, "sakura_ai_engine_gateway_in_flight_requests", "Number of authenticated requests currently holding a gateway concurrency permit.", "gauge")
	appendSample(&output, "sakura_ai_engine_gateway_in_flight_requests", strconv.Itoa(inFlight))
	appendHeader(&output, "sakura_ai_engine_gateway_chat_completion_requests_total", "Number of authenticated chat completion requests accepted by the gateway, counted once per caller request regardless of upstream retries.", "counter")
	for _, model := range sortedKeys(chatRequests, func(left, right string) bool { return left < right }) {
		appendSample(&output, "sakura_ai_engine_gateway_chat_completion_requests_total", label("model", model), strconv.FormatUint(chatRequests[model], 10))
	}
	appendHeader(&output, "sakura_ai_engine_gateway_upstream_attempts_total", "Number of upstream attempts by account slot, requested model, and response status code. Model is unknown when no valid model was supplied. Status code 0 means no HTTP response was received.", "counter")
	attemptKeys := sortedKeys(attempts, func(left, right attemptMetricKey) bool {
		if left.accountSlot != right.accountSlot {
			return left.accountSlot < right.accountSlot
		}
		if left.model != right.model {
			return left.model < right.model
		}
		return left.statusCode < right.statusCode
	})
	for _, key := range attemptKeys {
		appendSample(&output, "sakura_ai_engine_gateway_upstream_attempts_total", label("account_slot", strconv.Itoa(key.accountSlot)), label("model", key.model), label("status_code", strconv.Itoa(key.statusCode)), strconv.FormatUint(attempts[key], 10))
	}

	appendHeader(&output, "sakura_ai_engine_gateway_upstream_retries_scheduled_total", "Number of upstream retries scheduled before backoff, by previous account slot, requested model, and retry reason.", "counter")
	retryKeys := sortedKeys(retries, func(left, right retryMetricKey) bool {
		if left.accountSlot != right.accountSlot {
			return left.accountSlot < right.accountSlot
		}
		if left.model != right.model {
			return left.model < right.model
		}
		return left.reason < right.reason
	})
	for _, key := range retryKeys {
		appendSample(&output, "sakura_ai_engine_gateway_upstream_retries_scheduled_total", label("account_slot", strconv.Itoa(key.accountSlot)), label("model", key.model), label("reason", key.reason), strconv.FormatUint(retries[key], 10))
	}

	appendHeader(&output, "sakura_ai_engine_gateway_usage_tokens_total", "Reported input, output, and total tokens by account slot and requested model.", "counter")
	usageKeys := sortedKeys(usage, func(left, right usageMetricKey) bool {
		if left.accountSlot != right.accountSlot {
			return left.accountSlot < right.accountSlot
		}
		return left.model < right.model
	})
	for _, key := range usageKeys {
		totals := usage[key]
		for _, sample := range []struct {
			kind  string
			value uint64
			known bool
		}{{"input", totals.input, totals.inputKnown}, {"output", totals.output, totals.outputKnown}, {"total", totals.total, totals.totalKnown}} {
			if sample.known {
				appendSample(&output, "sakura_ai_engine_gateway_usage_tokens_total", label("account_slot", strconv.Itoa(key.accountSlot)), label("model", key.model), label("token_type", sample.kind), strconv.FormatUint(sample.value, 10))
			}
		}
	}

	appendHeader(&output, "sakura_ai_engine_gateway_usage_responses_total", "Number of chat completion responses that included usage data.", "counter")
	appendUsageCounts(&output, "sakura_ai_engine_gateway_usage_responses_total", usageResponses)
	appendHeader(&output, "sakura_ai_engine_gateway_usage_incomplete_total", "Number of successful chat completion responses without both input and output token counts.", "counter")
	appendUsageCounts(&output, "sakura_ai_engine_gateway_usage_incomplete_total", usageIncomplete)

	appendHeader(&output, "sakura_ai_engine_gateway_account_cooldown_seconds", "Remaining rate-limit cooldown in seconds for each one-based account slot.", "gauge")
	appendHeader(&output, "sakura_ai_engine_gateway_account_rate_limit_streak", "Consecutive rate-limit responses for each one-based account slot.", "gauge")
	for index, slot := range pool.metricsSnapshot() {
		accountSlot := strconv.Itoa(index + 1)
		appendSample(&output, "sakura_ai_engine_gateway_account_cooldown_seconds", label("account_slot", accountSlot), strconv.FormatFloat(slot.cooldown.Seconds(), 'f', -1, 64))
		appendSample(&output, "sakura_ai_engine_gateway_account_rate_limit_streak", label("account_slot", accountSlot), strconv.Itoa(slot.rateLimitStreak))
	}

	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, output.String())
}

func appendUsageCounts(output *strings.Builder, name string, counts map[usageMetricKey]uint64) {
	keys := sortedKeys(counts, func(left, right usageMetricKey) bool {
		if left.accountSlot != right.accountSlot {
			return left.accountSlot < right.accountSlot
		}
		return left.model < right.model
	})
	for _, key := range keys {
		appendSample(output, name, label("account_slot", strconv.Itoa(key.accountSlot)), label("model", key.model), strconv.FormatUint(counts[key], 10))
	}
}

func appendHeader(output *strings.Builder, name, help, metricType string) {
	output.WriteString("# HELP ")
	output.WriteString(name)
	output.WriteByte(' ')
	output.WriteString(help)
	output.WriteString("\n# TYPE ")
	output.WriteString(name)
	output.WriteByte(' ')
	output.WriteString(metricType)
	output.WriteByte('\n')
}

func appendSample(output *strings.Builder, name string, labels ...string) {
	value := labels[len(labels)-1]
	labels = labels[:len(labels)-1]
	output.WriteString(name)
	if len(labels) > 0 {
		output.WriteByte('{')
		for index, pair := range labels {
			if index > 0 {
				output.WriteByte(',')
			}
			output.WriteString(pair)
		}
		output.WriteByte('}')
	}
	output.WriteByte(' ')
	output.WriteString(value)
	output.WriteByte('\n')
}

func label(name, value string) string {
	var escaped strings.Builder
	for _, character := range value {
		switch character {
		case '\\':
			escaped.WriteString(`\\`)
		case '"':
			escaped.WriteString(`\"`)
		case '\n':
			escaped.WriteString(`\n`)
		default:
			escaped.WriteRune(character)
		}
	}
	return name + `="` + escaped.String() + `"`
}

func sortedKeys[K comparable, V any](values map[K]V, less func(K, K) bool) []K {
	keys := make([]K, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool { return less(keys[left], keys[right]) })
	return keys
}
