package slack

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	authorizePath = "/oauth/v2_user/authorize"
	tokenMethod   = "oauth.v2.user.access"
)

const callbackPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>dev-inbox</title></head>
<body><p>%s</p></body></html>
`

// Login runs the browser flow: it serves RedirectURL, calls open with the authorize URL, waits for
// the callback and exchanges the code at oauth.v2.user.access. An error from open aborts the login.
func Login(ctx context.Context, cfg OAuthConfig, open func(url string) error) (Token, error) {
	cfg = cfg.withDefaults()
	if cfg.ClientID == "" {
		return Token{}, errors.New("slack: OAuth client ID is required")
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil || redirect.Scheme != "http" || redirect.Host == "" {
		return Token{}, fmt.Errorf("slack: redirect URL %q must be an http URL with a host", cfg.RedirectURL)
	}
	verifier, err := randomString(32)
	if err != nil {
		return Token{}, err
	}
	state, err := randomString(16)
	if err != nil {
		return Token{}, err
	}

	addr := redirect.Host
	if redirect.Hostname() == "localhost" {
		addr = net.JoinHostPort("127.0.0.1", redirect.Port())
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return Token{}, fmt.Errorf("slack: listen for the OAuth callback: %w", err)
	}

	type result struct {
		tok Token
		err error
	}
	results := make(chan result, 1)
	var once sync.Once
	path := redirect.Path
	if path == "" {
		path = "/"
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		handled := false
		once.Do(func() {
			handled = true
			tok, err := handleCallback(ctx, cfg, r.URL.Query(), state, verifier)
			results <- result{tok, err}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, callbackPage, "Slack sign-in for dev-inbox failed. See the terminal for details.")
				return
			}
			_, _ = fmt.Fprintf(w, callbackPage, "Signed in to Slack for dev-inbox. You can close this tab.")
		})
		if !handled {
			http.Error(w, "sign-in already handled", http.StatusConflict)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed after Shutdown
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx) //nolint:errcheck
	}()

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {cfg.ClientID},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
		"redirect_uri":          {cfg.RedirectURL},
		"state":                 {state},
		"scope":                 {strings.Join(Scopes, " ")},
		"resource":              {Resource},
	}
	authURL := strings.TrimSuffix(cfg.BaseURL, "/") + authorizePath + "?" + q.Encode()
	if err := open(authURL); err != nil {
		return Token{}, fmt.Errorf("slack: open the authorize URL: %w", err)
	}

	select {
	case res := <-results:
		return res.tok, res.err
	case <-ctx.Done():
		return Token{}, ctx.Err()
	}
}

func handleCallback(ctx context.Context, cfg OAuthConfig, q url.Values, state, verifier string) (Token, error) {
	if q.Get("state") != state {
		return Token{}, errors.New("slack: OAuth callback state mismatch")
	}
	if e := q.Get("error"); e != "" {
		return Token{}, fmt.Errorf("slack: authorization failed: %s", e)
	}
	code := q.Get("code")
	if code == "" {
		return Token{}, errors.New("slack: OAuth callback without code")
	}
	return exchange(ctx, cfg, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {cfg.RedirectURL},
		"client_id":     {cfg.ClientID},
		"resource":      {Resource},
	})
}

// Refresh exchanges refreshToken for a new token. Refresh tokens are single use.
func Refresh(ctx context.Context, cfg OAuthConfig, refreshToken string) (Token, error) {
	cfg = cfg.withDefaults()
	if cfg.ClientID == "" {
		return Token{}, errors.New("slack: OAuth client ID is required")
	}
	if refreshToken == "" {
		return Token{}, errors.New("slack: no refresh token")
	}
	return exchange(ctx, cfg, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {cfg.ClientID},
		"resource":      {Resource},
	})
}

// exchange posts form to oauth.v2.user.access. Errors carry only Slack's error code, never the form
// or the response body, which hold secrets.
func exchange(ctx context.Context, cfg OAuthConfig, form url.Values) (Token, error) {
	endpoint := strings.TrimSuffix(cfg.BaseURL, "/") + "/api/" + tokenMethod
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("slack: %s: build request: %w", tokenMethod, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("slack: %s: %w", tokenMethod, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Token{}, fmt.Errorf("slack: %s: read response: %w", tokenMethod, err)
	}
	var r struct {
		OK           *bool  `json:"ok"`
		Error        string `json:"error"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Token{}, fmt.Errorf("slack: %s: HTTP %d, response is not JSON", tokenMethod, resp.StatusCode)
	}
	if r.Error != "" || (r.OK != nil && !*r.OK) {
		code := r.Error
		if code == "" {
			code = "unknown_error"
		}
		return Token{}, &APIError{Method: tokenMethod, Code: code}
	}
	if resp.StatusCode/100 != 2 {
		return Token{}, fmt.Errorf("slack: %s: HTTP %d", tokenMethod, resp.StatusCode)
	}
	if r.AccessToken == "" {
		return Token{}, fmt.Errorf("slack: %s: response without access token", tokenMethod)
	}
	t := Token{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, Scope: r.Scope}
	if r.ExpiresIn > 0 {
		t.ExpiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).UTC()
	}
	return t, nil
}

// redactURLError drops the URL from a *url.Error; Web API URLs carry query parameters.
func redactURLError(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("slack: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
