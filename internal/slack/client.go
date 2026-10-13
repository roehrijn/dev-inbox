package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// refreshMargin is how long before expiry the client refreshes proactively.
	refreshMargin       = 5 * time.Minute
	maxRateLimitRetries = 10
	defaultRetryAfter   = time.Second
	pageLimit           = "200"
	listLimit           = "1000"
)

// channelIDRe matches conversation IDs; channel names are lowercase.
var channelIDRe = regexp.MustCompile(`^[CGD][A-Z0-9]{6,}$`)

type Config struct {
	OAuth  OAuthConfig
	Store  TokenStore // Path defaults to DefaultTokenPath
	Logger *slog.Logger
}

// Client calls the Web API with the stored token. It refreshes the token shortly before it expires or
// when Slack answers token_expired, saving the rotated token, and waits out Retry-After on HTTP 429.
// It is safe for concurrent use.
type Client struct {
	oauth OAuthConfig
	store TokenStore
	log   *slog.Logger
	api   string

	now  func() time.Time
	wait func(ctx context.Context, d time.Duration) error

	mu  sync.Mutex // guards tok and serializes refreshes
	tok Token

	usersMu sync.Mutex
	users   map[string]string
}

// New loads the stored token; ErrNoToken if there is none.
func New(cfg Config) (*Client, error) {
	store := cfg.Store
	if store.Path == "" {
		p, err := DefaultTokenPath()
		if err != nil {
			return nil, err
		}
		store.Path = p
	}
	tok, err := store.Load()
	if err != nil {
		return nil, err
	}
	c := &Client{
		oauth: cfg.OAuth.withDefaults(),
		store: store,
		log:   cfg.Logger,
		now:   time.Now,
		wait:  sleep,
		tok:   tok,
		users: map[string]string{},
	}
	c.api = strings.TrimSuffix(c.oauth.BaseURL, "/") + "/api/"
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// accessToken returns the current access token, refreshing it first if it expires soon.
func (c *Client) accessToken(ctx context.Context) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tok.ExpiresAt.IsZero() && c.tok.ExpiresAt.Sub(c.now()) < refreshMargin {
		if err := c.refreshLocked(ctx); err != nil {
			return Token{}, err
		}
	}
	return c.tok, nil
}

// refreshAfterReject refreshes the token Slack rejected, unless a concurrent call already did.
func (c *Client) refreshAfterReject(ctx context.Context, rejected string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok.AccessToken != rejected {
		return nil
	}
	return c.refreshLocked(ctx)
}

func (c *Client) refreshLocked(ctx context.Context) error {
	if c.tok.RefreshToken == "" {
		return errors.New("slack: token expired and there is no refresh token, run `dev-inbox-slack login`")
	}
	t, err := Refresh(ctx, c.oauth, c.tok.RefreshToken)
	if err != nil {
		return fmt.Errorf("refresh Slack token, run `dev-inbox-slack login` if this persists: %w", err)
	}
	if t.RefreshToken == "" {
		t.RefreshToken = c.tok.RefreshToken
	}
	// The old refresh token is spent, so keep the new one in memory even if saving fails.
	saveErr := c.store.Save(t)
	c.tok = t
	if saveErr != nil {
		return fmt.Errorf("slack: token refreshed but not saved: %w", saveErr)
	}
	c.log.Info("refreshed Slack token", "expires_at", t.ExpiresAt)
	return nil
}

// call runs a Web API method and decodes the response into out.
func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	refreshed := false
	rateLimited := 0
	for {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		u := c.api + method
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return fmt.Errorf("slack: %s: build request: %w", method, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		resp, err := c.oauth.HTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("slack: %s: %w", method, redactURLError(err))
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			rateLimited++
			if rateLimited > maxRateLimitRetries {
				return fmt.Errorf("slack: %s: still rate limited after %d retries", method, maxRateLimitRetries)
			}
			d := retryAfter(resp.Header.Get("Retry-After"))
			c.log.Debug("Slack rate limit, waiting", "method", method, "retry_after", d)
			if err := c.wait(ctx, d); err != nil {
				return fmt.Errorf("slack: %s: waiting out rate limit: %w", method, err)
			}
			continue
		}
		if readErr != nil {
			return fmt.Errorf("slack: %s: read response: %w", method, readErr)
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("slack: %s: HTTP %d", method, resp.StatusCode)
		}
		var env struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("slack: %s: decode response: %w", method, err)
		}
		if !env.OK {
			if !refreshed && c.refreshable(env.Error, tok) {
				refreshed = true
				c.log.Debug("Slack rejected the token, refreshing", "method", method, "error", env.Error)
				if err := c.refreshAfterReject(ctx, tok.AccessToken); err != nil {
					return err
				}
				continue
			}
			code := env.Error
			if code == "" {
				code = "unknown_error"
			}
			return &APIError{Method: method, Code: code}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("slack: %s: decode response: %w", method, err)
		}
		return nil
	}
}

// refreshable reports whether an error code means the access token expired. Slack answers some
// methods with invalid_auth instead of token_expired, so that counts for rotating tokens too.
func (c *Client) refreshable(code string, tok Token) bool {
	switch code {
	case "token_expired":
		return true
	case "invalid_auth":
		return tok.RefreshToken != "" && !tok.ExpiresAt.IsZero()
	}
	return false
}

// retryAfter parses Retry-After in seconds, the only form Slack sends.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return defaultRetryAfter
	}
	return time.Duration(n) * time.Second
}

type page struct {
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

func (p page) next() string { return p.ResponseMetadata.NextCursor }

// AuthTest calls auth.test.
func (c *Client) AuthTest(ctx context.Context) (Identity, error) {
	var r struct {
		UserID string `json:"user_id"`
		URL    string `json:"url"`
	}
	if err := c.call(ctx, "auth.test", nil, &r); err != nil {
		return Identity{}, err
	}
	return Identity{UserID: r.UserID, TeamURL: r.URL}, nil
}

// Channel resolves a channel ID (conversations.info) or name with or without "#" (conversations.list).
func (c *Client) Channel(ctx context.Context, idOrName string) (Channel, error) {
	name, hash := strings.CutPrefix(strings.TrimSpace(idOrName), "#")
	if name == "" {
		return Channel{}, errors.New("slack: empty channel")
	}
	if !hash && channelIDRe.MatchString(name) {
		var r struct {
			Channel Channel `json:"channel"`
		}
		if err := c.call(ctx, "conversations.info", url.Values{"channel": {name}}, &r); err != nil {
			return Channel{}, err
		}
		return r.Channel, nil
	}
	params := url.Values{
		"types":            {"public_channel,private_channel"},
		"exclude_archived": {"true"},
		"limit":            {listLimit},
	}
	for {
		var r struct {
			page
			Channels []Channel `json:"channels"`
		}
		if err := c.call(ctx, "conversations.list", params, &r); err != nil {
			return Channel{}, err
		}
		for _, ch := range r.Channels {
			if ch.Name == name {
				return ch, nil
			}
		}
		if r.next() == "" {
			return Channel{}, fmt.Errorf("slack: channel %q not found or not visible", "#"+name)
		}
		params.Set("cursor", r.next())
	}
}

// History returns the messages of channel since oldest, all pages (conversations.history).
func (c *Client) History(ctx context.Context, channel string, oldest time.Time) ([]Message, error) {
	params := url.Values{"channel": {channel}, "limit": {pageLimit}}
	if !oldest.IsZero() {
		params.Set("oldest", formatTS(oldest))
	}
	return c.messages(ctx, "conversations.history", params)
}

// Replies returns a thread, parent first, all pages (conversations.replies).
func (c *Client) Replies(ctx context.Context, channel, threadTS string) ([]Message, error) {
	return c.messages(ctx, "conversations.replies", url.Values{"channel": {channel}, "ts": {threadTS}, "limit": {pageLimit}})
}

func (c *Client) messages(ctx context.Context, method string, params url.Values) ([]Message, error) {
	var all []Message
	for {
		var r struct {
			page
			Messages []Message `json:"messages"`
		}
		if err := c.call(ctx, method, params, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Messages...)
		if r.next() == "" {
			return all, nil
		}
		params.Set("cursor", r.next())
	}
}

// formatTS formats t like a Slack message timestamp, "seconds.micros".
func formatTS(t time.Time) string {
	us := t.UnixMicro()
	sec, frac := us/1_000_000, us%1_000_000
	if frac < 0 {
		sec, frac = sec-1, frac+1_000_000
	}
	return fmt.Sprintf("%d.%06d", sec, frac)
}

// UserName returns the display name of a user (users.info), falling back to the real name, cached.
// Without either it falls back to the user name, then the ID.
func (c *Client) UserName(ctx context.Context, userID string) (string, error) {
	c.usersMu.Lock()
	name, ok := c.users[userID]
	c.usersMu.Unlock()
	if ok {
		return name, nil
	}
	var r struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.call(ctx, "users.info", url.Values{"user": {userID}}, &r); err != nil {
		return "", err
	}
	for _, n := range []string{r.User.Profile.DisplayName, r.User.Profile.RealName, r.User.Name, userID} {
		if n != "" {
			name = n
			break
		}
	}
	c.usersMu.Lock()
	c.users[userID] = name
	c.usersMu.Unlock()
	return name, nil
}
