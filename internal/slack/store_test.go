package slack

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "dev-inbox")
	store := TokenStore{Path: filepath.Join(dir, "slack-token.json")}
	want := Token{
		AccessToken:  "xoxe.xoxp-1-access-1",
		RefreshToken: "xoxe-1-refresh-1",
		ExpiresAt:    time.Date(2026, 10, 13, 20, 0, 0, 123, time.UTC),
		Scope:        "channels:history,users:read",
	}
	for i := range 2 { // the second save overwrites
		want.AccessToken += "x"
		if err := store.Save(want); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.Scope != want.Scope || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("Load = %+v, want %+v", got, want)
	}

	for _, tt := range []struct {
		path string
		mode os.FileMode
	}{{store.Path, 0o600}, {dir, 0o700}} {
		fi, err := os.Stat(tt.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != tt.mode {
			t.Errorf("%s mode = %v, want %v", tt.path, fi.Mode().Perm(), tt.mode)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("token directory holds %d entries, want only the token file", len(entries))
	}
}

func TestTokenStoreLoadErrors(t *testing.T) {
	tests := []struct {
		name      string
		content   *string
		wantNoTok bool
	}{
		{name: "missing file", wantNoTok: true},
		{name: "no access token", content: new(`{"refresh_token":"xoxe-1-refresh-1"}`), wantNoTok: true},
		{name: "corrupt", content: new(`{not json`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := TokenStore{Path: filepath.Join(t.TempDir(), "slack-token.json")}
			if tt.content != nil {
				if err := os.WriteFile(store.Path, []byte(*tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := store.Load()
			if err == nil {
				t.Fatal("Load succeeded")
			}
			if errors.Is(err, ErrNoToken) != tt.wantNoTok {
				t.Errorf("Load error = %v, want ErrNoToken %v", err, tt.wantNoTok)
			}
		})
	}
}

func TestNewWithoutToken(t *testing.T) {
	_, err := New(Config{Store: TokenStore{Path: filepath.Join(t.TempDir(), "slack-token.json")}})
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("New error = %v, want ErrNoToken", err)
	}
}

func TestDefaultTokenPath(t *testing.T) {
	tests := []struct {
		name string
		xdg  string
		want string
	}{
		{name: "XDG_STATE_HOME", xdg: "/xdg/state", want: "/xdg/state/dev-inbox/slack-token.json"},
		{name: "unset", want: "/home/test/.local/state/dev-inbox/slack-token.json"},
		{name: "relative is ignored", xdg: "state", want: "/home/test/.local/state/dev-inbox/slack-token.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", "/home/test")
			t.Setenv("XDG_STATE_HOME", tt.xdg)
			got, err := DefaultTokenPath()
			if err != nil || got != tt.want {
				t.Errorf("DefaultTokenPath = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
