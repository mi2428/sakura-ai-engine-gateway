package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

func testMemoryCgroup(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for name, value := range map[string]string{"memory.max": "6442450944", "memory.current": "0"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func waitForQueued(t *testing.T, gate *memoryGate, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gate.snapshot().queued == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("memory queue did not reach %d: %+v", count, gate.snapshot())
}

func TestMemoryAccountingFailsClosedAtStartup(t *testing.T) {
	for name, value := range map[string]string{
		"unlimited": "max", "negative": "-1", "zero": "0", "overflow": "9223372036854775808", "too small": "1024", "empty": "",
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig(nil)
			config.MemoryCgroupPath = testMemoryCgroup(t)
			if err := os.WriteFile(filepath.Join(config.MemoryCgroupPath, "memory.max"), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := newMemoryGate(config); err == nil {
				t.Fatal("accepted invalid memory.max")
			}
		})
	}
	for _, name := range []string{"memory.max", "memory.current"} {
		t.Run("missing "+name, func(t *testing.T) {
			config := testConfig(nil)
			config.MemoryCgroupPath = testMemoryCgroup(t)
			if err := os.Remove(filepath.Join(config.MemoryCgroupPath, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := newMemoryGate(config); err == nil {
				t.Fatal("accepted missing memory accounting")
			}
		})
	}
}

func TestRequestMemoryCostUsesBodySize(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	if got := requestMemoryCost(request, 64<<20); got != requestMemoryBase+8 {
		t.Fatalf("known body cost = %d", got)
	}
	request.ContentLength = -1
	if got := requestMemoryCost(request, 64<<20); got != requestMemoryBase+4*(64<<20) {
		t.Fatalf("unknown body cost = %d", got)
	}
}

func TestMemoryReservationsAreAtomicAndCancelWithoutLeaks(t *testing.T) {
	config := testConfig(nil)
	config.MemoryCgroupPath = testMemoryCgroup(t)
	gate, err := newMemoryGate(config)
	if err != nil {
		t.Fatal(err)
	}
	gate.limit = 24 << 20
	const requests = 40
	const cost = 6 << 20
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, requests)
	for range requests {
		go func() { results <- gate.acquire(ctx, cost) }()
	}
	// Exactly three six-MiB reservations fit below the 18-MiB high watermark.
	for range 3 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("initial reservations did not finish")
		}
	}
	waitForQueued(t, gate, requests-3)
	if state := gate.snapshot(); state.active != 3 || state.reserved != 3*cost {
		t.Fatalf("overcommitted reservations: %+v", state)
	}
	cancel()
	for range requests - 3 {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled acquisition = %v", err)
		}
	}
	for range 3 {
		gate.release(cost)
	}
	if state := gate.snapshot(); state.active != 0 || state.queued != 0 || state.reserved != 0 {
		t.Fatalf("leaked reservation or waiter: %+v", state)
	}
}

func TestMemoryGateHysteresisAndMonitoringFailure(t *testing.T) {
	config := testConfig(nil)
	config.MemoryCgroupPath = testMemoryCgroup(t)
	gate, err := newMemoryGate(config)
	if err != nil {
		t.Fatal(err)
	}
	var used atomic.Int64
	var failed atomic.Bool
	gate.readUsage = func() (int64, error) {
		if failed.Load() {
			return 0, errors.New("monitor unavailable")
		}
		return used.Load(), nil
	}
	used.Store(gate.pauseAt())
	if !gate.snapshot().paused {
		t.Fatal("did not pause at high watermark")
	}
	used.Store(gate.resumeAt() + 1)
	if !gate.snapshot().paused {
		t.Fatal("resumed before low watermark")
	}
	used.Store(gate.resumeAt())
	if gate.snapshot().paused {
		t.Fatal("did not resume at low watermark")
	}
	failed.Store(true)
	if err := gate.acquire(context.Background(), requestMemoryBase); err == nil {
		t.Fatal("admitted a request using a stale memory sample")
	}
	if state := gate.snapshot(); state.available || state.active != 0 || state.reserved != 0 {
		t.Fatalf("monitor failure state = %+v", state)
	}
	failed.Store(false)
	if err := gate.acquire(context.Background(), requestMemoryBase); err != nil {
		t.Fatalf("did not recover after monitor recovery: %v", err)
	}
	gate.release(requestMemoryBase)
}

func TestMemoryReleaseAndCloseWakeWaiters(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(strconv.FormatBool(closing), func(t *testing.T) {
			config := testConfig(nil)
			config.MemoryCgroupPath = testMemoryCgroup(t)
			gate, err := newMemoryGate(config)
			if err != nil {
				t.Fatal(err)
			}
			cost := gate.pauseAt()
			if err := gate.acquire(context.Background(), cost); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- gate.acquire(ctx, requestMemoryBase) }()
			waitForQueued(t, gate, 1)
			if closing {
				gate.close()
				gate.close()
			} else {
				gate.release(cost)
			}
			select {
			case err := <-done:
				if closing && !errors.Is(err, errMemoryClosed) || !closing && err != nil {
					t.Fatalf("woken acquisition = %v", err)
				}
			case <-ctx.Done():
				t.Fatal("waiter did not wake")
			}
			if closing {
				gate.release(cost)
			} else {
				gate.release(requestMemoryBase)
			}
			if state := gate.snapshot(); state.active != 0 || state.queued != 0 || state.reserved != 0 {
				t.Fatalf("leaked state after release/close: %+v", state)
			}
		})
	}
}

type observedBody struct {
	reads  atomic.Int32
	reader io.Reader
}

func (body *observedBody) Read(buffer []byte) (int, error) {
	body.reads.Add(1)
	return body.reader.Read(buffer)
}
func (*observedBody) Close() error { return nil }

func TestGatewayQueuesBeforeReadingAndResumesAfterPressure(t *testing.T) {
	server, proxy := startTestGateway(t, testConfig(nil), http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"ok":true}`)
	}))
	var used atomic.Int64
	used.Store(proxy.memory.pauseAt())
	proxy.memory.readUsage = func() (int64, error) { return used.Load(), nil }
	body := &observedBody{reader: strings.NewReader("{}")}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	request.ContentLength = 2
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { proxy.ServeHTTP(response, request); close(done) }()
	waitForQueued(t, proxy.memory, 1)
	if body.reads.Load() != 0 {
		t.Fatal("read a body while waiting for memory")
	}
	for _, path := range []string{"/health", "/metrics"} {
		result := sendRequest(t, server.Client(), http.MethodGet, server.URL+path, "", nil, nil)
		if result.StatusCode != http.StatusOK {
			t.Fatalf("%s was blocked by memory pressure", path)
		}
	}
	used.Store(0)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("request did not resume after memory pressure cleared")
	}
	if response.Code != http.StatusOK || body.reads.Load() == 0 {
		t.Fatalf("resumed response = %d, reads = %d", response.Code, body.reads.Load())
	}
	if state := proxy.memory.snapshot(); state.active != 0 || state.reserved != 0 || state.queued != 0 {
		t.Fatalf("request leaked state: %+v", state)
	}
}

func TestGatewayMemoryQueueLimitsAndMonitoringFailure(t *testing.T) {
	for _, mode := range []string{"full", "timeout", "monitor unavailable", "oversized", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			config := testConfig(nil)
			config.MaxQueuedRequests = 1
			config.QueueTimeout = 30 * time.Millisecond
			_, proxy := startTestGateway(t, config, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("rejected request reached upstream")
			}))
			proxy.memory.readUsage = func() (int64, error) {
				if mode == "monitor unavailable" {
					return 0, errors.New("monitor unavailable")
				}
				return proxy.memory.pauseAt(), nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var filled chan error
			if mode == "full" {
				filled = make(chan error, 1)
				go func() { filled <- proxy.memory.acquire(ctx, requestMemoryBase) }()
				waitForQueued(t, proxy.memory, 1)
			}
			body := &observedBody{reader: strings.NewReader("{}")}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
			request.ContentLength = 2
			want := http.StatusServiceUnavailable
			if mode == "oversized" {
				request.ContentLength = config.MaxRequestBytes + 1
				want = http.StatusRequestEntityTooLarge
			}
			if mode == "canceled" {
				cancel()
				request = request.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != want || body.reads.Load() != 0 {
				t.Fatalf("rejected response = %d, reads = %d", response.Code, body.reads.Load())
			}
			if want == http.StatusServiceUnavailable && response.Header().Get("Retry-After") != "1" {
				t.Fatal("missing Retry-After on memory rejection")
			}
			if filled != nil {
				cancel()
				if err := <-filled; !errors.Is(err, context.Canceled) {
					t.Fatalf("queued cancellation = %v", err)
				}
			}
			if state := proxy.memory.snapshot(); state.active != 0 || state.reserved != 0 || state.queued != 0 {
				t.Fatalf("rejected request leaked state: %+v", state)
			}
		})
	}
}

func TestMemoryAcquireRejectsCancellationDuringSample(t *testing.T) {
	config := testConfig(nil)
	config.MemoryCgroupPath = testMemoryCgroup(t)
	gate, err := newMemoryGate(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate.readUsage = func() (int64, error) {
		cancel()
		return 0, nil
	}
	if err := gate.acquire(ctx, requestMemoryBase); !errors.Is(err, context.Canceled) {
		t.Fatalf("admitted canceled request: %v", err)
	}
	if state := gate.snapshot(); state.active != 0 || state.reserved != 0 {
		t.Fatalf("canceled request reserved memory: %+v", state)
	}
}

func TestGatewayRejectsHeadersWithoutDrainingUnreadUploads(t *testing.T) {
	for _, test := range []struct {
		name, method, path string
		length             int64
		want               int
	}{
		{"unknown path", http.MethodPost, "/unknown", 2, http.StatusNotFound},
		{"wrong method", http.MethodPost, "/health", 2, http.StatusMethodNotAllowed},
		{"GET with body", http.MethodGet, "/v1/models", 2, http.StatusBadRequest},
		{"oversized", http.MethodPost, "/v1/chat/completions", maximumRequestBytes + 1, http.StatusRequestEntityTooLarge},
		{"queue timeout", http.MethodPost, "/v1/chat/completions", 2, http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(nil)
			config.QueueTimeout = 20 * time.Millisecond
			server, proxy := startTestGateway(t, config, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("rejected upload reached upstream")
			}))
			proxy.memory.readUsage = func() (int64, error) { return proxy.memory.pauseAt(), nil }
			connection, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close() }()
			if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			// Send headers, but never send the advertised body. Error responses
			// must not wait for net/http's automatic request-body drain.
			if _, err := fmt.Fprintf(connection, "%s %s HTTP/1.1\r\nHost: test\r\nContent-Length: %d\r\n\r\n", test.method, test.path, test.length); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: test.method})
			if err != nil {
				t.Fatalf("response blocked on unread upload: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.want)
			}
		})
	}
}

func TestGatewayReadFailuresReleaseUnknownLengthReservations(t *testing.T) {
	for name, reader := range map[string]io.Reader{
		"oversized":  strings.NewReader("12345"),
		"read error": iotest.ErrReader(errors.New("read failed")),
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig(nil)
			config.MaxRequestBytes = 4
			_, proxy := startTestGateway(t, config, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("failed upload reached upstream")
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
			request.ContentLength = -1
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			want := http.StatusBadRequest
			if name == "oversized" {
				want = http.StatusRequestEntityTooLarge
			}
			if response.Code != want {
				t.Fatalf("response = %d, want %d", response.Code, want)
			}
			if state := proxy.memory.snapshot(); state.active != 0 || state.reserved != 0 || state.queued != 0 {
				t.Fatalf("read failure leaked memory reservation: %+v", state)
			}
		})
	}
}

func TestGatewayActiveStreamSurvivesPressureMonitorFailureAndClose(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, ": ping\n")
		writer.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	config := testConfig(nil)
	config.UpstreamURL, _ = url.Parse(upstream.URL)
	config.UpstreamIdleTimeout = 5 * time.Second
	proxy, err := newTestGateway(t, config, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var used atomic.Int64
	var failed atomic.Bool
	proxy.memory.readUsage = func() (int64, error) {
		if failed.Load() {
			return 0, errors.New("monitor unavailable")
		}
		return used.Load(), nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(func() { proxy.Close(); server.Close() })
	response, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || proxy.memory.snapshot().active != 1 {
		t.Fatalf("stream was not admitted: %d", response.StatusCode)
	}
	used.Store(proxy.memory.pauseAt())
	pending := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		proxy.ServeHTTP(pending, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		close(done)
	}()
	waitForQueued(t, proxy.memory, 1)
	failed.Store(true)
	unavailable := sendRequest(t, server.Client(), http.MethodGet, server.URL+"/v1/models", "", nil, nil)
	if unavailable.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("new request passed during monitoring failure")
	}
	metrics := sendRequest(t, server.Client(), http.MethodGet, server.URL+"/metrics", "", nil, nil)
	if !strings.Contains(string(metrics.Body), "sakura_ai_engine_gateway_memory_available 0") {
		t.Fatal("metrics concealed the monitoring failure")
	}
	proxy.Close()
	select {
	case <-done:
		if pending.Code != http.StatusServiceUnavailable {
			t.Fatalf("queued shutdown response = %d", pending.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not release the pending request")
	}
	once.Do(func() { close(release) })
	body, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("active stream interrupted: body=%q error=%v", body, err)
	}
	if state := proxy.memory.snapshot(); state.active != 0 || state.reserved != 0 || state.queued != 0 {
		t.Fatalf("stream or shutdown leaked state: %+v", state)
	}
}
