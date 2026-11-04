// Package main starts the Sakura AI Engine gateway.
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

	"github.com/mi2428/sakura-ai-engine-gateway/internal/gateway"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	config, err := gateway.LoadConfig()
	if err != nil {
		return err
	}
	proxy, err := gateway.New(config, logger)
	if err != nil {
		return err
	}
	defer proxy.Close()
	if err := os.Unsetenv("SAKURA_AI_ENGINE_ACCOUNT_TOKENS"); err != nil {
		return fmt.Errorf("remove account tokens from process environment: %w", err)
	}

	server := &http.Server{
		Addr:              config.ListenAddress,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// WriteTimeout is intentionally unset: a valid completion may stream for
		// longer than any fixed response deadline.
	}
	serverErrors := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErrors <- err
	}()
	logger.Info("gateway listening", "address", config.ListenAddress, "upstream", config.UpstreamURL.Host, "account_tokens", len(config.AccountTokens))

	select {
	case err := <-serverErrors:
		return err
	case <-ctx.Done():
		proxy.Close()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		shutdownError := server.Shutdown(shutdownContext)
		if shutdownError != nil {
			shutdownError = errors.Join(shutdownError, server.Close())
		}
		serveError := <-serverErrors
		return errors.Join(shutdownError, serveError)
	}
}
