package token

import (
	"context"
	"errors"
	"fmt"
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
	access := func(ctx context.Context) {
		_, token, err := m.Access(ctx)
		if err == nil && token != fresh.AccessToken {
			err = errors.New("caller received stale token")
		}
		results <- err
	}
	go access(context.Background())
	<-started
	for i := 0; i < count-1; i++ {
		ctx := newObservedContext()
		go access(ctx)
		<-ctx.entered
	}
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
	if err := m.ForceRefresh(context.Background()); !errors.Is(err, wantErr) || !errors.Is(err, ErrRefreshUnavailable) {
		t.Fatalf("ForceRefresh() error = %v, want both persistence cause and ErrRefreshUnavailable", err)
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
	loadReturned := make(chan struct{})
	var saves int
	m := newTestManager(func() (store.Credentials, error) {
		close(loadReturned)
		return old, nil
	}, func(store.Credentials) error {
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
	loadDone := make(chan error, 1)
	go func() { loadDone <- m.Load(context.Background()) }()
	<-loadReturned
	select {
	case err := <-loadDone:
		t.Fatalf("Load completed while Set save was blocked: %v", err)
	default:
	}
	close(releaseSave)
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
	if err := <-loadDone; err != nil {
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
		ctx := newObservedContext()
		go func() { _, _, err := m.Access(ctx); results <- err }()
		<-ctx.entered
	}

	canceled := newObservedContext()
	canceled.cancel()
	if _, _, err := m.Access(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}
	<-canceled.entered
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

func TestConcurrentSetsPublishInSaveOrder(t *testing.T) {
	first := namedCredentials("first", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	second := namedCredentials("second", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	firstSaveStarted := make(chan struct{})
	releaseFirstSave := make(chan struct{})
	secondInvoked := make(chan struct{})
	secondSaveStarted := make(chan struct{})
	secondSawFirst := make(chan bool, 1)
	var m *Manager
	m = newTestManager(nil, func(got store.Credentials) error {
		switch got {
		case first:
			close(firstSaveStarted)
			<-releaseFirstSave
		case second:
			close(secondSaveStarted)
			secondSawFirst <- m.current() == first
		}
		return nil
	}, nil, func() time.Time { return testNow })

	firstDone := make(chan error, 1)
	go func() { firstDone <- m.Set(first) }()
	<-firstSaveStarted
	secondDone := make(chan error, 1)
	go func() { close(secondInvoked); secondDone <- m.Set(second) }()
	<-secondInvoked
	select {
	case <-secondSaveStarted:
		t.Fatal("second Set save entered while first Set save was blocked")
	default:
	}
	close(releaseFirstSave)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if !<-secondSawFirst {
		t.Fatal("second Set began saving before first Set was published")
	}
	if got := m.current(); got != second {
		t.Fatalf("current credentials = %#v, want second Set", got)
	}
}

func TestFailedConcurrentLaterSetLeavesEarlierSetPublished(t *testing.T) {
	first := namedCredentials("first", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	second := namedCredentials("second", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	wantErr := errors.New("save second")
	firstSaveStarted := make(chan struct{})
	releaseFirstSave := make(chan struct{})
	secondInvoked := make(chan struct{})
	secondSaveStarted := make(chan struct{})
	var durable []store.Credentials
	var durableMu sync.Mutex
	m := newTestManager(nil, func(got store.Credentials) error {
		if got == first {
			close(firstSaveStarted)
			<-releaseFirstSave
			durableMu.Lock()
			durable = append(durable, got)
			durableMu.Unlock()
			return nil
		}
		close(secondSaveStarted)
		return wantErr
	}, nil, func() time.Time { return testNow })

	firstDone := make(chan error, 1)
	go func() { firstDone <- m.Set(first) }()
	<-firstSaveStarted
	secondDone := make(chan error, 1)
	go func() { close(secondInvoked); secondDone <- m.Set(second) }()
	<-secondInvoked
	select {
	case <-secondSaveStarted:
		t.Fatal("failed second Set save entered while first Set save was blocked")
	default:
	}
	close(releaseFirstSave)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; !errors.Is(err, wantErr) {
		t.Fatalf("second Set error = %v, want %v", err, wantErr)
	}
	if got := m.current(); got != first {
		t.Fatalf("current credentials = %#v, want first Set", got)
	}
	durableMu.Lock()
	defer durableMu.Unlock()
	if len(durable) != 1 || durable[0] != first {
		t.Fatalf("durable credentials = %#v, want first Set only", durable)
	}
}

func TestRefreshResponseWaitsBehindBlockedSetSaveAndBecomesSuperseded(t *testing.T) {
	old := namedCredentials("old", testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	newer := namedCredentials("newer", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	staleRefresh := namedCredentials("stale-refresh", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	setSaveStarted := make(chan struct{})
	releaseSetSave := make(chan struct{})
	refreshReturned := make(chan struct{})
	var saved []store.Credentials
	var saveMu sync.Mutex
	m := newTestManager(nil, func(got store.Credentials) error {
		if got == newer {
			close(setSaveStarted)
			<-releaseSetSave
		}
		saveMu.Lock()
		saved = append(saved, got)
		saveMu.Unlock()
		return nil
	}, func(context.Context, store.Credentials) (store.Credentials, error) {
		close(refreshReturned)
		return staleRefresh, nil
	}, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}
	setDone := make(chan error, 1)
	go func() { setDone <- m.Set(newer) }()
	<-setSaveStarted
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- m.ForceRefresh(context.Background()) }()
	<-refreshReturned
	select {
	case err := <-refreshDone:
		t.Fatalf("refresh completed while Set save was blocked: %v", err)
	default:
	}
	close(releaseSetSave)
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	if len(saved) != 2 || saved[0] != old || saved[1] != newer {
		t.Fatalf("saves = %#v, want old then newer", saved)
	}
}

func TestSetWaitsBehindBlockedRefreshSave(t *testing.T) {
	old := namedCredentials("old", testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	fresh := namedCredentials("fresh", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	newer := namedCredentials("newer", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	refreshSaveStarted := make(chan struct{})
	releaseRefreshSave := make(chan struct{})
	setInvoked := make(chan struct{})
	setSaveStarted := make(chan struct{})
	setSawFresh := make(chan bool, 1)
	var m *Manager
	m = newTestManager(nil, func(got store.Credentials) error {
		switch got {
		case fresh:
			close(refreshSaveStarted)
			<-releaseRefreshSave
		case newer:
			setSawFresh <- m.current() == fresh
			close(setSaveStarted)
		}
		return nil
	}, func(context.Context, store.Credentials) (store.Credentials, error) { return fresh, nil }, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- m.ForceRefresh(context.Background()) }()
	<-refreshSaveStarted
	setDone := make(chan error, 1)
	go func() { close(setInvoked); setDone <- m.Set(newer) }()
	<-setInvoked
	select {
	case <-setSaveStarted:
		t.Fatal("Set save started while refresh save was blocked")
	default:
	}
	close(releaseRefreshSave)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
	if !<-setSawFresh {
		t.Fatal("Set save began before refreshed credentials were published")
	}
}

func TestSetGenerationChangesOnlyAfterSuccessfulSave(t *testing.T) {
	newer := namedCredentials("newer", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	m := newTestManager(nil, func(store.Credentials) error {
		close(saveStarted)
		<-releaseSave
		return nil
	}, nil, func() time.Time { return testNow })
	before := m.currentGeneration()
	done := make(chan error, 1)
	go func() { done <- m.Set(newer) }()
	<-saveStarted
	if got := m.currentGeneration(); got != before {
		t.Fatalf("generation changed during save: got %d, want %d", got, before)
	}
	close(releaseSave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := m.currentGeneration(); got != before+1 {
		t.Fatalf("generation after save = %d, want %d", got, before+1)
	}
}

func TestSetSupersedingRefreshNeverAllowsTwoActiveRefreshes(t *testing.T) {
	old := namedCredentials("old", testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	newer := namedCredentials("newer", testNow.Add(time.Minute), testNow.Add(30*24*time.Hour))
	staleFresh := namedCredentials("stale-fresh", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	newFresh := namedCredentials("new-fresh", testNow.Add(30*24*time.Hour), testNow.Add(60*24*time.Hour))
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	m := newTestManager(nil, nil, func(context.Context, store.Credentials) (store.Credentials, error) {
		call := calls.Add(1)
		current := active.Add(1)
		for {
			maximum := maxActive.Load()
			if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		defer active.Add(-1)
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
			return staleFresh, nil
		}
		close(secondStarted)
		return newFresh, nil
	}, func() time.Time { return testNow })
	if err := m.Set(old); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- m.ForceRefresh(context.Background()) }()
	<-firstStarted
	if err := m.Set(newer); err != nil {
		t.Fatal(err)
	}
	accessDone := make(chan error, 1)
	accessCtx := newObservedContext()
	go func() {
		_, token, err := m.Access(accessCtx)
		if err == nil && token != newFresh.AccessToken {
			err = fmt.Errorf("access token = %q, want %q", token, newFresh.AccessToken)
		}
		accessDone <- err
	}()
	<-accessCtx.entered
	select {
	case <-secondStarted:
		t.Fatal("second refresh started before first refresh finished")
	default:
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	<-secondStarted
	if err := <-accessDone; err != nil {
		t.Fatal(err)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum active refreshes = %d, want 1", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("refresh calls = %d, want 2 sequential calls", got)
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

func (m *Manager) currentGeneration() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

func namedCredentials(name string, accessExpiry, refreshExpiry time.Time) store.Credentials {
	credentials := credentials(accessExpiry, refreshExpiry)
	credentials.AccessToken = name + "-access"
	credentials.RefreshToken = name + "-refresh"
	return credentials
}

type observedContext struct {
	context.Context
	entered  chan struct{}
	done     chan struct{}
	once     sync.Once
	canceled atomic.Bool
}

func newObservedContext() *observedContext {
	return &observedContext{
		Context: context.Background(),
		entered: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.done
}

func (c *observedContext) Err() error {
	if c.canceled.Load() {
		return context.Canceled
	}
	return c.Context.Err()
}

func (c *observedContext) cancel() {
	c.canceled.Store(true)
	close(c.done)
}
