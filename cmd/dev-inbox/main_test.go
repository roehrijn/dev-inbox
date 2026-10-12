package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/daemon"
	"github.com/roehrijn/dev-inbox/internal/derive"
)

// result is what a command line ran with.
type result struct {
	serve  *daemon.Config
	tuiURL *string
	stdout string
	stderr string
	err    error
}

// runCLI runs the command line against fake runners.
func runCLI(t *testing.T, args ...string) result {
	t.Helper()
	return runCLIContext(t, context.Background(), nil, args...)
}

func runCLIContext(t *testing.T, ctx context.Context, runErr error, args ...string) result {
	t.Helper()
	var res result
	r := runners{
		tui: func(_ context.Context, natsURL string) error {
			res.tuiURL = &natsURL
			return runErr
		},
		serve: func(_ context.Context, cfg daemon.Config) error {
			res.serve = &cfg
			return runErr
		},
	}
	var stdout, stderr bytes.Buffer
	res.err = run(ctx, r, args, &stdout, &stderr)
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

// setupEnv points the state and config directories at empty places so that no test reads the real ones.
func setupEnv(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "/xdg")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	return home
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func isUsage(err error) bool {
	var usage usageError
	return errors.As(err, &usage)
}

func defaultServeConfig() daemon.Config {
	return daemon.Config{
		StateDir:          "/xdg/dev-inbox",
		NATSHost:          "127.0.0.1",
		NATSPort:          14222,
		PollInterval:      time.Minute,
		ReconcileInterval: 5 * time.Minute,
		Greylist:          []string{"dependabot", "renovate"},
		Weights:           derive.DefaultWeights,
	}
}

// sameConfig compares configs ignoring the logger and the nil-ness of the greylist.
func sameConfig(got, want daemon.Config) bool {
	got.Logger, want.Logger = nil, nil
	if len(got.Greylist) == 0 && len(want.Greylist) == 0 {
		got.Greylist, want.Greylist = nil, nil
	}
	return reflect.DeepEqual(got, want)
}

func TestServe(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		want      func(*daemon.Config)
		wantDebug bool
	}{
		{name: "defaults", want: func(*daemon.Config) {}},
		{
			name: "overrides",
			args: []string{"--state-dir", "/tmp/s", "--nats-host", "0.0.0.0", "--nats-port", "4222",
				"--poll-interval", "2m", "--reconcile-interval", "10m", "--log-level", "debug"},
			want: func(c *daemon.Config) {
				c.StateDir, c.NATSHost, c.NATSPort = "/tmp/s", "0.0.0.0", 4222
				c.PollInterval, c.ReconcileInterval = 2*time.Minute, 10*time.Minute
			},
			wantDebug: true,
		},
		{
			name: "preferred team",
			args: []string{"--preferred-team", "acme-co/control-plane"},
			want: func(c *daemon.Config) { c.PreferredTeam = "acme-co/control-plane" },
		},
		{
			name: "empty preferred team disables the feature",
			args: []string{"--preferred-team", ""},
			want: func(*daemon.Config) {},
		},
		{
			name: "greylist replaces defaults",
			args: []string{"--greylist", "bot-a", "--greylist", "bot-b"},
			want: func(c *daemon.Config) { c.Greylist = []string{"bot-a", "bot-b"} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupEnv(t)
			res := runCLI(t, append([]string{"serve"}, tt.args...)...)
			if res.err != nil || res.serve == nil {
				t.Fatalf("err = %v, serve = %v", res.err, res.serve)
			}
			if res.serve.Logger == nil {
				t.Fatal("logger not set")
			}
			if debug := res.serve.Logger.Enabled(context.Background(), slog.LevelDebug); debug != tt.wantDebug {
				t.Errorf("debug enabled = %v, want %v", debug, tt.wantDebug)
			}
			want := defaultServeConfig()
			tt.want(&want)
			if !sameConfig(*res.serve, want) {
				t.Errorf("config = %+v, want %+v", *res.serve, want)
			}
			if res.tuiURL != nil {
				t.Error("the TUI ran for serve")
			}
		})
	}
}

func TestServeUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"team without org", []string{"serve", "--preferred-team", "control-plane"}},
		{"team without slug", []string{"serve", "--preferred-team", "acme-co/"}},
		{"team with extra segment", []string{"serve", "--preferred-team", "acme-co/a/b"}},
		{"blank greylist login", []string{"serve", "--greylist", " "}},
		{"empty greylist login", []string{"serve", "--greylist", "a,,b"}},
		{"invalid log level", []string{"serve", "--log-level", "loud"}},
		{"invalid port", []string{"serve", "--nats-port", "http"}},
		{"invalid duration", []string{"serve", "--poll-interval", "soon"}},
		{"unknown flag", []string{"serve", "--nope"}},
		{"extra argument", []string{"serve", "now"}},
		{"tui flag on serve", []string{"serve", "--nats-url", "nats://x"}},
		{"unknown command", []string{"srve"}},
		{"unknown config subcommand", []string{"config", "show"}},
		{"extra argument to config create", []string{"config", "create", "now"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupEnv(t)
			res := runCLI(t, tt.args...)
			if !isUsage(res.err) {
				t.Fatalf("err = %v, want a usage error", res.err)
			}
			if res.serve != nil || res.tuiURL != nil {
				t.Error("a runner ran despite the usage error")
			}
			if !strings.Contains(res.stderr, "dev-inbox: ") || !strings.Contains(res.stderr, "--help") {
				t.Errorf("stderr lacks the error and the usage hint:\n%s", res.stderr)
			}
		})
	}
}

func TestServeKeepsDefaultGreylist(t *testing.T) {
	setupEnv(t)
	want := []string{"dependabot", "renovate"}
	if !reflect.DeepEqual(daemon.DefaultGreylist, want) {
		t.Fatalf("DefaultGreylist = %q, want %q", daemon.DefaultGreylist, want)
	}
	res := runCLI(t, "serve", "--greylist", "x", "--greylist", "y")
	if res.err != nil || !reflect.DeepEqual(res.serve.Greylist, []string{"x", "y"}) {
		t.Fatalf("greylist = %v, %v; want [x y]", res.serve, res.err)
	}
	if !reflect.DeepEqual(daemon.DefaultGreylist, want) {
		t.Errorf("DefaultGreylist mutated to %q", daemon.DefaultGreylist)
	}
}

func TestTUI(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "default", want: api.DefaultNATSURL},
		{name: "flag", args: []string{"--nats-url", "nats://example:4222"}, want: "nats://example:4222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupEnv(t)
			res := runCLI(t, tt.args...)
			if res.err != nil || res.tuiURL == nil || *res.tuiURL != tt.want {
				t.Errorf("url = %v, err = %v; want %q", res.tuiURL, res.err, tt.want)
			}
			if res.serve != nil {
				t.Error("the daemon ran for the TUI")
			}
		})
	}
}

func TestRuntimeErrorIsNotUsage(t *testing.T) {
	setupEnv(t)
	boom := errors.New("boom")
	for _, args := range [][]string{{}, {"serve"}} {
		res := runCLIContext(t, context.Background(), boom, args...)
		if !errors.Is(res.err, boom) || isUsage(res.err) {
			t.Errorf("run(%q) = %v, want the runner's error and no usage error", args, res.err)
		}
		if res.stderr != "dev-inbox: boom\n" {
			t.Errorf("run(%q) stderr = %q", args, res.stderr)
		}
	}
}

func TestRunnersGetTheContext(t *testing.T) {
	setupEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var seen []error
	r := runners{
		tui:   func(ctx context.Context, _ string) error { seen = append(seen, ctx.Err()); return nil },
		serve: func(ctx context.Context, _ daemon.Config) error { seen = append(seen, ctx.Err()); return nil },
	}
	for _, args := range [][]string{{}, {"serve"}} {
		if err := run(ctx, r, args, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || !errors.Is(seen[0], context.Canceled) || !errors.Is(seen[1], context.Canceled) {
		t.Errorf("runner contexts = %v, want both canceled", seen)
	}
}

func TestHelp(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{args: []string{"--help"}, want: []string{"serve", "config", "--nats-url", "--config"}},
		{args: []string{"-h"}, want: []string{"serve", "config"}},
		{args: []string{"serve", "-h"}, want: []string{
			"--state-dir", "--nats-host", "--nats-port", "--poll-interval", "--reconcile-interval", "--log-level",
			"--preferred-team", "--greylist", "(default [dependabot,renovate])", "--config"}},
		{args: []string{"config", "--help"}, want: []string{"create"}},
		{args: []string{"config"}, want: []string{"create"}},
		{args: []string{"config", "create", "-h"}, want: []string{"dev-inbox config create >"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			setupEnv(t)
			res := runCLI(t, tt.args...)
			if res.err != nil || res.serve != nil || res.tuiURL != nil {
				t.Fatalf("err = %v, serve = %v, tui = %v", res.err, res.serve, res.tuiURL)
			}
			for _, w := range tt.want {
				if !strings.Contains(res.stdout, w) {
					t.Errorf("help lacks %q:\n%s", w, res.stdout)
				}
			}
			if res.stderr != "" {
				t.Errorf("stderr = %q, want empty", res.stderr)
			}
		})
	}
}
