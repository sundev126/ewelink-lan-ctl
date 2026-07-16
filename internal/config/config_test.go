package config

import (
	"testing"
	"time"
)

var configEnvironment = []string{
	"EWELINK_APP_ID",
	"EWELINK_APP_SECRET",
	"EWELINK_CALLBACK_URL",
	"EWELINK_LISTEN_ADDR",
	"EWELINK_STATE_FILE",
	"EWELINK_REQUEST_TIMEOUT",
	"EWELINK_TOKEN_CHECK_INTERVAL",
	"EWELINK_TOKEN_REFRESH_AHEAD",
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range configEnvironment {
		t.Setenv(name, "")
	}
}

func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	clearConfigEnvironment(t)
	t.Setenv("EWELINK_APP_ID", "app-id")
	t.Setenv("EWELINK_APP_SECRET", "app-secret")
}

func TestLoadRequiresAppID(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("EWELINK_APP_SECRET", "app-secret")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want missing App ID error")
	}
}

func TestLoadRequiresAppSecret(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("EWELINK_APP_ID", "app-id")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want missing App Secret error")
	}
}

func TestLoadUsesDefaults(t *testing.T) {
	setRequiredEnvironment(t)

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := Config{
		AppID:          "app-id",
		AppSecret:      "app-secret",
		CallbackURL:    "http://127.0.0.1:33998/callback",
		ListenAddr:     ":33998",
		StateFile:      "./data/state.json",
		RequestTimeout: 10 * time.Second,
		CheckInterval:  12 * time.Hour,
		RefreshAhead:   7 * 24 * time.Hour,
	}
	if got != want {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadUsesOverrides(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("EWELINK_CALLBACK_URL", "https://example.test/callback")
	t.Setenv("EWELINK_LISTEN_ADDR", "127.0.0.1:8080")
	t.Setenv("EWELINK_STATE_FILE", "/var/lib/ewelink/state.json")
	t.Setenv("EWELINK_REQUEST_TIMEOUT", "3s")
	t.Setenv("EWELINK_TOKEN_CHECK_INTERVAL", "30m")
	t.Setenv("EWELINK_TOKEN_REFRESH_AHEAD", "48h")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := Config{
		AppID:          "app-id",
		AppSecret:      "app-secret",
		CallbackURL:    "https://example.test/callback",
		ListenAddr:     "127.0.0.1:8080",
		StateFile:      "/var/lib/ewelink/state.json",
		RequestTimeout: 3 * time.Second,
		CheckInterval:  30 * time.Minute,
		RefreshAhead:   48 * time.Hour,
	}
	if got != want {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadRejectsNonPositiveDurations(t *testing.T) {
	tests := []struct {
		name     string
		variable string
		value    string
	}{
		{name: "zero request timeout", variable: "EWELINK_REQUEST_TIMEOUT", value: "0s"},
		{name: "negative check interval", variable: "EWELINK_TOKEN_CHECK_INTERVAL", value: "-1s"},
		{name: "zero refresh ahead", variable: "EWELINK_TOKEN_REFRESH_AHEAD", value: "0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv(tt.variable, tt.value)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() error = nil, want error for %s=%q", tt.variable, tt.value)
			}
		})
	}
}
