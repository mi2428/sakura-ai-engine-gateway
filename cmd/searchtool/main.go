// Package main starts the web search router.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mi2428/sakura-ai-engine-gateway/internal/search"
)

const (
	defaultListenAddress = ":8081"
	serverWriteTimeout   = 150 * time.Second // Five sequential 20s provider attempts plus request and response I/O.
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, logger); err != nil {
		logger.Error("search router stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	providerKeyEnvNames := []string{
		search.EnvBraveAPIKey,
		search.EnvTavilyAPIKey,
		search.EnvExaAPIKey,
		search.EnvFirecrawlAPIKey,
		search.EnvParallelAPIKey,
	}
	keys := make(map[string]string, len(providerKeyEnvNames))
	for _, name := range providerKeyEnvNames {
		keys[name] = os.Getenv(name)
	}
	router, err := search.New(os.Getenv(search.EnvClientToken), keys)
	clear(keys)
	if err != nil {
		return err
	}
	credentialEnvNames := append([]string{search.EnvClientToken}, providerKeyEnvNames...)
	for _, name := range credentialEnvNames {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("remove credential environment variable: %w", err)
		}
	}
	server := &http.Server{
		Addr:              envOr(search.EnvListenAddress, defaultListenAddress),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serverErrors := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErrors <- err
	}()
	logger.Info("search router listening", "address", server.Addr, "providers", 5)
	select {
	case err := <-serverErrors:
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownError := server.Shutdown(shutdownContext)
		if shutdownError != nil {
			shutdownError = errors.Join(shutdownError, server.Close())
		}
		return errors.Join(shutdownError, <-serverErrors)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
