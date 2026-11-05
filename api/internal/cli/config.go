// Package cli implements the user commands of the open-kase binary
// (login, tickets, settings, logout). They talk to a running
// server over the versioned REST API with the user's own credentials, so
// a user's CLI never needs a MongoDB URI and never bypasses permissions.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/arksnorman/open-kase/api/package/product"
)

// Credential kinds.
const (
	KindSession = "session" // a signed-in user: rotating refresh token
	KindToken   = "token"   // an integration (service principal) token
)

// Credentials are stored in an owner-only file. Session credentials hold
// the refresh and CSRF tokens exactly like the browser's cookies; the
// short-lived access token is cached to avoid a refresh per command.
type Credentials struct {
	URL             string    `json:"url"`
	Kind            string    `json:"kind"`
	Email           string    `json:"email,omitempty"`
	RefreshToken    string    `json:"refreshToken,omitempty"`
	CSRFToken       string    `json:"csrfToken,omitempty"`
	AccessToken     string    `json:"accessToken,omitempty"`
	AccessExpiresAt time.Time `json:"accessExpiresAt,omitempty"`
	Token           string    `json:"token,omitempty"`
	SavedAt         time.Time `json:"savedAt"`
}

// ConfigEnv overrides the credentials file location.
const ConfigEnv = product.EnvPrefix + "CLI_CONFIG"

// DefaultConfigPath returns the credentials file under the user's config
// directory (e.g. ~/.config/open-kase/credentials.json).
func DefaultConfigPath() (string, error) {
	if p := os.Getenv(ConfigEnv); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the user config directory (set %s): %w", ConfigEnv, err)
	}
	return filepath.Join(dir, product.Slug, "credentials.json"), nil
}

// ErrNotLoggedIn means no credentials file exists.
var ErrNotLoggedIn = errors.New("not signed in; run `" + product.Binary + " login --url URL` first")

// LoadCredentials reads the credentials file. It refuses files readable by
// other users.
func LoadCredentials(path string) (Credentials, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Credentials{}, ErrNotLoggedIn
	}
	if err != nil {
		return Credentials{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Credentials{}, fmt.Errorf("%s is accessible by other users (mode %v); run `chmod 600 %s` or log in again", path, info.Mode().Perm(), path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credentials{}, fmt.Errorf("%s is not a valid credentials file: %w", path, err)
	}
	if c.URL == "" || (c.Kind != KindSession && c.Kind != KindToken) {
		return Credentials{}, fmt.Errorf("%s is incomplete; log in again", path)
	}
	return c, nil
}

// SaveCredentials writes the file atomically with mode 0600 (directory 0700).
func SaveCredentials(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	c.SavedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// lockFile serializes refresh-token rotation between concurrent CLI
// processes: presenting the same refresh token twice would be detected as
// reuse and revoke the session. The lock is a file created exclusively;
// a lock older than 30 seconds is considered abandoned.
func lockFile(path string, wait time.Duration) (func(), error) {
	lock := path + ".lock"
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(lock) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > 30*time.Second {
			os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another %s command is refreshing the session (lock %s)", product.Binary, lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
