package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/roehrijn/dev-inbox/internal/derive"
)

func TestScoreTermsCoverEveryWeight(t *testing.T) {
	var w derive.Weights
	keys := make(map[string]bool)
	for i, term := range scoreTerms {
		if keys[term.key] {
			t.Errorf("duplicate key %q", term.key)
		}
		keys[term.key] = true
		if term.help == "" {
			t.Errorf("key %q has no help", term.key)
		}
		*term.field(&w) = i + 1
	}
	v := reflect.ValueOf(w)
	seen := make(map[int64]bool)
	for i := range v.NumField() {
		n := v.Field(i).Int()
		if n == 0 || seen[n] {
			t.Errorf("Weights.%s is bound to no key or to the same key as another field", v.Type().Field(i).Name)
		}
		seen[n] = true
	}
}

func TestScoreWeights(t *testing.T) {
	with := func(edit func(*derive.Weights)) derive.Weights {
		w := derive.DefaultWeights
		edit(&w)
		return w
	}
	tests := []struct {
		name string
		file string
		args []string
		want derive.Weights
	}{
		{name: "defaults without a file", want: derive.DefaultWeights},
		{name: "no score section", file: "serve:\n  nats-port: 4222\n", want: derive.DefaultWeights},
		{name: "empty score section", file: "serve:\n  score:\n", want: derive.DefaultWeights},
		{name: "empty score map", file: "serve:\n  score: {}\n", want: derive.DefaultWeights},
		{
			name: "partial override keeps the other defaults",
			file: "serve:\n  score:\n    urgent: 50\n    per-failing-check: -25\n",
			want: with(func(w *derive.Weights) { w.Urgent, w.PerFailingCheck = 50, -25 }),
		},
		{
			name: "zero disables a term",
			file: "serve:\n  score:\n    direct: 0\n    checks-success: 0\n    per-failing-check: 0\n    merge-conflict: 0\n",
			want: with(func(w *derive.Weights) { w.Direct, w.ChecksSuccess, w.PerFailingCheck, w.MergeConflict = 0, 0, 0, 0 }),
		},
		{
			name: "negative and positive in either direction",
			file: "serve:\n  score:\n    urgent: -5\n    greylist: 15\n    checks-running: -1\n",
			want: with(func(w *derive.Weights) { w.Urgent, w.Greylist, w.ChecksRunning = -5, 15, -1 }),
		},
		{
			name: "every key",
			file: "serve:\n  score:\n    urgent: 1\n    preferred-team: 2\n    greylist: 3\n    direct: 4\n" +
				"    checks-running: 5\n    checks-success: 6\n    per-failing-check: 7\n    merge-conflict: 8\n",
			want: derive.Weights{
				Urgent: 1, PreferredTeam: 2, Greylist: 3, Direct: 4, ChecksRunning: 5, ChecksSuccess: 6, PerFailingCheck: 7,
				MergeConflict: 8,
			},
		},
		{
			name: "flow style",
			file: "serve: {score: {urgent: 9}}\n",
			want: with(func(w *derive.Weights) { w.Urgent = 9 }),
		},
		{
			name: "other flags do not touch the weights",
			file: "serve:\n  score:\n    urgent: 9\n",
			args: []string{"--preferred-team", "acme/x", "--greylist", "a"},
			want: with(func(w *derive.Weights) { w.Urgent = 9 }),
		},
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
			if res.serve.Weights != tt.want {
				t.Errorf("weights = %+v, want %+v", res.serve.Weights, tt.want)
			}
		})
	}
}

func TestScoreDefaultsAreNotShared(t *testing.T) {
	setupEnv(t)
	want := derive.DefaultWeights
	res := runCLI(t, "serve", "--config", writeConfig(t, "serve:\n  score:\n    urgent: 99\n"))
	if res.err != nil || res.serve.Weights.Urgent != 99 {
		t.Fatalf("urgent = %v, err = %v; want 99", res.serve, res.err)
	}
	if derive.DefaultWeights != want {
		t.Errorf("DefaultWeights mutated to %+v", derive.DefaultWeights)
	}
}

func TestScoreHasNoFlags(t *testing.T) {
	setupEnv(t)
	for _, flag := range []string{"--urgent", "--score", "--score-urgent"} {
		res := runCLI(t, "serve", flag, "5")
		if !isUsage(res.err) || res.serve != nil {
			t.Errorf("serve %s: err = %v, want a usage error", flag, res.err)
		}
	}
	res := runCLI(t, "serve", "-h")
	if strings.Contains(res.stdout, "urgent") || strings.Contains(res.stdout, "per-failing-check") {
		t.Errorf("serve help mentions the score keys:\n%s", res.stdout)
	}
}

func TestConfigCreateScoreBlock(t *testing.T) {
	setupEnv(t)
	out := runCLI(t, "config", "create").stdout
	block := `
#   # Priority score weights in points; 0 disables a term, negative values subtract
#   score:
#     # Points when a PR-level linear.urgency annotation exists
#     urgent: 20

#     # Points when the author is a member of the preferred team
#     preferred-team: 30

#     # Points when the author is greylisted
#     greylist: -15

#     # Points when review is requested from you personally
#     direct: 10

#     # Points when any check is still running
#     checks-running: 5

#     # Points when the checks rollup is successful
#     checks-success: 20

#     # Points per failing check
#     per-failing-check: -10

#     # Points when the PR has merge conflicts
#     merge-conflict: -40

# tui:
`
	if !strings.Contains(out, block) {
		t.Errorf("template lacks the score block\n%s\nin\n%s", block, out)
	}
	if strings.Count(out, "#   score:\n") != 1 || strings.Index(out, "#   score:") > strings.Index(out, "# tui:") {
		t.Errorf("score block is not inside the serve section:\n%s", out)
	}
	for _, term := range scoreTerms {
		if !strings.Contains(out, "\n#     # "+term.help+"\n#     "+term.key+": ") {
			t.Errorf("template lacks a commented %s", term.key)
		}
	}
}
