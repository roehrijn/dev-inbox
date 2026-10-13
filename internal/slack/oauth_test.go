package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// browser follows the authorize URL like Slack would after consent: it redirects to redirect_uri
// with a code and the state, after letting mutate change the callback query.
type browser struct {
	t       *testing.T
	f       *fakeSlack
	mutate  func(q url.Values)
	authURL *url.URL
	status  int
	page    string
}

func (b *browser) open(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		b.t.Fatal(err)
	}
	b.authURL = u
	q := u.Query()
	b.f.mu.Lock()
	b.f.challenge = q.Get("code_challenge")
	b.f.redirectURI = q.Get("redirect_uri")
	b.f.mu.Unlock()

	cb := url.Values{"code": {testCode}, "state": {q.Get("state")}}
	if b.mutate != nil {
		b.mutate(cb)
	}
	resp, err := http.Get(q.Get("redirect_uri") + "?" + cb.Encode())
	if err != nil {
		b.t.Errorf("follow redirect: %v", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	b.status, b.page = resp.StatusCode, string(body)
	return nil
}

func TestLogin(t *testing.T) {
	f := newFake(t)
	cfg := f.oauthConfig()
	cfg.RedirectURL = freeRedirectURL(t)
	b := &browser{t: t, f: f}

	before := time.Now()
	tok, err := Login(t.Context(), cfg, b.open)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	after := time.Now()

	access, refresh, _ := f.tokens()
	if tok.AccessToken != access || tok.RefreshToken != refresh {
		t.Errorf("token = %+v, want access %q refresh %q", tok, access, refresh)
	}
	if tok.ExpiresAt.Before(before.Add(12*time.Hour)) || tok.ExpiresAt.After(after.Add(12*time.Hour)) {
		t.Errorf("ExpiresAt = %v, want about 12h from now", tok.ExpiresAt)
	}
	if b.status != http.StatusOK || !strings.Contains(b.page, "Signed in") {
		t.Errorf("callback page = %d %q, want 200 and a success message", b.status, b.page)
	}

	if b.authURL.Path != authorizePath || "http://"+b.authURL.Host != f.srv.URL {
		t.Errorf("authorize URL = %s, want %s%s", b.authURL, f.srv.URL, authorizePath)
	}
	q := b.authURL.Query()
	for _, tt := range []struct{ param, want string }{
		{"response_type", "code"},
		{"client_id", testClientID},
		{"code_challenge_method", "S256"},
		{"redirect_uri", cfg.RedirectURL},
		{"scope", "channels:history groups:history channels:read groups:read users:read"},
		{"resource", "https://mcp.slack.com"},
	} {
		if got := q.Get(tt.param); got != tt.want {
			t.Errorf("authorize %s = %q, want %q", tt.param, got, tt.want)
		}
	}
	if len(q.Get("state")) < 16 || len(q.Get("code_challenge")) != 43 {
		t.Errorf("state %q or code_challenge %q too short", q.Get("state"), q.Get("code_challenge"))
	}
	if q.Has("client_secret") {
		t.Error("authorize URL carries a client secret")
	}
}

func TestLoginFailures(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(q url.Values)
		tokenErr   string
		openErr    bool
		cancel     bool
		wantErr    string
		wantStatus int
	}{
		{name: "state mismatch", mutate: func(q url.Values) { q.Set("state", "forged") }, wantErr: "state mismatch", wantStatus: 400},
		{name: "access denied", mutate: func(q url.Values) { q.Del("code"); q.Set("error", "access_denied") }, wantErr: "access_denied", wantStatus: 400},
		{name: "missing code", mutate: func(q url.Values) { q.Del("code") }, wantErr: "without code", wantStatus: 400},
		{name: "exchange rejected", mutate: func(q url.Values) { q.Set("code", "stale") }, wantErr: "invalid_code", wantStatus: 400},
		{name: "open fails", openErr: true, wantErr: "open the authorize URL"},
		{name: "canceled", cancel: true, wantErr: context.Canceled.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			cfg := f.oauthConfig()
			cfg.RedirectURL = freeRedirectURL(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b := &browser{t: t, f: f, mutate: tt.mutate}
			open := b.open
			switch {
			case tt.openErr:
				open = func(string) error { return errors.New("no browser") }
			case tt.cancel:
				open = func(string) error { cancel(); return nil }
			}

			_, err := Login(ctx, cfg, open)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Login error = %v, want it to contain %q", err, tt.wantErr)
			}
			for _, secret := range []string{testClientID, testCode} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q leaks %q", err, secret)
				}
			}
			if tt.wantStatus != 0 && b.status != tt.wantStatus {
				t.Errorf("callback status = %d, want %d", b.status, tt.wantStatus)
			}
		})
	}
}

func TestLoginRequiresClientID(t *testing.T) {
	_, err := Login(t.Context(), OAuthConfig{}, func(string) error {
		t.Error("open called without client ID")
		return nil
	})
	if err == nil {
		t.Fatal("Login without client ID succeeded")
	}
}

func TestRefresh(t *testing.T) {
	tests := []struct {
		name       string
		refresh    func(f *fakeSlack) string
		clientID   string
		tokenError func(w http.ResponseWriter) bool
		wantCode   string
	}{
		{name: "rotates", refresh: func(f *fakeSlack) string { _, r, _ := f.tokens(); return r }},
		{name: "spent refresh token", refresh: func(*fakeSlack) string { return "xoxe-1-refresh-old" }, wantCode: "invalid_refresh_token"},
		{
			name:    "oauth style error",
			refresh: func(f *fakeSlack) string { _, r, _ := f.tokens(); return r },
			tokenError: func(w http.ResponseWriter) bool {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"error": "invalid_grant", "error_description": "client 123.456 unknown"})
				return true
			},
			wantCode: "invalid_grant",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			f.tokenError = tt.tokenError
			old := tt.refresh(f)
			tok, err := Refresh(t.Context(), f.oauthConfig(), old)
			if tt.wantCode != "" {
				apiErr, ok := errors.AsType[*APIError](err)
				if !ok || apiErr.Code != tt.wantCode {
					t.Fatalf("Refresh error = %v, want code %s", err, tt.wantCode)
				}
				for _, secret := range []string{testClientID, old} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error %q leaks %q", err, secret)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			access, refresh, n := f.tokens()
			if tok.AccessToken != access || tok.RefreshToken != refresh || refresh == old || n != 1 {
				t.Errorf("token = %+v after %d refreshes, want rotated access %q refresh %q", tok, n, access, refresh)
			}
			// Single use: the old refresh token no longer works.
			if _, err := Refresh(t.Context(), f.oauthConfig(), old); err == nil {
				t.Error("second refresh with a spent refresh token succeeded")
			}
		})
	}
}

func TestRefreshRequiresInputs(t *testing.T) {
	if _, err := Refresh(t.Context(), OAuthConfig{}, "xoxe-1-refresh-0"); err == nil {
		t.Error("Refresh without client ID succeeded")
	}
	if _, err := Refresh(t.Context(), OAuthConfig{ClientID: testClientID}, ""); err == nil {
		t.Error("Refresh without refresh token succeeded")
	}
}
