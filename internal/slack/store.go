package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// TokenStore keeps the token in a JSON file only the user can read.
type TokenStore struct{ Path string }

// DefaultTokenPath returns $XDG_STATE_HOME/dev-inbox/slack-token.json, or
// ~/.local/state/dev-inbox/slack-token.json.
func DefaultTokenPath() (string, error) {
	// The XDG spec says to ignore relative paths.
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "dev-inbox", "slack-token.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("slack: resolve token path: %w", err)
	}
	return filepath.Join(home, ".local", "state", "dev-inbox", "slack-token.json"), nil
}

// Load returns ErrNoToken if the file does not exist.
func (s TokenStore) Load() (Token, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Token{}, ErrNoToken
	}
	if err != nil {
		return Token{}, fmt.Errorf("slack: read token: %w", err)
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return Token{}, fmt.Errorf("slack: parse token file %s: %w", s.Path, err)
	}
	if t.AccessToken == "" {
		return Token{}, ErrNoToken
	}
	return t, nil
}

// Save writes atomically with mode 0600.
func (s TokenStore) Save(t Token) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("slack: encode token: %w", err)
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("slack: create token directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".slack-token-*.tmp")
	if err != nil {
		return fmt.Errorf("slack: save token: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after the rename
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("slack: save token: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("slack: save token: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("slack: save token: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("slack: save token: %w", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return fmt.Errorf("slack: save token: %w", err)
	}
	return nil
}
