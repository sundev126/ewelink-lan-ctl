package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zm/ewelink-lan-ctl/internal/config"
	"github.com/zm/ewelink-lan-ctl/internal/ewelink"
	"github.com/zm/ewelink-lan-ctl/internal/store"
	"github.com/zm/ewelink-lan-ctl/internal/token"
)

type stubRefreshClient struct {
	err error
}

func (c stubRefreshClient) Refresh(context.Context, string, string) (store.Credentials, error) {
	return store.Credentials{}, c.err
}

type stubCredentialLoader struct {
	err     error
	healthy bool
}

func (l stubCredentialLoader) Load(context.Context) error { return l.err }
func (l stubCredentialLoader) Health() bool               { return l.healthy }

func TestRunErrorLogCategoriesAreSpecificAndSafe(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		category string
	}{
		{name: "configuration", err: fmt.Errorf("%w: sensitive", errConfiguration), category: "configuration"},
		{name: "construction", err: fmt.Errorf("%w: sensitive", errConstruction), category: "construction"},
		{name: "serve", err: fmt.Errorf("%w: sensitive", errServe), category: "listen_serve"},
		{name: "shutdown", err: fmt.Errorf("%w: sensitive", errShutdown), category: "shutdown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := runErrorLogCategory(test.err); got != test.category {
				t.Fatalf("runErrorLogCategory() = %q, want %q", got, test.category)
			}
		})
	}
}

func TestLogCredentialLoadFailureUsesDistinctSafeCategories(t *testing.T) {
	tests := []struct {
		name     string
		loader   stubCredentialLoader
		category string
	}{
		{name: "malformed state", loader: stubCredentialLoader{err: fmt.Errorf("parse sensitive: %w", store.ErrMalformedCredentials)}, category: "state_malformed"},
		{name: "state file IO", loader: stubCredentialLoader{err: &os.PathError{Op: "open", Path: "/sensitive/path", Err: os.ErrPermission}}, category: "state_io"},
		{name: "OAuth required", loader: stubCredentialLoader{err: fmt.Errorf("provider sensitive: %w", token.ErrOAuthRequired)}, category: "oauth_required"},
		{name: "transient refresh while healthy", loader: stubCredentialLoader{err: errors.New("sensitive upstream payload"), healthy: true}, category: "startup_refresh_transient"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			logCredentialLoad(context.Background(), test.loader, logger)
			got := logs.String()
			if !strings.Contains(got, `"category":"`+test.category+`"`) {
				t.Fatalf("credential load log = %q, want category %q", got, test.category)
			}
			for _, sensitive := range []string{"sensitive", "/sensitive/path", "upstream payload"} {
				if strings.Contains(got, sensitive) {
					t.Fatalf("credential load log exposed %q: %q", sensitive, got)
				}
			}
			if test.loader.healthy {
				for _, misleading := range []string{"credentials unavailable", "invalid", "malformed", "failed"} {
					if strings.Contains(strings.ToLower(got), misleading) {
						t.Fatalf("healthy transient-refresh log is misleading (%q): %q", misleading, got)
					}
				}
			}
		})
	}
}

func TestBuildHandlerStartsWithoutStoredCredentials(t *testing.T) {
	cfg := config.Config{
		AppID:         "app-id",
		AppSecret:     "app-secret",
		CallbackURL:   "http://127.0.0.1/callback",
		StateFile:     filepath.Join(t.TempDir(), "missing-state.json"),
		RefreshAhead:  7 * 24 * time.Hour,
		CheckInterval: 12 * time.Hour,
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	handler, manager, err := buildHandler(cfg, &http.Client{Timeout: time.Second}, logger)
	if err != nil {
		t.Fatalf("buildHandler() error = %v", err)
	}
	if handler == nil || manager == nil {
		t.Fatalf("buildHandler() = (%v, %v), want non-nil handler and manager", handler, manager)
	}

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", response.Code, http.StatusOK)
	}
	var body struct {
		OAuthReady bool `json:"oauth_ready"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if body.OAuthReady {
		t.Fatal("GET /healthz oauth_ready = true, want false")
	}
	if logs.Len() != 0 {
		t.Fatalf("missing state log = %q, want no log", logs.String())
	}
}

func TestBuildHandlerLogsMalformedStateAndStartsUnauthenticated(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	const sensitive = "sensitive-refresh-token"
	if err := os.WriteFile(statePath, []byte(`{"refresh_token":"`+sensitive+`"`), 0o600); err != nil {
		t.Fatalf("write malformed state: %v", err)
	}
	cfg := config.Config{
		AppID: "app-id", AppSecret: "app-secret", CallbackURL: "http://127.0.0.1/callback",
		StateFile: statePath, RefreshAhead: 7 * 24 * time.Hour,
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	handler, _, err := buildHandler(cfg, &http.Client{Timeout: time.Second}, logger)
	if err != nil {
		t.Fatalf("buildHandler() error = %v, want service to start", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(got, `"oauth_ready":false`) {
		t.Fatalf("GET /healthz = %d %s, want 200 with oauth_ready false", response.Code, got)
	}
	if got := logs.String(); !strings.Contains(got, `"level":"ERROR"`) || !strings.Contains(got, `"category":"state_malformed"`) {
		t.Fatalf("malformed-state log = %q, want ERROR state_malformed category", got)
	} else if strings.Contains(got, sensitive) {
		t.Fatalf("malformed-state log exposed credential: %q", got)
	}
}

func TestRefreshAdapterMapsPermanentRejectionToOAuthRequired(t *testing.T) {
	rejection := &ewelink.UpstreamError{HTTPStatus: http.StatusUnauthorized, Message: "sensitive provider detail"}
	_, err := refreshWith(stubRefreshClient{err: rejection})(context.Background(), store.Credentials{
		Region: "as", RefreshToken: "sensitive-refresh-token",
	})
	if !errors.Is(err, token.ErrOAuthRequired) {
		t.Fatalf("refresh error = %v, want ErrOAuthRequired", err)
	}
}

func TestRefreshAdapterPreservesTransientFailure(t *testing.T) {
	transient := &ewelink.UpstreamError{HTTPStatus: http.StatusServiceUnavailable, Code: 503}
	_, err := refreshWith(stubRefreshClient{err: transient})(context.Background(), store.Credentials{})
	if !errors.Is(err, transient) {
		t.Fatalf("refresh error = %v, want original transient failure", err)
	}
	if errors.Is(err, token.ErrOAuthRequired) {
		t.Fatalf("refresh error = %v, do not want ErrOAuthRequired", err)
	}
}

func TestServeWithShutsDownAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := &http.Server{}
	started := make(chan struct{})
	shutdown := make(chan struct{})
	server.RegisterOnShutdown(func() { close(shutdown) })
	serveFn := func() error {
		close(started)
		<-shutdown
		return http.ErrServerClosed
	}

	result := make(chan error, 1)
	go func() { result <- serveWith(ctx, server, serveFn) }()
	<-started
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("serveWith() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveWith() did not finish after context cancellation")
	}
}

func TestServeWithDoesNotStartAfterContextAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := serveWith(ctx, &http.Server{}, func() error {
		called = true
		return errors.New("unexpected serve")
	})
	if err != nil {
		t.Fatalf("serveWith() error = %v, want nil", err)
	}
	if called {
		t.Fatal("serve function called after context was already canceled")
	}
}

func TestConfiguredHTTPComponentsUseRequestTimeout(t *testing.T) {
	cfg := config.Config{ListenAddr: "127.0.0.1:0", RequestTimeout: 3 * time.Second}
	client := newHTTPClient(cfg)
	if client.Timeout != cfg.RequestTimeout {
		t.Fatalf("HTTP client timeout = %v, want %v", client.Timeout, cfg.RequestTimeout)
	}

	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := newHTTPServer(cfg, handler)
	if server.Addr != cfg.ListenAddr {
		t.Fatalf("HTTP server address = %q, want %q", server.Addr, cfg.ListenAddr)
	}
	if server.Handler == nil {
		t.Fatal("HTTP server handler = nil")
	}
	if server.ReadHeaderTimeout != cfg.RequestTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, want %v", server.ReadHeaderTimeout, cfg.RequestTimeout)
	}
}
