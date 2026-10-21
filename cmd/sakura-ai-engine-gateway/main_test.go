package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestRunRejectsMissingCredentials(t *testing.T) {
	t.Setenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS", "")
	t.Setenv("GATEWAY_CLIENT_TOKENS", "")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := run(context.Background(), logger); err == nil {
		t.Fatal("run() accepted missing credentials")
	}
}

func TestRunGracefullyStopsOnContextCancellation(t *testing.T) {
	for _, name := range []string{
		"SAKURA_AI_ENGINE_API_URL", "GATEWAY_MAX_REQUEST_BYTES", "GATEWAY_MAX_CONCURRENT_REQUESTS",
		"GATEWAY_MAX_CONCURRENT_REQUESTS_PER_CLIENT", "SAKURA_RETRY_MAX", "SAKURA_RETRY_BASE_INTERVAL",
		"SAKURA_RETRY_MAX_INTERVAL", "SAKURA_RETRY_JITTER", "SAKURA_RETRY_BUDGET", "SAKURA_UPSTREAM_IDLE_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_LISTEN_ADDRESS", address)
	t.Setenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS", `["test-account"]`)
	t.Setenv("GATEWAY_CLIENT_TOKENS", `{"test":"01234567890123456789012345678901"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	ready := false
	for range 100 {
		response, requestErr := client.Get("http://" + address + "/health")
		if requestErr == nil {
			_ = response.Body.Close()
			ready = response.StatusCode == http.StatusOK
		}
		if ready {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("gateway stopped before serving health: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("gateway never served health")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() returned shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run() did not stop after context cancellation")
	}
	for _, name := range []string{"SAKURA_AI_ENGINE_ACCOUNT_TOKENS", "GATEWAY_CLIENT_TOKENS"} {
		if _, exists := os.LookupEnv(name); exists {
			t.Errorf("secret environment variable %s was not removed", name)
		}
	}
}
