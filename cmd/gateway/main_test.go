package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunRejectsMissingCredentials(t *testing.T) {
	t.Setenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS", "")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := run(context.Background(), logger); err == nil {
		t.Fatal("run() accepted missing credentials")
	}
}

func TestRunGracefullyStopsOnContextCancellation(t *testing.T) {
	for _, name := range []string{
		"SAKURA_AI_ENGINE_API_URL", "GATEWAY_MAX_REQUEST_BYTES", "GATEWAY_MAX_QUEUED_REQUESTS",
		"GATEWAY_QUEUE_TIMEOUT", "SAKURA_RETRY_MAX", "SAKURA_RETRY_BASE_INTERVAL",
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
	cgroup := t.TempDir()
	for name, value := range map[string]string{"memory.max": "6442450944", "memory.current": "0"} {
		if err := os.WriteFile(filepath.Join(cgroup, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GATEWAY_MEMORY_CGROUP_PATH", cgroup)

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
	if _, exists := os.LookupEnv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS"); exists {
		t.Error("secret account tokens were not removed from the environment")
	}
}
