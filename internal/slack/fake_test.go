package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testClientID = "123.456"
	testCode     = "code-abc"
)

// fakeSlack serves the OAuth token endpoint and Web API methods. Access and refresh tokens rotate on
// every refresh; only the current ones are accepted.
type fakeSlack struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	access      string
	refresh     string
	gen         int
	refreshes   int
	challenge   string
	redirectURI string
	expired     map[string]bool // access tokens answered with token_expired
	rejectAll   string          // when set, every API call fails with this error
	tokenError  func(w http.ResponseWriter) bool
	methods     map[string]http.HandlerFunc
	calls       map[string][]url.Values
}

func newFake(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		t:       t,
		access:  "xoxe.xoxp-1-access-0",
		refresh: "xoxe-1-refresh-0",
		expired: map[string]bool{},
		methods: map[string]http.HandlerFunc{},
		calls:   map[string][]url.Values{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) handle(method string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods[method] = h
}

func (f *fakeSlack) callsOf(method string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeSlack) tokens() (access, refresh string, refreshes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.access, f.refresh, f.refreshes
}

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/api/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if method == tokenMethod {
		f.serveToken(w, r)
		return
	}
	f.mu.Lock()
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	authErr := ""
	switch {
	case f.rejectAll != "":
		authErr = f.rejectAll
	case f.expired[bearer]:
		authErr = "token_expired"
	case bearer != f.access:
		authErr = "invalid_auth"
	}
	h := f.methods[method]
	if authErr == "" {
		f.calls[method] = append(f.calls[method], r.URL.Query())
	}
	f.mu.Unlock()
	if authErr != "" {
		writeJSON(w, map[string]any{"ok": false, "error": authErr})
		return
	}
	if h == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "unknown_method"})
		return
	}
	h(w, r)
}

func (f *fakeSlack) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("token endpoint: method %s, want POST", r.Method)
	}
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("token endpoint: parse form: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenError != nil && f.tokenError(w) {
		return
	}
	fail := func(code string) { writeJSON(w, map[string]any{"ok": false, "error": code}) }
	if r.PostForm.Get("client_id") != testClientID {
		fail("invalid_client_id")
		return
	}
	if r.PostForm.Get("resource") != Resource {
		fail("invalid_resource")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		switch {
		case r.PostForm.Get("code") != testCode:
			fail("invalid_code")
			return
		case challenge(r.PostForm.Get("code_verifier")) != f.challenge:
			fail("invalid_code_verifier")
			return
		case r.PostForm.Get("redirect_uri") != f.redirectURI:
			fail("bad_redirect_uri")
			return
		}
	case "refresh_token":
		if r.PostForm.Get("refresh_token") != f.refresh {
			fail("invalid_refresh_token")
			return
		}
		f.refreshes++
	default:
		fail("unsupported_grant_type")
		return
	}
	f.expired[f.access] = true
	f.gen++
	f.access = fmt.Sprintf("xoxe.xoxp-1-access-%d", f.gen)
	f.refresh = fmt.Sprintf("xoxe-1-refresh-%d", f.gen)
	writeJSON(w, map[string]any{
		"ok":            true,
		"access_token":  f.access,
		"refresh_token": f.refresh,
		"expires_in":    43200,
		"scope":         strings.Join(Scopes, ","),
		"token_type":    "user",
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func (f *fakeSlack) oauthConfig() OAuthConfig {
	return OAuthConfig{ClientID: testClientID, BaseURL: f.srv.URL}
}

// newClient stores tok and returns a client on it whose rate limit waits are recorded, not slept.
func (f *fakeSlack) newClient(t *testing.T, tok Token) (*Client, *[]time.Duration) {
	t.Helper()
	store := TokenStore{Path: filepath.Join(t.TempDir(), "state", "slack-token.json")}
	if err := store.Save(tok); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{OAuth: f.oauthConfig(), Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var waits []time.Duration
	c.wait = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		return ctx.Err()
	}
	return c, &waits
}

// validToken is the fake's current token, expiring in an hour.
func (f *fakeSlack) validToken() Token {
	access, refresh, _ := f.tokens()
	return Token{AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour)}
}

func freeRedirectURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return fmt.Sprintf("http://localhost:%d/callback", port)
}
