package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/jev"
	"github.com/roehrijn/dev-inbox/internal/jevplugin"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

var withKey = env(map[string]string{jev.APIKeyEnv: "secret"})

// runWith runs the command with args and returns the configuration handed to the plugin.
func runWith(t *testing.T, getenv func(string) string, args ...string) (jevplugin.Config, string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var got jevplugin.Config
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), func(_ context.Context, cfg jevplugin.Config) error {
		got = cfg
		return nil
	}, getenv, args, &stdout, &stderr)
	return got, stderr.String(), err
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
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
	if cfg.NATSURL != api.DefaultNATSURL || cfg.Threshold != 0.6 || cfg.Debounce != 2*time.Second ||
		cfg.Repos != nil || cfg.Jev == nil || cfg.Logger == nil {
		t.Errorf("got %+v", cfg)
	}
	want := map[string]int{
		jevplugin.IntentQuestionForMe: 40, jevplugin.IntentPushback: 30, jevplugin.IntentRereviewRequest: 30,
		jevplugin.IntentAddressed: 0, jevplugin.IntentAck: 0, jevplugin.IntentNotForMe: 0,
	}
	if !reflect.DeepEqual(cfg.Weights, want) {
		t.Errorf("weights %v, want %v", cfg.Weights, want)
	}
}

func TestConfigFile(t *testing.T) {
	path := writeConfig(t, `
serve:
  log-level: debug
  score:
    needs-me: 25
tui:
  nats-url: nats://tui:4222
plugins:
  other:
    anything: goes
  jev:
    nats-url: nats://jev:4222
    threshold: 0.75
    weights:
      question-for-me: 50
      ack: -5
    repos: [acme-co, roehrijn/dev-inbox]
    debounce: 500ms
`)
	cfg, _, err := runWith(t, withKey, "--config", path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NATSURL != "nats://jev:4222" || cfg.Threshold != 0.75 || cfg.Debounce != 500*time.Millisecond ||
		!reflect.DeepEqual(cfg.Repos, []string{"acme-co", "roehrijn/dev-inbox"}) {
		t.Errorf("got %+v", cfg)
	}
	if cfg.Weights[jevplugin.IntentQuestionForMe] != 50 || cfg.Weights[jevplugin.IntentAck] != -5 ||
		cfg.Weights[jevplugin.IntentPushback] != 30 {
		t.Errorf("weights %v: want overrides merged into the defaults", cfg.Weights)
	}
}

func TestNATSURLPrecedence(t *testing.T) {
	tuiOnly := writeConfig(t, "tui:\n  nats-url: nats://tui:4222\n")
	cfg, _, err := runWith(t, withKey, "--config", tuiOnly)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NATSURL != "nats://tui:4222" {
		t.Errorf("without plugins.jev.nats-url got %q, want tui.nats-url", cfg.NATSURL)
	}

	both := writeConfig(t, "tui:\n  nats-url: nats://tui:4222\nplugins:\n  jev:\n    nats-url: nats://jev:4222\n")
	cfg, _, err = runWith(t, withKey, "--config", both, "--nats-url", "nats://flag:4222")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NATSURL != "nats://flag:4222" {
		t.Errorf("with the flag got %q, want the flag", cfg.NATSURL)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "dev-inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dev-inbox", "config.yaml"),
		[]byte("plugins:\n  jev:\n    threshold: 0.9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got jevplugin.Config
	err := run(context.Background(), func(_ context.Context, cfg jevplugin.Config) error {
		got = cfg
		return nil
	}, withKey, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Threshold != 0.9 {
		t.Errorf("threshold %v, want 0.9 from the default config file", got.Threshold)
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
		{name: "missing API key", getenv: env(nil), want: []string{"TYPESAFE_API_KEY is not set"}},
		{name: "explicit config missing", args: []string{"--config", "/nonexistent/config.yaml"},
			want: []string{"does not exist"}},
		{name: "unknown keys", config: "plugins:\n  jev:\n    treshold: 0.5\n    weights:\n      questions: 1\n",
			want: []string{`unknown key "plugins.jev.treshold"`, `unknown key "plugins.jev.weights.questions"`}},
		{name: "intent name instead of weight key", config: "plugins:\n  jev:\n    weights:\n      question_for_me: 1\n",
			want: []string{`unknown key "plugins.jev.weights.question_for_me"`}},
		{name: "threshold out of range", config: "plugins:\n  jev:\n    threshold: 1.5\n",
			want: []string{"plugins.jev.threshold", "not between 0 and 1"}},
		{name: "fractional weight", config: "plugins:\n  jev:\n    weights:\n      ack: 1.5\n", want: []string{"1.5"}},
		{name: "quoted weight", config: "plugins:\n  jev:\n    weights:\n      ack: \"20\"\n", want: []string{"20"}},
		{name: "debounce without unit", config: "plugins:\n  jev:\n    debounce: 2\n",
			want: []string{"plugins.jev.debounce", "missing unit"}},
		{name: "negative debounce", config: "plugins:\n  jev:\n    debounce: -1s\n", want: []string{"negative"}},
		{name: "invalid repo", config: "plugins:\n  jev:\n    repos: [a/b/c]\n",
			want: []string{"plugins.jev.repos", `"a/b/c"`}},
		{name: "section not a mapping", config: "plugins:\n  jev: [1]\n", want: []string{"must be a mapping"}},
		{name: "invalid log level", args: []string{"--log-level", "loud"}, want: []string{`invalid log-level "loud"`}},
		{name: "argument", args: []string{"serve"}, want: []string{`unexpected argument "serve"`}},
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
			if !strings.Contains(stderr, "Run 'dev-inbox-jev --help' for usage.") {
				t.Errorf("stderr %q lacks the usage hint", stderr)
			}
		})
	}
}

func TestRunError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stderr bytes.Buffer
	err := run(context.Background(), func(context.Context, jevplugin.Config) error {
		return errors.New("boom")
	}, withKey, nil, &bytes.Buffer{}, &stderr)
	var usage usageError
	if err == nil || errors.As(err, &usage) {
		t.Fatalf("got %v, want a plain error", err)
	}
	if stderr.String() != "dev-inbox-jev: boom\n" {
		t.Errorf("stderr %q", stderr.String())
	}
}
