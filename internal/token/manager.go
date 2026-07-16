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
	pendingSet     uint64
	inFlight       *refreshCall
	commitGate     chan struct{}
}

func New(load func() (store.Credentials, error), save func(store.Credentials) error, refresh RefreshFunc, now func() time.Time, refreshAhead time.Duration) *Manager {
	manager := &Manager{
		load: load, save: save, refresh: refresh, now: now, refreshAhead: refreshAhead,
		commitGate: make(chan struct{}, 1),
	}
	manager.commitGate <- struct{}{}
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
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.mu.Lock()
			if m.generation == generation && m.pendingSet == 0 {
				m.credentials = store.Credentials{}
				m.hasCredentials = false
				m.generation++
			}
			m.mu.Unlock()
			return nil
		}
		return fmt.Errorf("load credentials: %w", err)
	}
	if err := validate(credentials); err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	m.mu.Lock()
	published := m.generation == generation && m.pendingSet == 0
	if published {
		m.credentials = credentials
		m.hasCredentials = true
		m.generation++
	}
	m.mu.Unlock()
	if !published {
		return nil
	}
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
	m.mu.Lock()
	m.generation++
	generation := m.generation
	m.pendingSet = generation
	m.mu.Unlock()

	<-m.commitGate
	m.mu.Lock()
	current := m.generation == generation
	m.mu.Unlock()
	if !current {
		m.commitGate <- struct{}{}
		return nil
	}
	err := m.save(credentials)
	m.mu.Lock()
	if m.generation == generation {
		if err == nil {
			m.credentials = credentials
			m.hasCredentials = true
			m.inFlight = nil
		}
		m.pendingSet = 0
		m.generation++
	}
	m.mu.Unlock()
	m.commitGate <- struct{}{}
	if err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

func (m *Manager) Access(ctx context.Context) (region, token string, err error) {
	credentials, ok, generation := m.snapshotVersion()
	if !ok {
		return "", "", ErrOAuthRequired
	}
	if !m.now().Before(credentials.RefreshTokenExpiresAt) {
		m.markOAuthRequired(generation)
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
		call.waiters++
		m.mu.Unlock()
		select {
		case <-call.done:
			m.waiterDone(call)
			return call.err
		case <-ctx.Done():
			m.waiterDone(call)
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
		m.hasCredentials = false
		m.generation++
		m.mu.Unlock()
		return ErrOAuthRequired
	}
	call := &refreshCall{done: make(chan struct{}), generation: m.generation}
	m.inFlight = call
	m.mu.Unlock()

	err := m.performRefresh(ctx, credentials, call.generation)
	m.mu.Lock()
	if errors.Is(err, ErrOAuthRequired) && m.generation == call.generation {
		m.hasCredentials = false
		m.generation++
	}
	call.err = err
	if m.inFlight == call {
		m.inFlight = nil
	}
	close(call.done)
	m.mu.Unlock()
	return err
}

func (m *Manager) waiterDone(call *refreshCall) {
	m.mu.Lock()
	call.waiters--
	m.mu.Unlock()
}

func (m *Manager) markOAuthRequired(generation uint64) {
	m.mu.Lock()
	if m.generation == generation {
		m.hasCredentials = false
		m.generation++
	}
	m.mu.Unlock()
}

func (m *Manager) performRefresh(ctx context.Context, credentials store.Credentials, generation uint64) error {
	if m.refresh == nil {
		return fmt.Errorf("refresh credentials: callback is nil")
	}
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
	<-m.commitGate
	defer func() { m.commitGate <- struct{}{} }()
	m.mu.Lock()
	current := m.generation == generation
	m.mu.Unlock()
	if !current {
		return nil
	}
	if err := m.save(fresh); err != nil {
		return fmt.Errorf("save refreshed credentials: %w", err)
	}
	m.mu.Lock()
	if m.generation == generation {
		m.credentials = fresh
		m.hasCredentials = true
		m.generation++
	}
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
