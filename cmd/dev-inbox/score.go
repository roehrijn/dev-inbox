package main

import (
	"fmt"
	"strings"

	"github.com/roehrijn/dev-inbox/internal/derive"
)

// scoreKey is the key of the score weights below the serve section. They have no flags.
const scoreKey = "score"

// scoreTerm binds a key of the serve.score section to its weight; the keys, the validation and the
// config template all derive from scoreTerms.
type scoreTerm struct {
	key   string
	field func(*derive.Weights) *int
	help  string
}

var scoreTerms = []scoreTerm{
	{"urgent", func(w *derive.Weights) *int { return &w.Urgent },
		"Points when a PR-level " + derive.UrgencyKey + " annotation exists"},
	{"greylist", func(w *derive.Weights) *int { return &w.Greylist },
		"Points when the author is greylisted"},
	{"direct", func(w *derive.Weights) *int { return &w.Direct },
		"Points when review is requested from you personally"},
	{"checks-running", func(w *derive.Weights) *int { return &w.ChecksRunning },
		"Points when any check is still running"},
	{"checks-success", func(w *derive.Weights) *int { return &w.ChecksSuccess },
		"Points when the checks rollup is successful"},
	{"per-failing-check", func(w *derive.Weights) *int { return &w.PerFailingCheck },
		"Points per failing check"},
	{"merge-conflict", func(w *derive.Weights) *int { return &w.MergeConflict },
		"Points when the PR has merge conflicts"},
	{"draft", func(w *derive.Weights) *int { return &w.Draft },
		"Points when the PR is a draft"},
	{"needs-me", func(w *derive.Weights) *int { return &w.NeedsMe },
		"Points when another author's PR has an attention reason; such PRs are never hidden"},
	{"rereview", func(w *derive.Weights) *int { return &w.Rereview },
		"Points when another author's PR changed since your review"},
	{"thread-reply", func(w *derive.Weights) *int { return &w.ThreadReply },
		"Points when someone replied in a review thread you started or joined"},
	{"mentioned", func(w *derive.Weights) *int { return &w.Mentioned },
		"Points when someone mentioned you after your last activity"},
	{"team-review", func(w *derive.Weights) *int { return &w.TeamReview },
		"Points when a review is requested from one of your teams and the PR waits for a reviewer"},
}

// scoreWeights overlays the values of the serve.score section on derive.DefaultWeights. Keys that are not
// present keep their default; unknown keys have been rejected by checkKeys already.
func scoreWeights(raw map[string]any) (derive.Weights, error) {
	w := derive.DefaultWeights
	for _, t := range scoreTerms {
		val, ok := raw[t.key]
		if !ok {
			continue
		}
		n, ok := val.(int)
		if !ok {
			return w, usagef("invalid %s.%s.%s %v: want an integer", serveSection.name, scoreKey, t.key, val)
		}
		*t.field(&w) = n
	}
	return w, nil
}

// writeScoreTemplate writes the commented serve.score block with every weight at its default.
func writeScoreTemplate(b *strings.Builder) {
	defaults := derive.DefaultWeights
	b.WriteString("\n#   # Priority score weights in points; 0 disables a term, negative values subtract\n")
	fmt.Fprintf(b, "#   %s:\n", scoreKey)
	for i, t := range scoreTerms {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(b, "#     # %s\n#     %s: %d\n", t.help, t.key, *t.field(&defaults))
	}
}
