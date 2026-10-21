// Package gateway implements the authenticated Sakura AI Engine API gateway.
package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	openWebUIModeHeader    = "X-OpenWebUI-Mode"
	maxErrorPrefixBytes    = 64 << 10
	streamBufferBytes      = 32 << 10
	lowReasoningEvent      = "data: {\"event\":{\"type\":\"status\",\"data\":{\"description\":\"Retried with low reasoning effort\",\"done\":true}}}\n\n"
	serverIdleTimeout      = 90 * time.Second
	clientWriteIdleTimeout = 90 * time.Second
	upstreamKeepAlive      = 30 * time.Second
	upstreamIdleConnLimit  = 32
)

type clientCredential struct {
	id        string
	tokenHash [sha256.Size]byte
}

type upstreamResponse struct {
	response *http.Response
	reader   *bufio.Reader
	prefix   []byte
	cancel   context.CancelFunc
}

// Gateway is an authenticated, OpenAI-compatible Sakura AI Engine gateway.
type Gateway struct {
	config        Config
	pool          *tokenPool
	client        *http.Client
	transport     *http.Transport
	logger        *slog.Logger
	credentials   []clientCredential
	permits       chan struct{}
	clientPermits map[string]chan struct{}
}

// New builds a gateway from validated configuration.
func New(config Config, logger *slog.Logger) (*Gateway, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, errors.New("logger is required")
	}

	clientIDs := make([]string, 0, len(config.ClientTokens))
	clientCount := len(config.ClientTokens)
	for clientID := range config.ClientTokens {
		clientIDs = append(clientIDs, clientID)
	}
	sort.Strings(clientIDs)
	credentials := make([]clientCredential, 0, len(clientIDs))
	for _, clientID := range clientIDs {
		credentials = append(credentials, clientCredential{
			id:        clientID,
			tokenHash: sha256.Sum256([]byte(config.ClientTokens[clientID])),
		})
	}
	clientPermits := make(map[string]chan struct{}, clientCount)
	for _, clientID := range clientIDs {
		clientPermits[clientID] = make(chan struct{}, config.MaxConcurrentPerClient)
	}
	config.ClientTokens = nil

	transport := newUpstreamTransport(config.UpstreamIdleTimeout, config.MaxConcurrent)
	return &Gateway{
		config:        config,
		pool:          newTokenPool(config),
		client:        &http.Client{Transport: transport, CheckRedirect: rejectRedirects},
		transport:     transport,
		logger:        logger,
		credentials:   credentials,
		permits:       make(chan struct{}, config.MaxConcurrent),
		clientPermits: clientPermits,
	}, nil
}

// Close releases idle upstream connections.
func (gateway *Gateway) Close() {
	gateway.transport.CloseIdleConnections()
}

// ServeHTTP exposes health checks and the authenticated OpenAI-compatible API.
func (gateway *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && (request.ContentLength != 0 || len(request.TransferEncoding) > 0) {
		writeError(writer, http.StatusBadRequest, "request_body_not_allowed", "GET requests must not include a body", nil)
		return
	}
	switch request.URL.Path {
	case "/health", "/readyz":
		if request.Method != http.MethodGet {
			writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", nil)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"}, nil)
		return
	}

	allowedMethod, supported := supportedAPIMethod(request.URL.Path)
	if !supported {
		writeError(writer, http.StatusNotFound, "not_found", "not found", nil)
		return
	}
	if request.Method != allowedMethod {
		writer.Header().Set("Allow", allowedMethod)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", nil)
		return
	}

	clientID, ok := gateway.authenticate(request.Header.Get("Authorization"))
	if !ok {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeError(writer, http.StatusUnauthorized, "unauthorized", "unauthorized", nil)
		return
	}
	select {
	case gateway.permits <- struct{}{}:
		defer func() { <-gateway.permits }()
	default:
		writeError(writer, http.StatusServiceUnavailable, "busy", "gateway is at capacity", http.Header{"Retry-After": {"1"}})
		return
	}
	clientPermit := gateway.clientPermits[clientID]
	select {
	case clientPermit <- struct{}{}:
		defer func() { <-clientPermit }()
	default:
		writeError(writer, http.StatusServiceUnavailable, "client_busy", "client is at its concurrency limit", http.Header{"Retry-After": {"1"}})
		return
	}

	body, err := readRequestBody(writer, request, gateway.config.MaxRequestBytes)
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large", nil)
			return
		}
		writeError(writer, http.StatusBadRequest, "invalid_request", "unable to read request body", nil)
		return
	}

	requestID, err := newRequestID()
	if err != nil {
		gateway.logger.Error("generate request ID", "error", err)
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		return
	}
	gateway.proxy(writer, request, body, clientID, requestID)
}

func (gateway *Gateway) authenticate(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}

	digest := sha256.Sum256([]byte(token))
	matchedClient := ""
	for _, credential := range gateway.credentials {
		if subtle.ConstantTimeCompare(digest[:], credential.tokenHash[:]) == 1 {
			matchedClient = credential.id
		}
	}
	return matchedClient, matchedClient != ""
}

func (gateway *Gateway) proxy(writer http.ResponseWriter, request *http.Request, body []byte, clientID, requestID string) {
	deadline := time.Now().Add(gateway.config.RetryBudget)
	requestBody := body
	var effectiveEffort string
	var lastReason retryReason
	var retryCount int

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			gateway.writeExhausted(writer, http.StatusGatewayTimeout, retryCount, lastReason, effectiveEffort)
			return
		}

		acquireContext, cancelAcquire := context.WithDeadline(request.Context(), deadline)
		lease, sharedWait, err := gateway.pool.acquire(acquireContext)
		cancelAcquire()
		if err != nil {
			if request.Context().Err() != nil {
				return
			}
			gateway.logger.Warn("account token unavailable", "request_id", requestID, "client", clientID, "wait", sharedWait)
			gateway.writeExhausted(writer, http.StatusGatewayTimeout, retryCount, lastReason, effectiveEffort)
			return
		}
		tokenSlot := lease.index

		upstreamRequest, err := gateway.newUpstreamRequest(request, requestBody, lease.token)
		if err != nil {
			gateway.logger.Error("build upstream request", "request_id", requestID, "client", clientID, "error_type", fmt.Sprintf("%T", err))
			writeError(writer, http.StatusBadGateway, "upstream_error", "unable to build upstream request", retryHeaders(retryCount, lastReason, effectiveEffort))
			return
		}

		attemptStarted := time.Now()
		attempt, err := gateway.performUpstreamAttempt(request, upstreamRequest, deadline)
		if err != nil {
			if request.Context().Err() != nil {
				return
			}
			reason := upstreamFailureReason(err)
			delay := retryDelay(gateway.config, retryCount, 0)
			if gateway.canRetry(retryCount, delay, deadline) {
				retryCount++
				lastReason = reason
				gateway.logRetry(requestID, clientID, tokenSlot, retryCount, reason, delay, sharedWait, 0)
				if !waitContext(request.Context(), delay) {
					if request.Context().Err() == nil {
						gateway.writeExhausted(writer, http.StatusGatewayTimeout, retryCount, lastReason, effectiveEffort)
					}
					return
				}
				continue
			}
			status := http.StatusBadGateway
			if reason == retryTimeout {
				status = http.StatusGatewayTimeout
			}
			gateway.logger.Warn("upstream request failed", "request_id", requestID, "client", clientID, "token_slot", tokenSlot, "attempt", retryCount+1, "reason", reason, "elapsed", time.Since(attemptStarted), "error_type", fmt.Sprintf("%T", err))
			writeError(writer, status, string(reason), "upstream unavailable", retryHeaders(retryCount, lastReason, effectiveEffort))
			return
		}
		response, reader, prefix, cancelAttempt := attempt.response, attempt.reader, attempt.prefix, attempt.cancel

		reason := classifyResponse(response.StatusCode, prefix)
		var sharedCooldown time.Duration
		if reason == retryRateLimit {
			providerDelay := retryAfter(response.Header.Get("Retry-After"), time.Now())
			scheduled, tokenCooldown := gateway.pool.rateLimited(lease, providerDelay)
			sharedCooldown = tokenCooldown
			lease.index = -1
			if gateway.canRetry(retryCount, sharedCooldown, deadline) {
				_ = response.Body.Close()
				cancelAttempt()
				retryCount++
				lastReason = reason
				gateway.logRetry(requestID, clientID, tokenSlot, retryCount, reason, scheduled, sharedWait, sharedCooldown)
				continue
			}
		} else if reason != "" {
			providerDelay := retryAfter(response.Header.Get("Retry-After"), time.Now())
			delay := retryDelay(gateway.config, retryCount, providerDelay)
			if gateway.canRetry(retryCount, delay, deadline) {
				if reason == retryTimeout {
					if updated, changed := timeoutRetryBody(requestBody); changed {
						requestBody = updated
						effectiveEffort = "low"
					}
				}
				_ = response.Body.Close()
				cancelAttempt()
				gateway.pool.recordSuccess(lease)
				retryCount++
				lastReason = reason
				gateway.logRetry(requestID, clientID, tokenSlot, retryCount, reason, delay, sharedWait, 0)
				if !waitContext(request.Context(), delay) {
					if request.Context().Err() == nil {
						gateway.writeExhausted(writer, http.StatusGatewayTimeout, retryCount, lastReason, effectiveEffort)
					}
					return
				}
				continue
			}
		}

		if reason != "" && response.StatusCode < http.StatusBadRequest {
			_ = response.Body.Close()
			cancelAttempt()
			if lease.index >= 0 {
				gateway.pool.recordSuccess(lease)
			}
			status := http.StatusBadGateway
			if reason == retryTimeout {
				status = http.StatusGatewayTimeout
			}
			gateway.logger.Warn("provider error exhausted", "request_id", requestID, "client", clientID, "token_slot", tokenSlot, "attempt", retryCount+1, "reason", reason, "status", status)
			writeError(writer, status, string(reason), "provider retry exhausted", retryHeaders(retryCount, lastReason, effectiveEffort))
			return
		}

		injectStatus := strings.EqualFold(request.Header.Get(openWebUIModeHeader), "true") && effectiveEffort == "low" && strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
		streamError := gateway.streamResponse(writer, reader, prefix, response, injectStatus, retryHeaders(retryCount, lastReason, effectiveEffort))
		cancelAttempt()
		if lease.index >= 0 {
			gateway.pool.recordSuccess(lease)
		}
		if streamError != nil {
			gateway.logger.Warn("stream terminated", "request_id", requestID, "client", clientID, "token_slot", tokenSlot, "status", response.StatusCode, "error_type", fmt.Sprintf("%T", streamError))
			return
		}
		gateway.logger.Info("upstream response", "request_id", requestID, "client", clientID, "token_slot", tokenSlot, "attempts", retryCount+1, "status", response.StatusCode, "retry_reason", reason, "elapsed", time.Since(attemptStarted), "shared_wait", sharedWait)
		return
	}
}

func (gateway *Gateway) canRetry(retryCount int, delay time.Duration, deadline time.Time) bool {
	remaining := time.Until(deadline)
	// Subtract before comparing: provider Retry-After may approach MaxInt64,
	// so adding the attempt timeout to it would overflow into a negative delay.
	return retryCount < gateway.config.MaxRetries && delay >= 0 && delay < remaining && gateway.config.UpstreamIdleTimeout < remaining-delay
}

func (gateway *Gateway) newUpstreamRequest(request *http.Request, body []byte, token string) (*http.Request, error) {
	target, err := parseUpstreamTarget(gateway.config.UpstreamURL, request)
	if err != nil {
		return nil, err
	}
	var requestBody io.Reader
	if request.Method == http.MethodPost {
		requestBody = bytes.NewReader(body)
	}
	// #nosec G704 -- target host is the validated Sakura origin and the path is allowlisted.
	upstreamRequest, err := http.NewRequestWithContext(request.Context(), request.Method, target.String(), requestBody)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(upstreamRequest.Header, request.Header)
	upstreamRequest.Host = gateway.config.UpstreamURL.Host
	upstreamRequest.Header.Set("Authorization", "Bearer "+token)
	// Always inspect the first upstream event as identity bytes, regardless
	// of the downstream client's preferred content encoding.
	upstreamRequest.Header.Set("Accept-Encoding", "identity")
	return upstreamRequest, nil
}

// performUpstreamAttempt enforces the retry budget through the bounded error
// prefix: a complete SSE event, a heartbeat line, or up to 64 KiB of JSON.
// After that, only the request context and I/O idle deadlines limit streaming.
func (gateway *Gateway) performUpstreamAttempt(request, upstreamRequest *http.Request, deadline time.Time) (upstreamResponse, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return upstreamResponse{}, context.DeadlineExceeded
	}
	attemptContext, cancelAttempt := context.WithCancel(request.Context())
	// Bound connect, headers, and the first response line. Stop this timer
	// once a response is ready so a valid SSE stream can outlive retry budget.
	attemptTimer := time.AfterFunc(remaining, cancelAttempt)
	upstreamRequest = upstreamRequest.WithContext(attemptContext)

	// #nosec G704 -- the destination origin is validated configuration and the route is allowlisted.
	response, err := gateway.client.Do(upstreamRequest)
	if err != nil {
		budgetExpired := !attemptTimer.Stop() || time.Until(deadline) <= 0
		cancelAttempt()
		if request.Context().Err() != nil {
			return upstreamResponse{}, request.Context().Err()
		}
		if budgetExpired {
			return upstreamResponse{}, context.DeadlineExceeded
		}
		return upstreamResponse{}, err
	}
	if err := decodeUpstreamBody(response); err != nil {
		budgetExpired := !attemptTimer.Stop() || time.Until(deadline) <= 0
		_ = response.Body.Close()
		cancelAttempt()
		if request.Context().Err() != nil {
			return upstreamResponse{}, request.Context().Err()
		}
		if budgetExpired {
			return upstreamResponse{}, context.DeadlineExceeded
		}
		return upstreamResponse{}, err
	}

	reader, prefix, err := readErrorPrefix(response.Body, strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream"))
	if err != nil {
		budgetExpired := !attemptTimer.Stop() || time.Until(deadline) <= 0
		_ = response.Body.Close()
		cancelAttempt()
		if request.Context().Err() != nil {
			return upstreamResponse{}, request.Context().Err()
		}
		if budgetExpired {
			return upstreamResponse{}, context.DeadlineExceeded
		}
		return upstreamResponse{}, err
	}

	budgetExpired := !attemptTimer.Stop() || time.Until(deadline) <= 0
	if request.Context().Err() != nil {
		_ = response.Body.Close()
		cancelAttempt()
		return upstreamResponse{}, request.Context().Err()
	}
	if budgetExpired {
		_ = response.Body.Close()
		cancelAttempt()
		return upstreamResponse{}, context.DeadlineExceeded
	}
	return upstreamResponse{response: response, reader: reader, prefix: prefix, cancel: cancelAttempt}, nil
}

func (gateway *Gateway) streamResponse(writer http.ResponseWriter, reader *bufio.Reader, prefix []byte, response *http.Response, injectStatus bool, extraHeaders http.Header) error {
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(writer.Header(), response.Header, injectStatus)
	for name, values := range extraHeaders {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(writer)
	writeChunk := func(chunk []byte) bool {
		if err := controller.SetWriteDeadline(time.Now().Add(clientWriteIdleTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		count, writeError := writer.Write(chunk)
		if writeError == nil && count != len(chunk) {
			writeError = io.ErrShortWrite
		}
		if writeError == nil {
			writeError = controller.Flush()
		}
		clearError := controller.SetWriteDeadline(time.Time{})
		if errors.Is(clearError, http.ErrNotSupported) {
			clearError = nil
		}
		return writeError == nil && clearError == nil
	}
	if injectStatus && !writeChunk([]byte(lowReasoningEvent)) {
		return errors.New("write retry status event")
	}
	if len(prefix) > 0 && !writeChunk(prefix) {
		return errors.New("write upstream response prefix")
	}
	buffer := make([]byte, streamBufferBytes)
	for {
		count, err := reader.Read(buffer)
		if count > 0 && !writeChunk(buffer[:count]) {
			return errors.New("write streamed response chunk")
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read upstream response stream: %w", err)
		}
	}
}

func (gateway *Gateway) writeExhausted(writer http.ResponseWriter, status, retryCount int, reason retryReason, effort string) {
	message := "upstream retry budget exhausted"
	code := string(reason)
	if code == "" {
		code = "timeout"
	}
	writeError(writer, status, code, message, retryHeaders(retryCount, reason, effort))
}

func (gateway *Gateway) logRetry(requestID, clientID string, tokenSlot, retryCount int, reason retryReason, scheduled, sharedWait, cooldown time.Duration) {
	gateway.logger.Info("retry upstream request", "request_id", requestID, "client", clientID, "token_slot", tokenSlot, "retry", retryCount, "reason", reason, "scheduled", scheduled, "shared_wait", sharedWait, "cooldown_wait", cooldown)
}

func newUpstreamTransport(idleTimeout time.Duration, maxConcurrent int) *http.Transport {
	dialer := &net.Dialer{Timeout: idleTimeout, KeepAlive: upstreamKeepAlive}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialIdleTimeout(ctx, dialer, network, address, idleTimeout)
		},
		// Per-operation deadlines on a shared connection would interfere with
		// unrelated HTTP/2 streams, so this transport intentionally uses HTTP/1.
		ForceAttemptHTTP2:     false,
		MaxConnsPerHost:       maxConcurrent,
		MaxIdleConns:          upstreamIdleConnLimit,
		MaxIdleConnsPerHost:   maxConcurrent,
		IdleConnTimeout:       serverIdleTimeout,
		TLSHandshakeTimeout:   idleTimeout,
		ResponseHeaderTimeout: idleTimeout,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
}

type idleTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func dialIdleTimeout(ctx context.Context, dialer *net.Dialer, network, address string, timeout time.Duration) (net.Conn, error) {
	connection, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return idleTimeoutConn{Conn: connection, timeout: timeout}, nil
}

// Read and Write reset idle deadlines for every socket operation, preserving
// long-lived responses that continue to make progress.
func (connection idleTimeoutConn) Read(buffer []byte) (int, error) {
	if err := connection.SetReadDeadline(time.Now().Add(connection.timeout)); err != nil {
		return 0, err
	}
	return connection.Conn.Read(buffer)
}

func (connection idleTimeoutConn) Write(buffer []byte) (int, error) {
	if err := connection.SetWriteDeadline(time.Now().Add(connection.timeout)); err != nil {
		return 0, err
	}
	return connection.Conn.Write(buffer)
}

func rejectRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func copyRequestHeaders(destination, source http.Header) {
	blocked := hopByHopHeaders(source)
	blocked["Authorization"] = struct{}{}
	blocked["Host"] = struct{}{}
	blocked["Content-Length"] = struct{}{}
	blocked["Accept-Encoding"] = struct{}{}
	blocked[http.CanonicalHeaderKey(openWebUIModeHeader)] = struct{}{}
	for _, name := range []string{
		"Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
		"Tailscale-User-Login", "Tailscale-User-Name", "Tailscale-User-Profile-Pic", "Tailscale-App-Capabilities",
	} {
		blocked[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(name)
		if _, hopByHop := blocked[canonical]; hopByHop {
			continue
		}
		for _, value := range values {
			destination.Add(canonical, value)
		}
	}
}

func copyResponseHeaders(destination, source http.Header, stripContentLength bool) {
	blocked := hopByHopHeaders(source)
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(name)
		if _, hopByHop := blocked[canonical]; hopByHop || isRetryHeader(canonical) || canonical == "Set-Cookie" || stripContentLength && canonical == "Content-Length" {
			continue
		}
		for _, value := range values {
			destination.Add(canonical, value)
		}
	}
}

func hopByHopHeaders(headers http.Header) map[string]struct{} {
	blocked := map[string]struct{}{
		"Connection": {}, "Keep-Alive": {}, "Proxy-Authenticate": {}, "Proxy-Authorization": {},
		"Te": {}, "Trailer": {}, "Transfer-Encoding": {}, "Upgrade": {},
	}
	for _, connection := range headers.Values("Connection") {
		for header := range strings.SplitSeq(connection, ",") {
			if header = strings.TrimSpace(header); header != "" {
				blocked[http.CanonicalHeaderKey(header)] = struct{}{}
			}
		}
	}
	return blocked
}

func isRetryHeader(header string) bool {
	return strings.HasPrefix(strings.ToLower(header), "x-sakura-retry-") || strings.EqualFold(header, "X-Sakura-Effective-Reasoning-Effort")
}

func readRequestBody(writer http.ResponseWriter, request *http.Request, maximum int64) ([]byte, error) {
	if request.Method == http.MethodGet || request.Body == nil {
		return nil, nil
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maximum)
	return io.ReadAll(request.Body)
}

func readErrorPrefix(body io.Reader, sse bool) (*bufio.Reader, []byte, error) {
	reader := bufio.NewReader(body)
	prefix := make([]byte, 0, 512)
	lineStart := 0
	// Inspect the first SSE event (or 64 KiB). A complete comment/heartbeat
	// line starts streaming immediately; after forwarding bytes we cannot retry.
	for len(prefix) < maxErrorPrefixBytes {
		value, err := reader.ReadByte()
		if errors.Is(err, io.EOF) {
			return reader, prefix, nil
		}
		if err != nil {
			return reader, nil, err
		}
		prefix = append(prefix, value)
		if sse && value == '\n' {
			line := bytes.TrimSpace(prefix[lineStart:])
			if len(line) == 0 || line[0] == ':' {
				break
			}
			lineStart = len(prefix)
		}
	}
	return reader, prefix, nil
}

// gzipBody closes both the inflater and the socket; gzip.Reader.Close alone
// does not close the underlying HTTP response body.
type gzipBody struct {
	*gzip.Reader
	upstream io.ReadCloser
}

func (body *gzipBody) Close() error {
	return errors.Join(body.Reader.Close(), body.upstream.Close())
}

func decodeUpstreamBody(response *http.Response) error {
	switch strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return nil
	case "gzip":
		decoder, err := gzip.NewReader(response.Body)
		if err != nil {
			return err
		}
		response.Body = &gzipBody{Reader: decoder, upstream: response.Body}
		response.Header.Del("Content-Encoding")
		response.Header.Del("Content-Length")
		return nil
	default:
		return errors.New("unsupported upstream content encoding")
	}
}

func retryHeaders(count int, reason retryReason, effort string) http.Header {
	headers := make(http.Header)
	if count == 0 {
		return headers
	}
	headers.Set("X-Sakura-Retry-Count", fmt.Sprint(count))
	if reason != "" {
		headers.Set("X-Sakura-Retry-Reason", string(reason))
	}
	if effort != "" {
		headers.Set("X-Sakura-Effective-Reasoning-Effort", effort)
	}
	return headers
}

func upstreamFailureReason(err error) retryReason {
	if errors.Is(err, context.DeadlineExceeded) {
		return retryTimeout
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return retryTimeout
	}
	return retryUpstream
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func supportedAPIMethod(path string) (string, bool) {
	switch path {
	case "/v1/models":
		return http.MethodGet, true
	case "/v1/chat/completions":
		return http.MethodPost, true
	default:
		return "", false
	}
}

func newRequestID() (string, error) {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func writeJSON(writer http.ResponseWriter, status int, payload any, headers http.Header) {
	body, err := json.Marshal(payload)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", fmt.Sprint(len(body)))
	writer.Header().Set("Cache-Control", "no-store")
	for name, values := range headers {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func writeError(writer http.ResponseWriter, status int, code, message string, headers http.Header) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}}, headers)
}

func parseUpstreamTarget(base *url.URL, request *http.Request) (*url.URL, error) {
	target := *base
	target.Path = request.URL.Path
	target.RawPath = request.URL.RawPath
	target.RawQuery = request.URL.RawQuery
	if target.Host == "" {
		return nil, errors.New("upstream host is missing")
	}
	return &target, nil
}
