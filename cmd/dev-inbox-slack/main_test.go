package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/slack"
	"github.com/roehrijn/dev-inbox/internal/slackplugin"
)

const clientID = "123.456"

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

var withID = env(map[string]string{slack.ClientIDEnv: clientID})

type stubSlack struct{ slackplugin.Slack }

func (stubSlack) AuthTest(context.Context) (slack.Identity, error) {
	return slack.Identity{UserID: "U123", TeamURL: "https://acme-co.slack.com/"}, nil
}

// testDeps records the configuration handed to the plugin and the Slack client.
type testDeps struct {
	plugin slackplugin.Config
	client slack.Config
}

func (td *testDeps) deps(getenv func(string) string) deps {
	return deps{
		run: func(_ context.Context, cfg slackplugin.Config) error {
			td.plugin = cfg
			return nil
		},
		getenv: getenv,
		login: func(context.Context, slack.OAuthConfig, func(string) error) (slack.Token, error) {
			return slack.Token{}, errors.New("unexpected login")
		},
		newClient: func(cfg slack.Config) (slackplugin.Slack, error) {
			td.client = cfg
			return stubSlack{}, nil
		},
		browse: func(string) error { return errors.New("unexpected browser") },
	}
}

// runWith runs the command with args and returns the configuration handed to the plugin.
func runWith(t *testing.T, getenv func(string) string, args ...string) (*testDeps, string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	td := &testDeps{}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), td.deps(getenv), args, &stdout, &stderr)
	return td, stderr.String(), err
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slack.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	td, _, err := runWith(t, withID, "--config", writeConfig(t, "channels: [dev-pr-reviews]\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := td.plugin
	if cfg.NATSURL != api.DefaultNATSURL || cfg.Score != slackplugin.DefaultScore ||
		cfg.PollInterval != slackplugin.DefaultPollInterval || cfg.Lookback != slackplugin.DefaultLookback ||
		!reflect.DeepEqual(cfg.Channels, []string{"dev-pr-reviews"}) || len(cfg.Groups) != 0 ||
		cfg.Slack == nil || cfg.Logger == nil {
		t.Errorf("got %+v", cfg)
	}
	if td.client.OAuth.ClientID != clientID || td.client.Store.Path != "" || td.client.Logger == nil {
		t.Errorf("slack config %+v", td.client)
	}
}

func TestConfigFile(t *testing.T) {
	path := writeConfig(t, `
nats-url: nats://slack:4222
log-level: debug
oauth-client-id: 987.654
channels: ["#dev-pr-reviews", C0123]
groups: [S0123ABCD]
score: 15
poll-interval: 30s
lookback: 48h
`)
	td, _, err := runWith(t, env(nil), "--config", path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := td.plugin
	if cfg.NATSURL != "nats://slack:4222" || cfg.Score != 15 || cfg.PollInterval != 30*time.Second ||
		cfg.Lookback != 48*time.Hour || !reflect.DeepEqual(cfg.Channels, []string{"#dev-pr-reviews", "C0123"}) ||
		!reflect.DeepEqual(cfg.Groups, []string{"S0123ABCD"}) {
		t.Errorf("got %+v", cfg)
	}
	if td.client.OAuth.ClientID != "987.654" {
		t.Errorf("client ID %q from the file, want 987.654", td.client.OAuth.ClientID)
	}
}

func TestClientIDPrecedence(t *testing.T) {
	path := writeConfig(t, "oauth-client-id: \"111.222\"\nchannels: [dev-pr-reviews]\n")
	for name, tt := range map[string]struct {
		getenv func(string) string
		want   string
	}{
		"config file":        {env(nil), "111.222"},
		"env over file":      {withID, clientID},
		"blank env":          {env(map[string]string{slack.ClientIDEnv: " "}), "111.222"},
		"env with spaces":    {env(map[string]string{slack.ClientIDEnv: " 333.444\n"}), "333.444"},
		"file with no value": {withID, clientID},
	} {
		t.Run(name, func(t *testing.T) {
			p := path
			if name == "file with no value" {
				p = writeConfig(t, "channels: [dev-pr-reviews]\n")
			}
			td, _, err := runWith(t, tt.getenv, "--config", p)
			if err != nil {
				t.Fatal(err)
			}
			if td.client.OAuth.ClientID != tt.want {
				t.Errorf("got %q, want %q", td.client.OAuth.ClientID, tt.want)
			}
		})
	}
}

func TestEmptyConfigFile(t *testing.T) {
	for name, content := range map[string]string{
		"empty":        "",
		"comment only": "# nothing\n",
		"null":         "null\n",
		"tilde":        "~\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runWith(t, withID, "--config", writeConfig(t, content))
			if err == nil || !strings.Contains(err.Error(), "no channels") {
				t.Errorf("got %v, want the missing channels", err)
			}
		})
	}
}

func TestNATSURLPrecedence(t *testing.T) {
	path := writeConfig(t, "nats-url: nats://slack:4222\nchannels: [c]\n")
	plain := writeConfig(t, "channels: [c]\n")
	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"config file":      {[]string{"--config", path}, "nats://slack:4222"},
		"flag over config": {[]string{"--config", path, "--nats-url", "nats://flag:4222"}, "nats://flag:4222"},
		"default":          {[]string{"--config", plain}, api.DefaultNATSURL},
	} {
		t.Run(name, func(t *testing.T) {
			td, _, err := runWith(t, withID, tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			if td.plugin.NATSURL != tt.want {
				t.Errorf("got %q, want %q", td.plugin.NATSURL, tt.want)
			}
		})
	}
}

func TestLogLevelPrecedence(t *testing.T) {
	path := writeConfig(t, "log-level: error\nchannels: [c]\n")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tt := range []struct {
		name    string
		args    []string
		wantLog bool
	}{
		{name: "config file", args: []string{"--config", path}},
		{name: "flag overrides file", args: []string{"--config", path, "--log-level", "debug"}, wantLog: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			d := (&testDeps{}).deps(withID)
			d.run = func(_ context.Context, cfg slackplugin.Config) error {
				cfg.Logger.Debug("probe")
				return nil
			}
			if err := run(context.Background(), d, tt.args, &bytes.Buffer{}, &stderr); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(stderr.String(), "probe"); got != tt.wantLog {
				t.Errorf("debug line logged %v, want %v: %q", got, tt.wantLog, stderr.String())
			}
		})
	}
}

func TestClientIDNotPrinted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := writeConfig(t, "channels: [c]\noauth-client-id: \"123.456\"\nbogus: 1\n")
	var stderr bytes.Buffer
	_ = run(context.Background(), (&testDeps{}).deps(env(nil)), []string{"--config", path}, &bytes.Buffer{}, &stderr)
	if strings.Contains(stderr.String(), clientID) {
		t.Errorf("usage error leaks the client ID: %q", stderr.String())
	}

	stderr.Reset()
	d := (&testDeps{}).deps(withID)
	d.run = func(_ context.Context, cfg slackplugin.Config) error {
		cfg.Logger.Info("started")
		return errors.New("boom")
	}
	_ = run(context.Background(), d, []string{"--config", writeConfig(t, "channels: [c]\n"), "--log-level", "debug"},
		&bytes.Buffer{}, &stderr)
	if strings.Contains(stderr.String(), clientID) {
		t.Errorf("stderr leaks the client ID: %q", stderr.String())
	}
}

func TestDefaultConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "dev-inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]string{
		"slack.yaml":  "channels: [dev-pr-reviews]\nscore: 14\n",
		"config.yaml": "score: 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "dev-inbox", file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	td := &testDeps{}
	if err := run(context.Background(), td.deps(withID), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if td.plugin.Score != 14 {
		t.Errorf("score %d, want 14 from slack.yaml", td.plugin.Score)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name   string
		config string // written to a file passed with --config; "" passes a file with a channel
		args   []string
		getenv func(string) string
		want   []string
	}{
		{name: "missing client ID", getenv: env(nil), want: []string{"SLACK_CLIENT_ID", "oauth-client-id"}},
		{name: "missing client ID at login", getenv: env(nil), args: []string{"login"},
			want: []string{"SLACK_CLIENT_ID", "oauth-client-id"}},
		{name: "no channels", config: "groups: [S1]\n", want: []string{"no channels", "slack.yaml"}},
		{name: "explicit config missing", args: []string{"--config", "/nonexistent/slack.yaml"},
			want: []string{"does not exist"}},
		{name: "unknown keys", config: "channel: [c]\nclient-id: x\n",
			want: []string{`unknown key "channel"`, `unknown key "client-id"`}},
		{name: "core-style plugins section", config: "plugins:\n  slack:\n    score: 3\n",
			want: []string{`unknown key "plugins"`}},
		{name: "non-mapping document", config: "[1, 2]\n", want: []string{"the document must be a mapping"}},
		{name: "scalar document", config: "foo\n", want: []string{"the document must be a mapping"}},
		{name: "channels not a list", config: "channels: dev-pr-reviews\n", want: []string{"slack.yaml"}},
		{name: "empty channel", config: "channels: [\"\"]\n", want: []string{"channels: empty channel"}},
		{name: "invalid group", config: "channels: [c]\ngroups: [\"S1 S2\"]\n", want: []string{"groups", "S1 S2"}},
		{name: "fractional score", config: "channels: [c]\nscore: 1.5\n", want: []string{"slack.yaml: score", "1.5"}},
		{name: "quoted score", config: "channels: [c]\nscore: \"7\"\n", want: []string{"score"}},
		{name: "zero score", config: "channels: [c]\nscore: 0\n", want: []string{"score", "not positive"}},
		{name: "negative score", config: "channels: [c]\nscore: -1\n", want: []string{"score", "not positive"}},
		{name: "poll interval without unit", config: "channels: [c]\npoll-interval: 5\n",
			want: []string{"poll-interval", "missing unit"}},
		{name: "zero poll interval", config: "channels: [c]\npoll-interval: 0s\n",
			want: []string{"poll-interval", "not positive"}},
		{name: "negative lookback", config: "channels: [c]\nlookback: -1h\n", want: []string{"lookback", "not positive"}},
		{name: "invalid lookback", config: "channels: [c]\nlookback: a week\n", want: []string{"lookback"}},
		{name: "invalid log level", args: []string{"--log-level", "loud"}, want: []string{`invalid log-level "loud"`}},
		{name: "argument", args: []string{"serve"}, want: []string{`unexpected argument "serve"`}},
		{name: "login argument", args: []string{"login", "now"}, want: []string{`unexpected argument "now"`}},
		{name: "run flag on login", args: []string{"login", "--nats-url", "nats://x:1"}, want: []string{"unknown flag"}},
		{name: "unknown flag", args: []string{"--bogus"}, want: []string{"unknown flag"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := tt.config
			if config == "" {
				config = "channels: [dev-pr-reviews]\n"
			}
			args := tt.args
			if !strings.Contains(strings.Join(args, " "), "--config") {
				args = append(args, "--config", writeConfig(t, config))
			}
			getenv := tt.getenv
			if getenv == nil {
				getenv = withID
			}
			_, stderr, err := runWith(t, getenv, args...)
			var usage usageError
			if !errors.As(err, &usage) {
				t.Fatalf("got error %v, want a usage error", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if !strings.Contains(stderr, "Run 'dev-inbox-slack --help' for usage.") {
				t.Errorf("stderr %q lacks the usage hint", stderr)
			}
		})
	}
}

func TestNoToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := (&testDeps{}).deps(withID)
	d.newClient = defaultDeps().newClient
	var stderr bytes.Buffer
	err := run(context.Background(), d, []string{"--config", writeConfig(t, "channels: [c]\n")}, &bytes.Buffer{}, &stderr)
	var usage usageError
	if !errors.Is(err, slack.ErrNoToken) || errors.As(err, &usage) {
		t.Fatalf("got %v, want ErrNoToken", err)
	}
	if !strings.Contains(stderr.String(), "dev-inbox-slack login") {
		t.Errorf("stderr %q lacks the login hint", stderr.String())
	}
}

func TestLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	token := slack.Token{AccessToken: "xoxe.p-access", RefreshToken: "xoxe-1-refresh",
		ExpiresAt: time.Date(2026, 10, 12, 22, 0, 0, 0, time.UTC)}
	const authorize = "https://slack.com/oauth/v2_user/authorize?client_id=123.456"

	for _, tt := range []struct {
		name      string
		browseErr error
	}{
		{name: "browser opens"},
		{name: "browser fails", browseErr: errors.New("xdg-open not found")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			td := &testDeps{}
			d := td.deps(withID)
			var gotOAuth slack.OAuthConfig
			var browsed string
			d.login = func(_ context.Context, cfg slack.OAuthConfig, open func(string) error) (slack.Token, error) {
				gotOAuth = cfg
				if err := open(authorize); err != nil {
					return slack.Token{}, err
				}
				return token, nil
			}
			d.browse = func(url string) error {
				browsed = url
				return tt.browseErr
			}
			var stdout, stderr bytes.Buffer
			if err := run(context.Background(), d, []string{"login", "--config", writeConfig(t, "")}, &stdout, &stderr); err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			if gotOAuth.ClientID != clientID || browsed != authorize {
				t.Errorf("login with %+v, browsed %q", gotOAuth, browsed)
			}
			if !strings.Contains(stderr.String(), authorize) {
				t.Errorf("stderr %q lacks the URL", stderr.String())
			}
			if failed := strings.Contains(stderr.String(), "xdg-open not found"); failed != (tt.browseErr != nil) {
				t.Errorf("browser failure reported %v: %q", failed, stderr.String())
			}
			if stdout.String() != "signed in as U123\n" {
				t.Errorf("stdout %q", stdout.String())
			}
			path := filepath.Join(state, "dev-inbox", "slack-token.json")
			if td.client.Store.Path != path || td.client.OAuth.ClientID != clientID {
				t.Errorf("client config %+v", td.client)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved slack.Token
			if err := json.Unmarshal(data, &saved); err != nil || saved.AccessToken != token.AccessToken ||
				saved.RefreshToken != token.RefreshToken {
				t.Errorf("saved %s: %v", data, err)
			}
		})
	}
}

func TestLoginFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := (&testDeps{}).deps(withID)
	d.login = func(context.Context, slack.OAuthConfig, func(string) error) (slack.Token, error) {
		return slack.Token{}, errors.New("access_denied")
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), d, []string{"login"}, &stdout, &stderr)
	if err == nil || stderr.String() != "dev-inbox-slack: slack login: access_denied\n" || stdout.Len() != 0 {
		t.Errorf("err %v, stderr %q, stdout %q", err, stderr.String(), stdout.String())
	}
}

func TestRunError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	d := (&testDeps{}).deps(withID)
	d.run = func(context.Context, slackplugin.Config) error { return errors.New("boom") }
	var stderr bytes.Buffer
	err := run(context.Background(), d, []string{"--config", writeConfig(t, "channels: [c]\n")}, &bytes.Buffer{}, &stderr)
	var usage usageError
	if err == nil || errors.As(err, &usage) {
		t.Fatalf("got %v, want a plain error", err)
	}
	if stderr.String() != "dev-inbox-slack: boom\n" {
		t.Errorf("stderr %q", stderr.String())
	}
}

func TestVersionFlag(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "1.2.3"
	td := &testDeps{}
	var stdout bytes.Buffer
	err := run(context.Background(), td.deps(env(nil)), []string{"--version"}, &stdout, &bytes.Buffer{})
	if err != nil || stdout.String() != "dev-inbox-slack version 1.2.3\n" {
		t.Errorf("--version = %q, %v", stdout.String(), err)
	}
}
