package token

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zm/ewelink-lan-ctl/internal/store"
)

var testNow = time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)

func credentials(accessExpiry, refreshExpiry time.Time) store.Credentials {
	return store.Credentials{
		Region: "us", AccessToken: "access", AccessTokenExpiresAt: accessExpiry,
		RefreshToken: "refresh", RefreshTokenExpiresAt: refreshExpiry,
	}
}

func newTestManager(load func() (store.Credentials, error), save func(store.Credentials) error, refresh RefreshFunc, now func() time.Time) *Manager {
	if save == nil {
		save = func(store.Credentials) error { return nil }
	}
	if refresh == nil {
		refresh = func(context.Context, store.Credentials) (store.Credentials, error) {
			return store.Credentials{}, errors.New("unexpected refresh")
		}
	}
	return New(load, save, refresh, now, 7*24*time.Hour)
}

func TestLoadMissingStateIsUnauthenticated(t *testing.T) {
	m := newTestManager(func() (store.Credentials, error) { return store.Credentials{}, os.ErrNotExist }, nil, nil, func() time.Time { return testNow })
	if err := m.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if m.Health() {
		t.Fatal("Health() = true, want false")
	}
	_, _, err := m.Access(context.Background())
	if !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("Access() error = %v, want ErrOAuthRequired", err)
	}
}

func TestLoadRejectsMalformedState(t *testing.T) {
	m := newTestManager(func() (store.Credentials, error) { return store.Credentials{Region: "us"}, nil }, nil, nil, func() time.Time { return testNow })
	if err := m.Load(context.Background()); err == nil {
		t.Fatal("Load() error = nil, want malformed credentials error")
	}
}

func TestAccessReturnsValidToken(t *testing.T) {
	stored := credentials(testNow.Add(8*24*time.Hour), testNow.Add(30*24*time.Hour))
	m := newTestManager(func() (store.Credentials, error) { return stored, nil }, nil, nil, func() time.Time { return testNow })
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	region, access, err := m.Access(context.Background())
	if err != nil || region != "us" || access != "access" {
		t.Fatalf("Access() = %q, %q, %v", region, access, err)
	}
	if !m.Health() {
		t.Fatal("Health() = false, want true")
	}
}

func TestLoadRefreshesInsideStartupWindow(t *testing.T) {
	old := credentials(testNow.Add(6*24*time.Hour), testNow.Add(30*24*time.Hour))
	fresh := credentials(testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	called := 0
	m := newTestManager(func() (store.Credentials, error) { return old, nil }, nil,
		func(context.Context, store.Credentials) (store.Credentials, error) { called++; return fresh, nil }, func() time.Time { return testNow })
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("refresh calls = %d, want 1", called)
	}
}

func TestAccessRefreshesInsideFiveMinuteWindow(t *testing.T) {
	old := credentials(testNow.Add(4*time.Minute), testNow.Add(30*24*time.Hour))
	fresh := credentials(testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	m := newTestManager(func() (store.Credentials, error) { return old, nil }, nil,
		func(context.Context, store.Credentials) (store.Credentials, error) { return fresh, nil }, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}
	_, token, err := m.Access(context.Background())
	if err != nil || token != fresh.AccessToken {
		t.Fatalf("Access() token = %q, error = %v", token, err)
	}
}

func TestConcurrentAccessSharesRefreshResult(t *testing.T) {
	old := credentials(testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	fresh := credentials(testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	m := newTestManager(nil, nil, func(context.Context, store.Credentials) (store.Credentials, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return fresh, nil
	}, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}

	const count = 16
	results := make(chan error, count)
	var ready sync.WaitGroup
	ready.Add(count)
	gate := make(chan struct{})
	for i := 0; i < count; i++ {
		go func() {
			ready.Done()
			<-gate
			_, token, err := m.Access(context.Background())
			if err == nil && token != fresh.AccessToken {
				err = errors.New("caller received stale token")
			}
			results <- err
		}()
	}
	ready.Wait()
	close(gate)
	<-started
	close(release)
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Fatalf("caller error = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestFailedRefreshPreservesOldState(t *testing.T) {
	old := credentials(testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	wantErr := errors.New("refresh failed")
	m := newTestManager(nil, nil, func(context.Context, store.Credentials) (store.Credentials, error) {
		return store.Credentials{}, wantErr
	}, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}
	if err := m.ForceRefresh(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("ForceRefresh() error = %v", err)
	}
	if got := m.current(); got != old {
		t.Fatalf("state changed after failed refresh: %#v", got)
	}
}

func TestRefreshPublishesOnlyAfterSuccessfulSave(t *testing.T) {
	old := credentials(testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	fresh := credentials(testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var saves int
	m := newTestManager(nil, func(got store.Credentials) error {
		saves++
		if saves == 1 {
			return nil
		}
		close(saveStarted)
		<-releaseSave
		return nil
	},
		func(context.Context, store.Credentials) (store.Credentials, error) { return fresh, nil }, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.ForceRefresh(context.Background()) }()
	<-saveStarted
	if got := m.current(); got != old {
		t.Fatalf("state published before save: %#v", got)
	}
	close(releaseSave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := m.current(); got != fresh {
		t.Fatalf("state = %#v, want fresh", got)
	}
}

func TestSaveFailurePreservesOldState(t *testing.T) {
	old := credentials(testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	fresh := credentials(testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	wantErr := errors.New("disk full")
	m := newTestManager(nil, func(store.Credentials) error { return wantErr }, func(context.Context, store.Credentials) (store.Credentials, error) { return fresh, nil }, func() time.Time { return testNow })
	if err := m.Set(old); !errors.Is(err, wantErr) {
		t.Fatalf("Set() error = %v", err)
	}
	// Seed via a manager whose initial Set can succeed, then make the save callback fail.
	m = newTestManager(nil, func(store.Credentials) error { return wantErr }, func(context.Context, store.Credentials) (store.Credentials, error) { return fresh, nil }, func() time.Time { return testNow })
	m.setCurrentForTest(old)
	if err := m.ForceRefresh(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("ForceRefresh() error = %v", err)
	}
	if got := m.current(); got != old {
		t.Fatalf("state changed after save failure: %#v", got)
	}
}

func TestExpiredRefreshTokenRequiresOAuth(t *testing.T) {
	old := credentials(testNow.Add(-time.Minute), testNow)
	m := newTestManager(nil, nil, nil, func() time.Time { return testNow })
	m.setCurrentForTest(old)
	if err := m.ForceRefresh(context.Background()); !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("ForceRefresh() error = %v", err)
	}
}

func TestHealthReportsExpiredAccessAsRefreshable(t *testing.T) {
	old := credentials(testNow.Add(-time.Minute), testNow.Add(24*time.Hour))
	m := newTestManager(nil, nil, nil, func() time.Time { return testNow })
	m.setCurrentForTest(old)
	if !m.Health() {
		t.Fatal("Health() = false, want true for refreshable credentials")
	}
}

func TestRunChecksImmediatelyAndStopsOnCancellation(t *testing.T) {
	now := testNow
	old := credentials(testNow.Add(8*24*time.Hour), testNow.Add(30*24*time.Hour))
	refreshed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	m := newTestManager(nil, nil, func(context.Context, store.Credentials) (store.Credentials, error) {
		close(refreshed)
		cancel()
		return credentials(now.Add(30*24*time.Hour), now.Add(60*24*time.Hour)), nil
	}, func() time.Time { return now })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}
	now = testNow.Add(2 * 24 * time.Hour)
	done := make(chan struct{})
	go func() { m.Run(ctx, time.Hour); close(done) }()
	<-refreshed
	<-done
}

func (m *Manager) current() store.Credentials {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.credentials
}

func (m *Manager) setCurrentForTest(credentials store.Credentials) {
	m.mu.Lock()
	m.credentials = credentials
	m.hasCredentials = true
	m.mu.Unlock()
}
