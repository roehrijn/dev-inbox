package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/linear"
	"github.com/roehrijn/dev-inbox/internal/linearplugin"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

var withKey = env(map[string]string{linear.APIKeyEnv: "secret"})

// runWith runs the command with args and returns the configuration handed to the plugin.
func runWith(t *testing.T, getenv func(string) string, args ...string) (linearplugin.Config, string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var got linearplugin.Config
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), func(_ context.Context, cfg linearplugin.Config) error {
		got = cfg
		return nil
	}, getenv, args, &stdout, &stderr)
	return got, stderr.String(), err
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "linear.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	cfg, _, err := runWith(t, withKey)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NATSURL != api.DefaultNATSURL || cfg.HorizonDays != linearplugin.DefaultHorizonDays ||
		cfg.SweepInterval != linearplugin.DefaultSweepInterval || cfg.Linear == nil || cfg.Logger == nil {
		t.Errorf("got %+v", cfg)
	}
}

func TestConfigFile(t *testing.T) {
	path := writeConfig(t, `
nats-url: nats://linear:4222
horizon-days: 3
sweep-interval: 30m
`)
	cfg, _, err := runWith(t, withKey, "--config", path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NATSURL != "nats://linear:4222" || cfg.HorizonDays != 3 || cfg.SweepInterval != 30*time.Minute {
		t.Errorf("got %+v", cfg)
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
			cfg, _, err := runWith(t, withKey, "--config", writeConfig(t, content))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.NATSURL != api.DefaultNATSURL || cfg.HorizonDays != linearplugin.DefaultHorizonDays {
				t.Errorf("got %+v, want the defaults", cfg)
			}
		})
	}
}

func TestZeroHorizonDays(t *testing.T) {
	cfg, _, err := runWith(t, withKey, "--config", writeConfig(t, "horizon-days: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HorizonDays != 0 {
		t.Errorf("horizon days %d, want 0", cfg.HorizonDays)
	}
}

func TestNATSURLPrecedence(t *testing.T) {
	path := writeConfig(t, "nats-url: nats://linear:4222\n")
	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"config file":      {[]string{"--config", path}, "nats://linear:4222"},
		"flag over config": {[]string{"--config", path, "--nats-url", "nats://flag:4222"}, "nats://flag:4222"},
		"default":          {nil, api.DefaultNATSURL},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _, err := runWith(t, withKey, tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.NATSURL != tt.want {
				t.Errorf("got %q, want %q", cfg.NATSURL, tt.want)
			}
		})
	}
}

func TestLogLevelPrecedence(t *testing.T) {
	path := writeConfig(t, "log-level: error\n")
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
			err := run(context.Background(), func(_ context.Context, cfg linearplugin.Config) error {
				cfg.Logger.Debug("probe")
				return nil
			}, withKey, tt.args, &bytes.Buffer{}, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(stderr.String(), "probe"); got != tt.wantLog {
				t.Errorf("debug line logged %v, want %v: %q", got, tt.wantLog, stderr.String())
			}
		})
	}
}

func TestAPIKeyNotLogged(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stderr bytes.Buffer
	err := run(context.Background(), func(_ context.Context, cfg linearplugin.Config) error {
		cfg.Logger.Info("started")
		return nil
	}, withKey, []string{"--log-level", "debug"}, &bytes.Buffer{}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "secret") {
		t.Errorf("stderr leaks the API key: %q", stderr.String())
	}
}

func TestDefaultConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "dev-inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]string{
		"linear.yaml": "horizon-days: 14\n",
		"config.yaml": "horizon-days: 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "dev-inbox", file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var got linearplugin.Config
	err := run(context.Background(), func(_ context.Context, cfg linearplugin.Config) error {
		got = cfg
		return nil
	}, withKey, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if got.HorizonDays != 14 {
		t.Errorf("horizon days %d, want 14 from linear.yaml", got.HorizonDays)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name   string
		config string // written to a file passed with --config; "" passes no --config
		args   []string
		getenv func(string) string
		want   []string
	}{
		{name: "missing API key", getenv: env(nil), want: []string{"LINEAR_API_KEY is not set"}},
		{name: "blank API key", getenv: env(map[string]string{linear.APIKeyEnv: "  "}),
			want: []string{"LINEAR_API_KEY is not set"}},
		{name: "explicit config missing", args: []string{"--config", "/nonexistent/config.yaml"},
			want: []string{"does not exist"}},
		{name: "unknown keys", config: "horizon: 1\napi-key: x\n",
			want: []string{`unknown key "horizon"`, `unknown key "api-key"`}},
		{name: "core-style plugins section", config: "plugins:\n  linear:\n    horizon-days: 3\n",
			want: []string{`unknown key "plugins"`}},
		{name: "core-style tui section", config: "tui:\n  nats-url: nats://tui:4222\n",
			want: []string{`unknown key "tui"`}},
		{name: "non-mapping document", config: "[1, 2]\n", want: []string{"the document must be a mapping"}},
		{name: "scalar document", config: "foo\n", want: []string{"the document must be a mapping"}},
		{name: "fractional horizon", config: "horizon-days: 1.5\n", want: []string{"linear.yaml: horizon-days", "1.5"}},
		{name: "quoted horizon", config: "horizon-days: \"7\"\n", want: []string{"horizon-days"}},
		{name: "negative horizon", config: "horizon-days: -1\n", want: []string{"horizon-days", "negative"}},
		{name: "sweep interval without unit", config: "sweep-interval: 5\n",
			want: []string{"sweep-interval", "missing unit"}},
		{name: "zero sweep interval", config: "sweep-interval: 0s\n", want: []string{"sweep-interval", "not positive"}},
		{name: "negative sweep interval", config: "sweep-interval: -1h\n", want: []string{"not positive"}},
		{name: "invalid log level", args: []string{"--log-level", "loud"}, want: []string{`invalid log-level "loud"`}},
		{name: "argument", args: []string{"serve"}, want: []string{`unexpected argument "serve"`}},
		{name: "unknown flag", args: []string{"--bogus"}, want: []string{"unknown flag"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.args
			if tt.config != "" {
				args = append([]string{"--config", writeConfig(t, tt.config)}, args...)
			}
			getenv := tt.getenv
			if getenv == nil {
				getenv = withKey
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
			if !strings.Contains(stderr, "Run 'dev-inbox-linear --help' for usage.") {
				t.Errorf("stderr %q lacks the usage hint", stderr)
			}
		})
	}
}

func TestRunError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stderr bytes.Buffer
	err := run(context.Background(), func(context.Context, linearplugin.Config) error {
		return errors.New("boom")
	}, withKey, nil, &bytes.Buffer{}, &stderr)
	var usage usageError
	if err == nil || errors.As(err, &usage) {
		t.Fatalf("got %v, want a plain error", err)
	}
	if stderr.String() != "dev-inbox-linear: boom\n" {
		t.Errorf("stderr %q", stderr.String())
	}
}
