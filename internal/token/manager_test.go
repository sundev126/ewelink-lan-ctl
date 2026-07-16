package token

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
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
	waitForRefreshWaiters(t, m, count-1)
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

func TestAccessRequiresOAuthWhenRefreshExpiredBeforeAccess(t *testing.T) {
	old := credentials(testNow.Add(24*time.Hour), testNow.Add(-time.Minute))
	var refreshCalls int
	m := newTestManager(nil, nil, func(context.Context, store.Credentials) (store.Credentials, error) {
		refreshCalls++
		return store.Credentials{}, errors.New("unexpected refresh")
	}, func() time.Time { return testNow })
	m.setCurrentForTest(old)

	_, _, err := m.Access(context.Background())
	if !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("Access() error = %v, want ErrOAuthRequired", err)
	}
	if refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want 0", refreshCalls)
	}
}

func TestPermanentRefreshRejectionMarksOAuthRequiredWithoutSaving(t *testing.T) {
	old := credentials(testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	var saved []store.Credentials
	m := newTestManager(nil, func(got store.Credentials) error {
		saved = append(saved, got)
		return nil
	}, func(context.Context, store.Credentials) (store.Credentials, error) {
		return store.Credentials{}, fmt.Errorf("provider rejected refresh token: %w", ErrOAuthRequired)
	}, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}

	if err := m.ForceRefresh(context.Background()); !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("ForceRefresh() error = %v, want ErrOAuthRequired", err)
	}
	if m.Health() {
		t.Fatal("Health() = true after permanent rejection")
	}
	if _, _, err := m.Access(context.Background()); !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("subsequent Access() error = %v, want ErrOAuthRequired", err)
	}
	if len(saved) != 1 || saved[0] != old {
		t.Fatalf("durable saves = %#v, want only original credentials", saved)
	}
	if got := m.current(); got != old {
		t.Fatalf("retained credentials = %#v, want %#v", got, old)
	}
}

func TestSetSupersedesBlockedLoad(t *testing.T) {
	loaded := namedCredentials("loaded", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	newer := namedCredentials("newer", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	loadStarted := make(chan struct{})
	releaseLoad := make(chan struct{})
	saveStarted := make(chan struct{})
	m := newTestManager(func() (store.Credentials, error) {
		close(loadStarted)
		<-releaseLoad
		return loaded, nil
	}, func(store.Credentials) error {
		close(saveStarted)
		return nil
	}, nil, func() time.Time { return testNow })

	loadDone := make(chan error, 1)
	go func() { loadDone <- m.Load(context.Background()) }()
	<-loadStarted
	setDone := make(chan error, 1)
	go func() { setDone <- m.Set(newer) }()
	<-saveStarted
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
	close(releaseLoad)
	if err := <-loadDone; err != nil {
		t.Fatal(err)
	}
	region, token, err := m.Access(context.Background())
	if err != nil || region != newer.Region || token != newer.AccessToken {
		t.Fatalf("Access() = %q, %q, %v; want newer credentials", region, token, err)
	}
}

func TestLoadDuringBlockedSetCannotPublishStaleCredentials(t *testing.T) {
	old := namedCredentials("old", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	newer := namedCredentials("newer", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var saves int
	m := newTestManager(func() (store.Credentials, error) { return old, nil }, func(store.Credentials) error {
		saves++
		if saves == 1 {
			return nil
		}
		close(saveStarted)
		<-releaseSave
		return nil
	}, nil, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}

	setDone := make(chan error, 1)
	go func() { setDone <- m.Set(newer) }()
	<-saveStarted
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(releaseSave)
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
	region, token, err := m.Access(context.Background())
	if err != nil || region != newer.Region || token != newer.AccessToken {
		t.Fatalf("Access() = %q, %q, %v; want newer credentials", region, token, err)
	}
}

func TestSetSupersedesBlockedRefreshWithoutPersistingStaleResult(t *testing.T) {
	old := namedCredentials("old", testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	newer := namedCredentials("newer", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	refreshed := namedCredentials("refreshed-old", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var saveMu sync.Mutex
	var saved []store.Credentials
	m := newTestManager(nil, func(got store.Credentials) error {
		saveMu.Lock()
		saved = append(saved, got)
		saveMu.Unlock()
		return nil
	}, func(context.Context, store.Credentials) (store.Credentials, error) {
		close(refreshStarted)
		<-releaseRefresh
		return refreshed, nil
	}, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- m.ForceRefresh(context.Background()) }()
	<-refreshStarted
	setDone := make(chan error, 1)
	go func() { setDone <- m.Set(newer) }()
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
	close(releaseRefresh)
	if err := <-refreshDone; err != nil {
		t.Fatalf("superseded ForceRefresh() error = %v", err)
	}

	saveMu.Lock()
	gotSaved := append([]store.Credentials(nil), saved...)
	saveMu.Unlock()
	if len(gotSaved) != 2 || gotSaved[0] != old || gotSaved[1] != newer {
		t.Fatalf("durable saves = %#v, want old then newer only", gotSaved)
	}
	if got := m.current(); got != newer {
		t.Fatalf("current credentials = %#v, want newer", got)
	}
}

func TestBlockedRefreshWaitersShareErrorAndCanceledWaiterCanLeave(t *testing.T) {
	old := credentials(testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	wantErr := errors.New("temporary refresh failure")
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var calls atomic.Int32
	m := newTestManager(nil, nil, func(context.Context, store.Credentials) (store.Credentials, error) {
		calls.Add(1)
		close(refreshStarted)
		<-releaseRefresh
		return store.Credentials{}, wantErr
	}, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}

	const waiters = 4
	results := make(chan error, waiters+1)
	go func() { _, _, err := m.Access(context.Background()); results <- err }()
	<-refreshStarted
	for i := 0; i < waiters; i++ {
		go func() { _, _, err := m.Access(context.Background()); results <- err }()
	}
	waitForRefreshWaiters(t, m, waiters)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := m.Access(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls with canceled waiter = %d, want 1", calls.Load())
	}

	close(releaseRefresh)
	for i := 0; i < waiters+1; i++ {
		if err := <-results; !errors.Is(err, wantErr) {
			t.Fatalf("shared refresh error = %v, want %v", err, wantErr)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
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

func namedCredentials(name string, accessExpiry, refreshExpiry time.Time) store.Credentials {
	credentials := credentials(accessExpiry, refreshExpiry)
	credentials.Region = name + "-region"
	credentials.AccessToken = name + "-access"
	credentials.RefreshToken = name + "-refresh"
	return credentials
}

func waitForRefreshWaiters(t *testing.T, m *Manager, want int) {
	t.Helper()
	for i := 0; i < 10000; i++ {
		m.mu.Lock()
		got := 0
		if m.inFlight != nil {
			got = m.inFlight.waiters
		}
		m.mu.Unlock()
		if got >= want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("in-flight refresh waiters did not reach %d", want)
}
