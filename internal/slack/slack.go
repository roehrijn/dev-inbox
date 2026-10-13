// Package slack is a read-only client of the Slack Web API, authenticated as the user through the
// OAuth user flow of an existing Slack app: PKCE, no client secret, short-lived rotating tokens.
package slack

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	ClientIDEnv    = "SLACK_CLIENT_ID"
	DefaultBaseURL = "https://slack.com"
	// DefaultRedirectURL must match a redirect URL registered for the app.
	DefaultRedirectURL = "http://localhost:3118/callback"
	// Resource is sent as the RFC 8707 resource parameter, like the MCP clients of the app do.
	Resource = "https://mcp.slack.com"

	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 8 << 20
)

// Scopes are the user scopes requested at login.
var Scopes = []string{"channels:history", "groups:history", "channels:read", "groups:read", "users:read"}

// ErrNoToken means no token is stored yet; run `dev-inbox-slack login`.
var ErrNoToken = errors.New("no Slack token, run `dev-inbox-slack login`")

type OAuthConfig struct {
	ClientID    string
	RedirectURL string       // default DefaultRedirectURL
	BaseURL     string       // default DefaultBaseURL; API at BaseURL/api/, authorize at BaseURL/oauth/v2_user/authorize
	HTTPClient  *http.Client // default a client with a 30s timeout
}

func (cfg OAuthConfig) withDefaults() OAuthConfig {
	if cfg.RedirectURL == "" {
		cfg.RedirectURL = DefaultRedirectURL
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultTimeout}
	}
	return cfg
}

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope,omitempty"`
}

// APIError is an {"ok":false} answer of Slack. Code is Slack's error code, e.g. channel_not_found.
type APIError struct {
	Method string
	Code   string
}

func (e *APIError) Error() string { return fmt.Sprintf("slack: %s: %s", e.Method, e.Code) }

// Identity is the authenticated user. TeamURL is the workspace URL, e.g. https://acme-co.slack.com/.
type Identity struct {
	UserID  string
	TeamURL string
}

type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Message is a channel message or thread reply. ThreadTS is empty for messages without a thread and
// equals TS for a thread parent. LatestReply is the TS of the newest reply of a thread parent.
type Message struct {
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Subtype     string `json:"subtype"`
	Text        string `json:"text"`
	ReplyCount  int    `json:"reply_count"`
	LatestReply string `json:"latest_reply"`
}
