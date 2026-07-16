package token

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/zm/ewelink-lan-ctl/internal/store"
)

var ErrOAuthRequired = errors.New("oauth authorization required")

const accessSafetyWindow = 5 * time.Minute

type RefreshFunc func(context.Context, store.Credentials) (store.Credentials, error)

type refreshCall struct {
	done chan struct{}
	err  error
}

type Manager struct {
	load         func() (store.Credentials, error)
	save         func(store.Credentials) error
	refresh      RefreshFunc
	now          func() time.Time
	refreshAhead time.Duration

	mu             sync.Mutex
	credentials    store.Credentials
	hasCredentials bool
	inFlight       *refreshCall
	ioMu           sync.Mutex
}

func New(load func() (store.Credentials, error), save func(store.Credentials) error, refresh RefreshFunc, now func() time.Time, refreshAhead time.Duration) *Manager {
	return &Manager{load: load, save: save, refresh: refresh, now: now, refreshAhead: refreshAhead}
}

func (m *Manager) Load(ctx context.Context) error {
	if m.load == nil {
		return fmt.Errorf("load credentials: callback is nil")
	}
	m.ioMu.Lock()
	credentials, err := m.load()
	m.ioMu.Unlock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.mu.Lock()
			m.credentials = store.Credentials{}
			m.hasCredentials = false
			m.mu.Unlock()
			return nil
		}
		return fmt.Errorf("load credentials: %w", err)
	}
	if err := validate(credentials); err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	m.mu.Lock()
	m.credentials = credentials
	m.hasCredentials = true
	m.mu.Unlock()
	if needsRefresh(credentials, m.now(), m.refreshAhead) {
		return m.refreshCredentials(ctx, m.refreshAhead, false)
	}
	return nil
}

func (m *Manager) Set(credentials store.Credentials) error {
	if err := validate(credentials); err != nil {
		return err
	}
	if m.save == nil {
		return fmt.Errorf("save credentials: callback is nil")
	}
	m.ioMu.Lock()
	err := m.save(credentials)
	if err == nil {
		m.mu.Lock()
		m.credentials = credentials
		m.hasCredentials = true
		m.mu.Unlock()
	}
	m.ioMu.Unlock()
	if err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

func (m *Manager) Access(ctx context.Context) (region, token string, err error) {
	credentials, ok := m.snapshot()
	if !ok {
		return "", "", ErrOAuthRequired
	}
	if needsRefresh(credentials, m.now(), accessSafetyWindow) {
		if err := m.refreshCredentials(ctx, accessSafetyWindow, false); err != nil {
			return "", "", err
		}
		credentials, ok = m.snapshot()
		if !ok {
			return "", "", ErrOAuthRequired
		}
	}
	return credentials.Region, credentials.AccessToken, nil
}

func (m *Manager) ForceRefresh(ctx context.Context) error {
	return m.refreshCredentials(ctx, 0, true)
}

func (m *Manager) refreshCredentials(ctx context.Context, ahead time.Duration, force bool) error {
	m.mu.Lock()
	if m.inFlight != nil {
		call := m.inFlight
		m.mu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !m.hasCredentials {
		m.mu.Unlock()
		return ErrOAuthRequired
	}
	credentials := m.credentials
	if !force && !needsRefresh(credentials, m.now(), ahead) {
		m.mu.Unlock()
		return nil
	}
	if !m.now().Before(credentials.RefreshTokenExpiresAt) {
		m.mu.Unlock()
		return ErrOAuthRequired
	}
	call := &refreshCall{done: make(chan struct{})}
	m.inFlight = call
	m.mu.Unlock()

	err := m.performRefresh(ctx, credentials)
	m.mu.Lock()
	call.err = err
	m.inFlight = nil
	close(call.done)
	m.mu.Unlock()
	return err
}

func (m *Manager) performRefresh(ctx context.Context, credentials store.Credentials) error {
	if m.refresh == nil {
		return fmt.Errorf("refresh credentials: callback is nil")
	}
	m.ioMu.Lock()
	defer m.ioMu.Unlock()
	fresh, err := m.refresh(ctx, credentials)
	if err != nil {
		return fmt.Errorf("refresh credentials: %w", err)
	}
	if err := validate(fresh); err != nil {
		return fmt.Errorf("refresh credentials: %w", err)
	}
	if m.save == nil {
		return fmt.Errorf("save refreshed credentials: callback is nil")
	}
	if err := m.save(fresh); err != nil {
		return fmt.Errorf("save refreshed credentials: %w", err)
	}
	m.mu.Lock()
	m.credentials = fresh
	m.hasCredentials = true
	m.mu.Unlock()
	return nil
}

func (m *Manager) Health() bool {
	credentials, ok := m.snapshot()
	return ok && m.now().Before(credentials.RefreshTokenExpiresAt)
}

func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	m.refreshIfNeeded(ctx)
	if ctx.Err() != nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshIfNeeded(ctx)
		}
	}
}

func (m *Manager) refreshIfNeeded(ctx context.Context) {
	credentials, ok := m.snapshot()
	if ok && needsRefresh(credentials, m.now(), m.refreshAhead) {
		_ = m.refreshCredentials(ctx, m.refreshAhead, false)
	}
}

func (m *Manager) snapshot() (store.Credentials, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.credentials, m.hasCredentials
}

func needsRefresh(credentials store.Credentials, now time.Time, ahead time.Duration) bool {
	return !now.Add(ahead).Before(credentials.AccessTokenExpiresAt)
}

func validate(credentials store.Credentials) error {
	if credentials.Region == "" || credentials.AccessToken == "" || credentials.RefreshToken == "" ||
		credentials.AccessTokenExpiresAt.IsZero() || credentials.RefreshTokenExpiresAt.IsZero() {
		return errors.New("malformed credentials")
	}
	return nil
}
