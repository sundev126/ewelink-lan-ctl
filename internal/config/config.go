package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	AppID, AppSecret, CallbackURL, ListenAddr, StateFile string
	RequestTimeout, CheckInterval, RefreshAhead          time.Duration
}

func Load() (Config, error) {
	config := Config{
		AppID:       os.Getenv("EWELINK_APP_ID"),
		AppSecret:   os.Getenv("EWELINK_APP_SECRET"),
		CallbackURL: valueOrDefault("EWELINK_CALLBACK_URL", "http://127.0.0.1:33998/callback"),
		ListenAddr:  valueOrDefault("EWELINK_LISTEN_ADDR", ":33998"),
		StateFile:   valueOrDefault("EWELINK_STATE_FILE", "./data/state.json"),
	}
	if config.AppID == "" {
		return Config{}, fmt.Errorf("EWELINK_APP_ID is required")
	}
	if config.AppSecret == "" {
		return Config{}, fmt.Errorf("EWELINK_APP_SECRET is required")
	}

	var err error
	if config.RequestTimeout, err = positiveDuration("EWELINK_REQUEST_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if config.CheckInterval, err = positiveDuration("EWELINK_TOKEN_CHECK_INTERVAL", 12*time.Hour); err != nil {
		return Config{}, err
	}
	if config.RefreshAhead, err = positiveDuration("EWELINK_TOKEN_REFRESH_AHEAD", 7*24*time.Hour); err != nil {
		return Config{}, err
	}

	return config, nil
}

func valueOrDefault(name, defaultValue string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return defaultValue
}

func positiveDuration(name string, defaultValue time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return duration, nil
}
