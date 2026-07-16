package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCredentials() Credentials {
	return Credentials{
		Region:                "cn",
		AccessToken:           "access-token",
		AccessTokenExpiresAt:  time.Date(2026, 7, 16, 12, 30, 0, 0, time.FixedZone("CST", 8*60*60)),
		RefreshToken:          "refresh-token",
		RefreshTokenExpiresAt: time.Date(2026, 8, 16, 12, 30, 0, 0, time.FixedZone("CST", 8*60*60)),
	}
}

func TestFileRoundTrip(t *testing.T) {
	file := File{Path: filepath.Join(t.TempDir(), "state.json")}
	want := testCredentials()

	if err := file.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := file.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Region != want.Region || got.AccessToken != want.AccessToken ||
		!got.AccessTokenExpiresAt.Equal(want.AccessTokenExpiresAt) ||
		got.RefreshToken != want.RefreshToken ||
		!got.RefreshTokenExpiresAt.Equal(want.RefreshTokenExpiresAt) {
		t.Fatalf("Load() = %#v, want equivalent to %#v", got, want)
	}
}

func TestFileSaveUsesUTCTimestamps(t *testing.T) {
	file := File{Path: filepath.Join(t.TempDir(), "state.json")}

	if err := file.Save(testCredentials()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	data, err := os.ReadFile(file.Path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(data), "+08:00") || strings.Count(string(data), "Z") != 2 {
		t.Fatalf("saved timestamps are not UTC: %s", data)
	}
	got, err := file.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.AccessTokenExpiresAt.Location() != time.UTC || got.RefreshTokenExpiresAt.Location() != time.UTC {
		t.Fatalf("Load() timestamp locations = %v, %v; want UTC", got.AccessTokenExpiresAt.Location(), got.RefreshTokenExpiresAt.Location())
	}
}

func TestFileSaveUsesPrivateModes(t *testing.T) {
	root := t.TempDir()
	file := File{Path: filepath.Join(root, "nested", "state.json")}

	if err := file.Save(testCredentials()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	fileInfo, err := os.Stat(file.Path)
	if err != nil {
		t.Fatalf("Stat(file) error = %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(file.Path))
	if err != nil {
		t.Fatalf("Stat(parent) error = %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode = %04o, want 0700", got)
	}
}

func TestFileLoadMissingReturnsNotExist(t *testing.T) {
	file := File{Path: filepath.Join(t.TempDir(), "missing.json")}

	_, err := file.Load()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() error = %v, want os.ErrNotExist", err)
	}
}

func TestFileLoadRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := (File{Path: path}).Load(); err == nil {
		t.Fatal("Load() error = nil, want malformed JSON error")
	}
}

func TestInterruptedSavePreservesPreviousState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	file := File{Path: filepath.Join(dir, "state.json")}
	previous := testCredentials()
	if err := file.Save(previous); err != nil {
		t.Fatalf("initial Save() error = %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	updated := previous
	updated.AccessToken = "replacement-token"
	if err := file.Save(updated); err == nil {
		t.Fatal("interrupted Save() error = nil, want error")
	}

	got, err := file.Load()
	if err != nil {
		t.Fatalf("Load() after interrupted save error = %v", err)
	}
	if got.AccessToken != previous.AccessToken {
		t.Fatalf("access token after interrupted save = %q, want %q", got.AccessToken, previous.AccessToken)
	}
}
