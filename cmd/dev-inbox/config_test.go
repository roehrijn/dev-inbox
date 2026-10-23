package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/daemon"
	"github.com/roehrijn/dev-inbox/internal/derive"
)

const fullServeFile = `
serve:
  state-dir: /file/state
  nats-host: 10.0.0.1
  nats-port: 4222
  poll-interval: 2m
  reconcile-interval: 30s
  log-level: warn
  preferred-teams:
    acme/file-team: 25
    acme/other: -5
  greylist: [bot-a, bot-b]
  role-teams: [acme/staff, acme/leads]
  score:
    urgent: 1
    greylist: 3
    direct: 4
    checks-running: 5
    checks-success: 6
    per-failing-check: 7
    merge-conflict: 8
    needs-me: 9
    rereview: 10
    thread-reply: 11
    mentioned: 12
    team-review: 13
    role-review: 15
    draft: 14
`

func fileServeConfig() daemon.Config {
	return daemon.Config{
		StateDir:          "/file/state",
		NATSHost:          "10.0.0.1",
		NATSPort:          4222,
		PollInterval:      2 * time.Minute,
		ReconcileInterval: 30 * time.Second,
		PreferredTeams:    map[string]int{"acme/file-team": 25, "acme/other": -5},
		Greylist:          []string{"bot-a", "bot-b"},
		RoleTeams:         []string{"acme/staff", "acme/leads"},
		Weights: derive.Weights{
			Urgent: 1, Greylist: 3, Direct: 4, ChecksRunning: 5, ChecksSuccess: 6, PerFailingCheck: 7,
			MergeConflict: 8, NeedsMe: 9, Rereview: 10, ThreadReply: 11, Mentioned: 12, TeamReview: 13,
			RoleReview: 15, Draft: 14,
		},
	}
}

func TestServePrecedence(t *testing.T) {
	tests := []struct {
		name string
		file string
		args []string
		want func() daemon.Config
	}{
		{
			name: "defaults without a file",
			want: defaultServeConfig,
		},
		{
			name: "file over defaults",
			file: fullServeFile,
			want: fileServeConfig,
		},
		{
			name: "partial file keeps the other defaults",
			file: "serve:\n  nats-port: 4222\n  poll-interval: 90s\n",
			want: func() daemon.Config {
				c := defaultServeConfig()
				c.NATSPort, c.PollInterval = 4222, 90*time.Second
				return c
			},
		},
		{
			name: "flags over file",
			file: fullServeFile,
			args: []string{"--nats-port", "5555", "--poll-interval", "10m", "--greylist", "x",
				"--preferred-teams", "acme/flag-team=7", "--state-dir", "/flag/state", "--role-teams", "acme/flag-role"},
			want: func() daemon.Config {
				c := fileServeConfig()
				c.NATSPort, c.PollInterval, c.Greylist = 5555, 10*time.Minute, []string{"x"}
				c.RoleTeams = []string{"acme/flag-role"}
				c.PreferredTeams, c.StateDir = map[string]int{"acme/flag-team": 7}, "/flag/state"
				return c
			},
		},
		{
			name: "flag equal to its default still beats the file",
			file: fullServeFile,
			args: []string{"--nats-port", "14222", "--poll-interval", "1m", "--greylist", "dependabot,renovate"},
			want: func() daemon.Config {
				c := fileServeConfig()
				c.NATSPort, c.PollInterval = 14222, time.Minute
				c.Greylist = []string{"dependabot", "renovate"}
				return c
			},
		},
		{
			name: "tui section does not affect serve",
			file: "tui:\n  nats-url: nats://elsewhere:1\n",
			want: defaultServeConfig,
		},
		{
			name: "empty section",
			file: "serve:\n",
			want: defaultServeConfig,
		},
		{
			name: "empty file",
			file: "",
			want: defaultServeConfig,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupEnv(t)
			args := []string{"serve"}
			if tt.file != "" || tt.name == "empty file" {
				args = append(args, "--config", writeConfig(t, tt.file))
			}
			res := runCLI(t, append(args, tt.args...)...)
			if res.err != nil || res.serve == nil {
				t.Fatalf("err = %v", res.err)
			}
			if want := tt.want(); !sameConfig(*res.serve, want) {
				t.Errorf("config = %+v, want %+v", *res.serve, want)
			}
		})
	}
}

func TestServeLogLevelFromFile(t *testing.T) {
	setupEnv(t)
	for level, wantDebug := range map[string]bool{"debug": true, "info": false} {
		res := runCLI(t, "serve", "--config", writeConfig(t, "serve:\n  log-level: "+level+"\n"))
		if res.err != nil {
			t.Fatal(res.err)
		}
		if got := res.serve.Logger.Enabled(t.Context(), -4); got != wantDebug {
			t.Errorf("log-level %s: debug enabled = %v, want %v", level, got, wantDebug)
		}
	}
	res := runCLI(t, "serve", "--log-level", "info", "--config", writeConfig(t, "serve:\n  log-level: debug\n"))
	if res.err != nil || res.serve.Logger.Enabled(t.Context(), -4) {
		t.Errorf("flag --log-level info should beat the file's debug, err = %v", res.err)
	}
}

func TestTUIPrecedence(t *testing.T) {
	setupEnv(t)
	file := writeConfig(t, "tui:\n  nats-url: nats://file:1\n")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"file over default", []string{"--config", file}, "nats://file:1"},
		{"flag over file", []string{"--config", file, "--nats-url", "nats://flag:2"}, "nats://flag:2"},
		{"serve section does not affect the TUI", []string{"--config", writeConfig(t, fullServeFile)}, api.DefaultNATSURL},
	}
	for _, tt := range tests {
		res := runCLI(t, tt.args...)
		if res.err != nil || res.tuiURL == nil || *res.tuiURL != tt.want {
			t.Errorf("%s: url = %v, err = %v; want %q", tt.name, res.tuiURL, res.err, tt.want)
		}
	}
}

func TestEmptyGreylist(t *testing.T) {
	withLogins := writeConfig(t, "serve:\n  greylist: [bot-a]\n")
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"empty list in the file", []string{"--config", writeConfig(t, "serve:\n  greylist: []\n")}, nil},
		{"empty flag value", []string{"--greylist="}, nil},
		{"empty flag argument", []string{"--greylist", ""}, nil},
		{"empty flag over file", []string{"--config", withLogins, "--greylist="}, nil},
		{"file over defaults", []string{"--config", withLogins}, []string{"bot-a"}},
		{"flag over empty file list", []string{"--config", writeConfig(t, "serve:\n  greylist: []\n"),
			"--greylist", "x"}, []string{"x"}},
		{"empty flag then a login", []string{"--greylist=", "--greylist", "x"}, []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupEnv(t)
			res := runCLI(t, append([]string{"serve"}, tt.args...)...)
			if res.err != nil {
				t.Fatal(res.err)
			}
			want := defaultServeConfig()
			want.Greylist = tt.want
			if !sameConfig(*res.serve, want) {
				t.Errorf("greylist = %q, want %q", res.serve.Greylist, tt.want)
			}
		})
	}
}

func TestPreferredTeams(t *testing.T) {
	const file = "serve:\n  preferred-teams:\n    acme/file-a: 30\n    acme/file-b: 10\n"
	tests := []struct {
		name string
		file string
		args []string
		want map[string]int
	}{
		{name: "none", want: nil},
		{name: "file alone", file: file, want: map[string]int{"acme/file-a": 30, "acme/file-b": 10}},
		{name: "empty map in the file", file: "serve:\n  preferred-teams: {}\n", want: nil},
		{name: "flow style in the file", file: "serve: {preferred-teams: {acme/x: 5}}\n",
			want: map[string]int{"acme/x": 5}},
		{name: "negative points in the file", file: "serve:\n  preferred-teams:\n    acme/x: -20\n",
			want: map[string]int{"acme/x": -20}},
		{name: "file keys are lowercased by viper", file: "serve:\n  preferred-teams:\n    Acme/Eng-Team: 3\n",
			want: map[string]int{"acme/eng-team": 3}},
		{name: "flag replaces the file map", file: file, args: []string{"--preferred-teams", "acme/flag=7"},
			want: map[string]int{"acme/flag": 7}},
		{name: "repeated flag merges and still replaces the file map", file: file,
			args: []string{"--preferred-teams", "acme/flag-a=7", "--preferred-teams", "acme/flag-b=8"},
			want: map[string]int{"acme/flag-a": 7, "acme/flag-b": 8}},
		{name: "flag over a file without teams", file: "serve:\n  nats-port: 4222\n",
			args: []string{"--preferred-teams", "acme/flag=7"}, want: map[string]int{"acme/flag": 7}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupEnv(t)
			args := []string{"serve"}
			if tt.file != "" {
				args = append(args, "--config", writeConfig(t, tt.file))
			}
			res := runCLI(t, append(args, tt.args...)...)
			if res.err != nil || res.serve == nil {
				t.Fatalf("err = %v", res.err)
			}
			want := defaultServeConfig()
			want.PreferredTeams = tt.want
			if tt.file == "serve:\n  nats-port: 4222\n" {
				want.NATSPort = 4222
			}
			if !sameConfig(*res.serve, want) {
				t.Errorf("config = %+v, want %+v", *res.serve, want)
			}
		})
	}
}

func TestConfigFileLocation(t *testing.T) {
	t.Run("missing default file is fine", func(t *testing.T) {
		setupEnv(t)
		for _, args := range [][]string{{"serve"}, {}} {
			if res := runCLI(t, args...); res.err != nil {
				t.Errorf("run(%q) = %v", args, res.err)
			}
		}
	})
	t.Run("missing explicit file is an error", func(t *testing.T) {
		setupEnv(t)
		missing := filepath.Join(t.TempDir(), "nope.yaml")
		for _, args := range [][]string{{"serve", "--config", missing}, {"--config", missing}, {"--config=" + missing}} {
			res := runCLI(t, args...)
			if !isUsage(res.err) || !strings.Contains(res.err.Error(), missing) {
				t.Errorf("run(%q) = %v, want a usage error naming %s", args, res.err, missing)
			}
			if res.serve != nil || res.tuiURL != nil {
				t.Errorf("run(%q) ran despite the missing file", args)
			}
		}
	})
	t.Run("empty explicit path is an error", func(t *testing.T) {
		setupEnv(t)
		if res := runCLI(t, "serve", "--config="); res.err == nil {
			t.Error("want an error")
		}
	})
	t.Run("XDG_CONFIG_HOME", func(t *testing.T) {
		home := setupEnv(t)
		dir := filepath.Join(home, "xdg-config", "dev-inbox")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("serve:\n  nats-port: 4321\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := runCLI(t, "serve")
		if res.err != nil || res.serve.NATSPort != 4321 {
			t.Fatalf("port = %v, err = %v; want 4321 from the default file", res.serve, res.err)
		}
		res = runCLI(t, "serve", "--config", writeConfig(t, "serve:\n  nats-port: 1234\n"))
		if res.err != nil || res.serve.NATSPort != 1234 {
			t.Errorf("port = %v, err = %v; want 1234 from the explicit file", res.serve, res.err)
		}
	})
	t.Run("home fallback", func(t *testing.T) {
		home := setupEnv(t)
		for _, xdg := range []string{"", "relative/dir"} {
			t.Setenv("XDG_CONFIG_HOME", xdg)
			if got, want := defaultConfigPath(), filepath.Join(home, ".config", "dev-inbox", "config.yaml"); got != want {
				t.Errorf("XDG_CONFIG_HOME=%q: path = %q, want %q", xdg, got, want)
			}
		}
		t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
		if got, want := defaultConfigPath(), "/xdg/config/dev-inbox/config.yaml"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	})
	t.Run("the default path is shown in the help", func(t *testing.T) {
		home := setupEnv(t)
		res := runCLI(t, "--help")
		if want := filepath.Join(home, "xdg-config", "dev-inbox", "config.yaml"); !strings.Contains(res.stdout, want) {
			t.Errorf("help lacks %q:\n%s", want, res.stdout)
		}
	})
}

func TestConfigFileErrors(t *testing.T) {
	type fileError struct {
		name    string
		file    string
		wantMsg string
	}
	// Both commands reject a broken file, whichever section they use.
	structural := []fileError{
		{"unknown key", "serve:\n  nats-prot: 1\n", `unknown key "serve.nats-prot"`},
		{"unknown key in the other section", "tui:\n  nats-urls: x\n", `unknown key "tui.nats-urls"`},
		{"flag of another section", "serve:\n  nats-url: x\n", `unknown key "serve.nats-url"`},
		{"unknown section", "server:\n  nats-port: 1\n", `unknown key "server.nats-port"`},
		{"unknown top-level key", "nats-port: 1\n", `unknown key "nats-port"`},
		{"invalid yaml", "serve: [unclosed\n", "invalid config"},
		{"not a mapping", "- serve\n", "invalid config"},
		{"duplicate key", "serve:\n  nats-port: 1\n  nats-port: 2\n", "invalid config"},
		{"section is a scalar", "serve: 5\n", "invalid config"},
		{"port is not a number", "serve:\n  nats-port: abc\n", "nats-port"},
		{"greylist is a map", "serve:\n  greylist: {a: b}\n", "invalid config"},
		{"unknown score key", "serve:\n  score:\n    urgnt: 1\n", `unknown key "serve.score.urgnt"`},
		{"score key in the serve section", "serve:\n  urgent: 1\n", `unknown key "serve.urgent"`},
		{"score key in the tui section", "tui:\n  score:\n    urgent: 1\n", `unknown key "tui.score.urgent"`},
		{"score is a scalar", "serve:\n  score: 5\n", "invalid config"},
		{"score is a list", "serve:\n  score: [1]\n", "invalid config"},
		{"preferred-teams is a scalar", "serve:\n  preferred-teams: acme/x\n", "invalid config"},
		{"preferred-teams is a list", "serve:\n  preferred-teams: [acme/x]\n", "invalid config"},
		{"old preferred-team key", "serve:\n  preferred-team: acme/x\n",
			`unknown key "serve.preferred-team": replaced by serve.preferred-teams (org/slug: points)`},
		{"old preferred-team score key", "serve:\n  score:\n    preferred-team: 30\n",
			`unknown key "serve.score.preferred-team": replaced by serve.preferred-teams (org/slug: points)`},
		{"preferred-team in the tui section", "tui:\n  preferred-team: acme/x\n", `unknown key "tui.preferred-team"`},
		{"preferred-teams in the tui section", "tui:\n  preferred-teams: {acme/x: 1}\n",
			`unknown key "tui.preferred-teams.acme/x"`},
	}
	serveOnly := []fileError{
		{"duration without unit", "serve:\n  poll-interval: 60\n", "invalid poll-interval"},
		{"invalid duration", "serve:\n  reconcile-interval: soon\n", "invalid reconcile-interval"},
		{"invalid log level", "serve:\n  log-level: loud\n", "invalid log-level"},
		{"team without org", "serve:\n  preferred-teams:\n    control-plane: 1\n", `invalid preferred-teams: "control-plane"`},
		{"team without slug", "serve:\n  preferred-teams:\n    acme/: 1\n", `invalid preferred-teams: "acme/"`},
		{"team with extra segment", "serve:\n  preferred-teams:\n    acme/a/b: 1\n", `invalid preferred-teams: "acme/a/b"`},
		{"one invalid team among valid ones", "serve:\n  preferred-teams:\n    acme/ok: 1\n    nope: 2\n",
			`invalid preferred-teams: "nope"`},
		{"fractional points", "serve:\n  preferred-teams:\n    acme/x: 1.5\n", "invalid preferred-teams acme/x: points 1.5"},
		{"quoted points", "serve:\n  preferred-teams:\n    acme/x: '20'\n", "invalid preferred-teams acme/x: points 20"},
		{"null points", "serve:\n  preferred-teams:\n    acme/x:\n", "invalid preferred-teams acme/x"},
		{"boolean points", "serve:\n  preferred-teams:\n    acme/x: true\n", "invalid preferred-teams acme/x: points true"},
		{"role team without org", "serve:\n  role-teams: [staff]\n", `invalid role-teams: "staff"`},
		{"huge points", "serve:\n  preferred-teams:\n    acme/x: 99999999999999999999\n", "invalid preferred-teams acme/x"},
		{"blank greylist login", "serve:\n  greylist: [a, ' ']\n", "invalid greylist"},
		{"fractional score", "serve:\n  score:\n    urgent: 1.5\n", "invalid serve.score.urgent 1.5"},
		{"whole float score", "serve:\n  score:\n    direct: 10.0\n", "invalid serve.score.direct"},
		{"quoted score", "serve:\n  score:\n    greylist: '-15'\n", "invalid serve.score.greylist"},
		{"word score", "serve:\n  score:\n    checks-running: many\n", "invalid serve.score.checks-running many"},
		{"boolean score", "serve:\n  score:\n    checks-success: true\n", "invalid serve.score.checks-success"},
		{"list score", "serve:\n  score:\n    urgent: [1]\n", "invalid serve.score.urgent"},
		{"huge score", "serve:\n  score:\n    per-failing-check: 99999999999999999999\n",
			"invalid serve.score.per-failing-check"},
	}
	check := func(t *testing.T, tt fileError, args ...string) {
		t.Helper()
		setupEnv(t)
		res := runCLI(t, args...)
		if !isUsage(res.err) || !strings.Contains(res.err.Error(), tt.wantMsg) {
			t.Errorf("err = %v, want a usage error containing %q", res.err, tt.wantMsg)
		}
		if res.serve != nil || res.tuiURL != nil {
			t.Error("a runner ran despite the error")
		}
	}
	for _, tt := range structural {
		t.Run("serve/"+tt.name, func(t *testing.T) { check(t, tt, "serve", "--config", writeConfig(t, tt.file)) })
		t.Run("tui/"+tt.name, func(t *testing.T) { check(t, tt, "--config", writeConfig(t, tt.file)) })
	}
	for _, tt := range serveOnly {
		t.Run("serve/"+tt.name, func(t *testing.T) { check(t, tt, "serve", "--config", writeConfig(t, tt.file)) })
	}
	t.Run("all unknown keys are reported", func(t *testing.T) {
		setupEnv(t)
		res := runCLI(t, "serve", "--config", writeConfig(t, "serve:\n  b: 1\n  a: 2\n  score:\n    d: 4\ntui:\n  c: 3\n"))
		for _, key := range []string{"serve.a", "serve.b", "serve.score.d", "tui.c"} {
			if !isUsage(res.err) || !strings.Contains(res.err.Error(), `"`+key+`"`) {
				t.Errorf("err = %v, want it to name %s", res.err, key)
			}
		}
	})
}

func TestInvalidDefaultFileIsAnError(t *testing.T) {
	home := setupEnv(t)
	dir := filepath.Join(home, "xdg-config", "dev-inbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("serve:\n  nats-prot: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := runCLI(t, "serve"); !isUsage(res.err) {
		t.Errorf("err = %v, want a usage error", res.err)
	}
}

func TestPluginsKeyIsRejected(t *testing.T) {
	for _, file := range []string{
		"plugins:\n  jev:\n    threshold: 0.6\n",
		"plugins: 5\n",
	} {
		setupEnv(t)
		res := runCLI(t, "serve", "--config", writeConfig(t, file))
		if !isUsage(res.err) || !strings.Contains(res.err.Error(), `unknown key "plugins`) || res.serve != nil {
			t.Errorf("config %q: err = %v, want a usage error naming plugins", file, res.err)
		}
	}
}

func TestStateDirHomeExpansion(t *testing.T) {
	home := setupEnv(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"file", []string{"--config", writeConfig(t, "serve:\n  state-dir: ~/from-file\n")},
			filepath.Join(home, "from-file")},
		{"flag", []string{"--state-dir", "~/from-flag"}, filepath.Join(home, "from-flag")},
		{"flag over file", []string{"--config", writeConfig(t, "serve:\n  state-dir: ~/from-file\n"),
			"--state-dir=~/from-flag"}, filepath.Join(home, "from-flag")},
		{"nested", []string{"--state-dir", "~/a/b"}, filepath.Join(home, "a", "b")},
		{"absolute path is kept", []string{"--state-dir", "/abs/~/x"}, "/abs/~/x"},
		{"other user is kept", []string{"--state-dir", "~other/x"}, "~other/x"},
		{"empty state dir is kept for the daemon default", []string{"--state-dir", ""}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runCLI(t, append([]string{"serve"}, tt.args...)...)
			if res.err != nil || res.serve.StateDir != tt.want {
				t.Errorf("state dir = %v, err = %v; want %q", res.serve, res.err, tt.want)
			}
		})
	}
}

func TestConfigCreate(t *testing.T) {
	setupEnv(t)
	res := runCLI(t, "config", "create")
	if res.err != nil || res.stderr != "" || res.serve != nil || res.tuiURL != nil {
		t.Fatalf("err = %v, stderr = %q", res.err, res.stderr)
	}
	out := res.stdout

	wantTeams := "\n#   # Rank PRs of members of these GitHub teams by org/slug=points; repeatable, replaces the config " +
		"file's teams; negative points rank down\n#   preferred-teams: {}\n"
	if !strings.Contains(out, wantTeams) {
		t.Errorf("output lacks\n%s\nin\n%s", wantTeams, out)
	}
	if want := "\n#   # Listen port of the embedded NATS server\n#   nats-port: 14222\n"; !strings.Contains(out, want) {
		t.Errorf("output lacks\n%s\nin\n%s", want, out)
	}
	if !strings.HasPrefix(out, "# serve:\n") || !strings.Contains(out, "\n# tui:\n#   # NATS URL of the daemon\n") {
		t.Errorf("sections out of order or unlabeled:\n%s", out)
	}
	for _, sec := range sections {
		for _, f := range sec.flags() {
			if !strings.Contains(out, "\n#   "+f.Name+": ") {
				t.Errorf("%s.%s missing from\n%s", sec.name, f.Name, out)
			}
		}
	}

	t.Run("ignores the loaded config", func(t *testing.T) {
		withFile := runCLI(t, "config", "create", "--config", writeConfig(t, fullServeFile))
		if withFile.err != nil || withFile.stdout != out {
			t.Errorf("output changed with a config file: err = %v\n%s", withFile.err, withFile.stdout)
		}
	})
}

func TestConfigCreateRoundTrip(t *testing.T) {
	for _, stateHome := range []string{"/xdg", "/odd dir: #1 [x]"} {
		t.Run(stateHome, func(t *testing.T) {
			setupEnv(t)
			t.Setenv("XDG_STATE_HOME", stateHome)
			want := defaultServeConfig()
			want.StateDir = stateHome + "/dev-inbox"

			tmpl := runCLI(t, "config", "create").stdout
			uncommented := uncomment(t, tmpl)

			var doc map[string]map[string]any
			if err := yaml.Unmarshal([]byte(uncommented), &doc); err != nil {
				t.Fatalf("uncommented template is not valid YAML: %v\n%s", err, uncommented)
			}
			for _, sec := range sections {
				for _, f := range sec.flags() {
					if _, ok := doc[sec.name][f.Name]; !ok {
						t.Errorf("uncommented template lacks %s.%s", sec.name, f.Name)
					}
				}
			}
			if teams, ok := doc[serveSection.name][preferredTeamsKey].(map[string]any); !ok || len(teams) != 0 {
				t.Errorf("uncommented preferred-teams = %#v, want an empty map", doc[serveSection.name][preferredTeamsKey])
			}
			score, _ := doc[serveSection.name][scoreKey].(map[string]any)
			for _, term := range scoreTerms {
				if _, ok := score[term.key]; !ok {
					t.Errorf("uncommented template lacks %s.%s.%s", serveSection.name, scoreKey, term.key)
				}
			}

			path := writeConfig(t, uncommented)
			serve := runCLI(t, "serve", "--config", path)
			if serve.err != nil || !sameConfig(*serve.serve, want) {
				t.Errorf("serve config = %+v, err = %v; want %+v", serve.serve, serve.err, want)
			}
			tui := runCLI(t, "--config", path)
			if tui.err != nil || *tui.tuiURL != api.DefaultNATSURL {
				t.Errorf("tui url = %v, err = %v; want %q", tui.tuiURL, tui.err, api.DefaultNATSURL)
			}

			// The template is read, not merely accepted.
			edited := strings.Replace(uncommented, "preferred-teams: {}", "preferred-teams: {acme/x: 4, acme/y: -2}", 1)
			edited = strings.Replace(edited, "nats-port: 14222", "nats-port: 5555", 1)
			edited = strings.Replace(edited, "greylist: [dependabot, renovate]", "greylist: []", 1)
			edited = strings.Replace(edited, "urgent: 20", "urgent: 0", 1)
			edited = strings.Replace(edited, "per-failing-check: -10", "per-failing-check: -3", 1)
			serve = runCLI(t, "serve", "--config", writeConfig(t, edited))
			want.NATSPort, want.Greylist = 5555, nil
			want.PreferredTeams = map[string]int{"acme/x": 4, "acme/y": -2}
			want.Weights.Urgent, want.Weights.PerFailingCheck = 0, -3
			if serve.err != nil || !sameConfig(*serve.serve, want) {
				t.Errorf("edited serve config = %+v, err = %v; want %+v", serve.serve, serve.err, want)
			}
		})
	}
}

func TestConfigCreateTemplateLoadsAsIs(t *testing.T) {
	setupEnv(t)
	path := writeConfig(t, runCLI(t, "config", "create").stdout)
	res := runCLI(t, "serve", "--config", path)
	if res.err != nil || !sameConfig(*res.serve, defaultServeConfig()) {
		t.Errorf("config = %+v, err = %v; want the defaults", res.serve, res.err)
	}
}

// uncomment strips the leading "# " of each line of a config template.
func uncomment(t *testing.T, tmpl string) string {
	t.Helper()
	lines := strings.Split(tmpl, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		rest, ok := strings.CutPrefix(line, "# ")
		if !ok {
			t.Fatalf("template line %q does not start with \"# \"", line)
		}
		lines[i] = rest
	}
	return strings.Join(lines, "\n")
}
