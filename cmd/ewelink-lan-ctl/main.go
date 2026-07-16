package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zm/ewelink-lan-ctl/internal/config"
	"github.com/zm/ewelink-lan-ctl/internal/ewelink"
	"github.com/zm/ewelink-lan-ctl/internal/httpapi"
	"github.com/zm/ewelink-lan-ctl/internal/store"
	"github.com/zm/ewelink-lan-ctl/internal/token"
)

const shutdownTimeout = 10 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("service stopped", "category", "fatal")
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpClient := newHTTPClient(cfg)
	handler, manager, err := buildHandler(cfg, httpClient, logger)
	if err != nil {
		return fmt.Errorf("build service: %w", err)
	}
	go manager.Run(ctx, cfg.CheckInterval)

	server := newHTTPServer(cfg, handler)
	return serve(ctx, server)
}

func newHTTPClient(cfg config.Config) *http.Client {
	return &http.Client{Timeout: cfg.RequestTimeout}
}

func newHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.RequestTimeout,
	}
}

func buildHandler(cfg config.Config, httpClient *http.Client, logger *slog.Logger) (http.Handler, *token.Manager, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	state := store.File{Path: cfg.StateFile}
	client := ewelink.NewClient(ewelink.Config{
		AppID:       cfg.AppID,
		AppSecret:   cfg.AppSecret,
		CallbackURL: cfg.CallbackURL,
		HTTPClient:  httpClient,
	})
	manager := token.New(state.Load, state.Save, refreshWith(client), time.Now, cfg.RefreshAhead)
	if err := manager.Load(context.Background()); err != nil {
		logger.Error("credentials unavailable", "category", "invalid_state")
	}
	gateway := &ewelink.Gateway{Tokens: manager, Client: client}
	handler := httpapi.New(httpapi.Config{
		Gateway: gateway,
		OAuth:   client,
		Tokens:  manager,
		Logger:  logger,
	})
	return handler, manager, nil
}

type refreshClient interface {
	Refresh(context.Context, string, string) (store.Credentials, error)
}

func refreshWith(client refreshClient) token.RefreshFunc {
	return func(ctx context.Context, credentials store.Credentials) (store.Credentials, error) {
		refreshed, err := client.Refresh(ctx, credentials.Region, credentials.RefreshToken)
		if permanentRefreshRejection(err) {
			return store.Credentials{}, fmt.Errorf("refresh rejected: %w", token.ErrOAuthRequired)
		}
		return refreshed, err
	}
}

func permanentRefreshRejection(err error) bool {
	if ewelink.IsTokenError(err) {
		return true
	}
	var upstream *ewelink.UpstreamError
	return errors.As(err, &upstream) &&
		(upstream.HTTPStatus == http.StatusUnauthorized || upstream.HTTPStatus == http.StatusForbidden)
}

func serve(ctx context.Context, server *http.Server) error {
	return serveWith(ctx, server, server.ListenAndServe)
}

func serveWith(ctx context.Context, server *http.Server, serveFn func() error) error {
	if ctx.Err() != nil {
		return nil
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- serveFn()
	}()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		select {
		case err := <-serveResult:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-shutdownCtx.Done():
			return fmt.Errorf("wait for HTTP server shutdown: %w", shutdownCtx.Err())
		}
	}
}
