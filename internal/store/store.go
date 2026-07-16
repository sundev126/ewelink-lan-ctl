package store

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Credentials struct {
	Region                string    `json:"region"`
	AccessToken           string    `json:"access_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshToken          string    `json:"refresh_token"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
}

type File struct {
	Path string
}

func (f File) Load() (Credentials, error) {
	file, err := os.Open(f.Path)
	if err != nil {
		return Credentials{}, fmt.Errorf("open credentials: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	var credentials Credentials
	if err := decoder.Decode(&credentials); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Credentials{}, fmt.Errorf("decode credentials: unexpected trailing JSON value")
		}
		return Credentials{}, fmt.Errorf("decode credentials trailing data: %w", err)
	}
	credentials.AccessTokenExpiresAt = credentials.AccessTokenExpiresAt.UTC()
	credentials.RefreshTokenExpiresAt = credentials.RefreshTokenExpiresAt.UTC()
	return credentials, nil
}

func (f File) Save(credentials Credentials) error {
	directory := filepath.Dir(f.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("set credentials directory permissions: %w", err)
	}

	temporary, err := os.CreateTemp(directory, "."+filepath.Base(f.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary credentials file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary credentials permissions: %w", err)
	}
	credentials.AccessTokenExpiresAt = credentials.AccessTokenExpiresAt.UTC()
	credentials.RefreshTokenExpiresAt = credentials.RefreshTokenExpiresAt.UTC()
	if err := json.NewEncoder(temporary).Encode(credentials); err != nil {
		temporary.Close()
		return fmt.Errorf("encode credentials: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary credentials: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary credentials: %w", err)
	}
	if err := os.Rename(temporaryPath, f.Path); err != nil {
		return fmt.Errorf("replace credentials: %w", err)
	}

	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open credentials directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return fmt.Errorf("sync credentials directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close credentials directory: %w", err)
	}
	return nil
}
