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

// RefreshFunc returns an error wrapping ErrOAuthRequired when the provider
// permanently rejects the refresh credentials. Other errors are transient.
type RefreshFunc func(context.Context, store.Credentials) (store.Credentials, error)

type refreshCall struct {
	done       chan struct{}
	err        error
	applied    bool
	waiters    int
	generation uint64
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
	generation     uint64
	inFlight       *refreshCall
	mutationGate   chan struct{}
}

func New(load func() (store.Credentials, error), save func(store.Credentials) error, refresh RefreshFunc, now func() time.Time, refreshAhead time.Duration) *Manager {
	manager := &Manager{
		load: load, save: save, refresh: refresh, now: now, refreshAhead: refreshAhead,
		mutationGate: make(chan struct{}, 1),
	}
	manager.mutationGate <- struct{}{}
	return manager
}

func (m *Manager) Load(ctx context.Context) error {
	if m.load == nil {
		return fmt.Errorf("load credentials: callback is nil")
	}
	m.mu.Lock()
	generation := m.generation
	m.mu.Unlock()

	credentials, err := m.load()
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return fmt.Errorf("load credentials: %w", err)
	}
	if !missing {
		if err := validate(credentials); err != nil {
			return fmt.Errorf("load credentials: %w", err)
		}
	}

	<-m.mutationGate
	m.mu.Lock()
	published := m.generation == generation
	if published {
		if missing {
			m.credentials = store.Credentials{}
			m.hasCredentials = false
		} else {
			m.credentials = credentials
			m.hasCredentials = true
		}
		m.generation++
	}
	m.mu.Unlock()
	m.mutationGate <- struct{}{}
	if !published || missing {
		return nil
	}
	if needsRefresh(credentials, m.now(), m.refreshAhead) {
		return m.ensureRefresh(ctx, m.refreshAhead, false)
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
	<-m.mutationGate
	if err := m.save(credentials); err != nil {
		m.mutationGate <- struct{}{}
		return fmt.Errorf("save credentials: %w", err)
	}
	m.mu.Lock()
	m.credentials = credentials
	m.hasCredentials = true
	m.generation++
	m.mu.Unlock()
	m.mutationGate <- struct{}{}
	return nil
}

func (m *Manager) Access(ctx context.Context) (region, token string, err error) {
	for {
		credentials, ok, generation := m.snapshotVersion()
		if !ok {
			return "", "", ErrOAuthRequired
		}
		if !m.now().Before(credentials.RefreshTokenExpiresAt) {
			if !m.markOAuthRequired(generation) {
				continue
			}
			return "", "", ErrOAuthRequired
		}
		if !needsRefresh(credentials, m.now(), accessSafetyWindow) {
			return credentials.Region, credentials.AccessToken, nil
		}
		applied, err := m.refreshCredentials(ctx, accessSafetyWindow, false)
		if err != nil {
			return "", "", err
		}
		if !applied {
			continue
		}
	}
}

func (m *Manager) ForceRefresh(ctx context.Context) error {
	applied, err := m.refreshCredentials(ctx, 0, true)
	if err != nil || applied {
		return err
	}
	return m.ensureRefresh(ctx, accessSafetyWindow, false)
}

func (m *Manager) ensureRefresh(ctx context.Context, ahead time.Duration, force bool) error {
	for {
		applied, err := m.refreshCredentials(ctx, ahead, force)
		if err != nil || applied {
			return err
		}
	}
}

func (m *Manager) refreshCredentials(ctx context.Context, ahead time.Duration, force bool) (bool, error) {
	m.mu.Lock()
	if m.inFlight != nil {
		call := m.inFlight
		call.waiters++
		m.mu.Unlock()
		select {
		case <-call.done:
			m.waiterDone(call)
			return call.applied, call.err
		case <-ctx.Done():
			m.waiterDone(call)
			return false, ctx.Err()
		}
	}
	if !m.hasCredentials {
		m.mu.Unlock()
		return true, ErrOAuthRequired
	}
	credentials := m.credentials
	generation := m.generation
	if !force && !needsRefresh(credentials, m.now(), ahead) {
		m.mu.Unlock()
		return true, nil
	}
	if !m.now().Before(credentials.RefreshTokenExpiresAt) {
		m.mu.Unlock()
		if !m.markOAuthRequired(generation) {
			return false, nil
		}
		return true, ErrOAuthRequired
	}
	if m.refresh == nil {
		m.mu.Unlock()
		return true, fmt.Errorf("refresh credentials: callback is nil")
	}
	call := &refreshCall{done: make(chan struct{}), generation: generation}
	m.inFlight = call
	m.mu.Unlock()

	fresh, refreshErr := m.refresh(ctx, credentials)
	applied, err := m.commitRefresh(call.generation, fresh, refreshErr)
	m.mu.Lock()
	call.applied = applied
	call.err = err
	if m.inFlight == call {
		m.inFlight = nil
	}
	close(call.done)
	m.mu.Unlock()
	return applied, err
}

func (m *Manager) commitRefresh(generation uint64, fresh store.Credentials, refreshErr error) (bool, error) {
	<-m.mutationGate
	defer func() { m.mutationGate <- struct{}{} }()
	m.mu.Lock()
	current := m.generation == generation
	m.mu.Unlock()
	if !current {
		return false, nil
	}
	if refreshErr != nil {
		err := fmt.Errorf("refresh credentials: %w", refreshErr)
		if errors.Is(refreshErr, ErrOAuthRequired) {
			m.mu.Lock()
			if m.generation == generation {
				m.hasCredentials = false
				m.generation++
			}
			m.mu.Unlock()
		}
		return true, err
	}
	if err := validate(fresh); err != nil {
		return true, fmt.Errorf("refresh credentials: %w", err)
	}
	if m.save == nil {
		return true, fmt.Errorf("save refreshed credentials: callback is nil")
	}
	if err := m.save(fresh); err != nil {
		return true, fmt.Errorf("save refreshed credentials: %w", err)
	}
	m.mu.Lock()
	if m.generation != generation {
		m.mu.Unlock()
		return false, nil
	}
	m.credentials = fresh
	m.hasCredentials = true
	m.generation++
	m.mu.Unlock()
	return true, nil
}

func (m *Manager) waiterDone(call *refreshCall) {
	m.mu.Lock()
	call.waiters--
	m.mu.Unlock()
}

func (m *Manager) markOAuthRequired(generation uint64) bool {
	<-m.mutationGate
	defer func() { m.mutationGate <- struct{}{} }()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != generation {
		return false
	}
	m.hasCredentials = false
	m.generation++
	return true
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
		_ = m.ensureRefresh(ctx, m.refreshAhead, false)
	}
}

func (m *Manager) snapshot() (store.Credentials, bool) {
	credentials, ok, _ := m.snapshotVersion()
	return credentials, ok
}

func (m *Manager) snapshotVersion() (store.Credentials, bool, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.credentials, m.hasCredentials, m.generation
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
