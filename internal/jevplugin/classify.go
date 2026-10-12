package jevplugin

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/jev"
	"github.com/roehrijn/dev-inbox/internal/model"
)

// Intents of a comment towards the viewer, the options of the classification question.
const (
	IntentQuestionForMe   = "question_for_me"
	IntentPushback        = "pushback"
	IntentRereviewRequest = "rereview_request"
	IntentAddressed       = "addressed"
	IntentAck             = "ack"
	IntentNotForMe        = "not_for_me"
	// IntentUnclear is recorded when the most probable intent stays below the threshold.
	IntentUnclear = "unclear"
)

// Annotation keys written by the plugin.
const (
	KeyIntent    = "jev.intent"    // comment-level, see intentValue
	KeyAttention = "jev.attention" // PR-level, see attentionValue
)

type intent struct {
	name        string
	description string
	// reason is added to the PR's attention reasons; badge labels the intent in the TUI badge.
	reason string
	badge  string
	// suppress marks the comment with "attention": false, so the core's thread-reply and mention
	// signals ignore it.
	suppress bool
}

// intents are the options in the order Jev sees them. Descriptions refer to fields of the state and
// the instructions by name in backticks, as https://docs.typesafe.ai/primitives recommends.
var intents = []intent{
	{name: IntentQuestionForMe, description: "Asks `viewer` a question or wants their input or decision.",
		reason: "question for you", badge: "question"},
	{name: IntentPushback, description: "Disagrees with or questions a finding or suggestion that `viewer` made.",
		reason: "pushback on your comment", badge: "pushback"},
	{name: IntentRereviewRequest, description: "Asks `viewer` to look at the pull request again.",
		reason: "asks for re-review", badge: "re-review"},
	{name: IntentAddressed, description: "Reports that a finding of `viewer` was fixed or addressed, without asking anything.",
		badge: "addressed"},
	{name: IntentAck, description: "Only thanks or acknowledges, without asking anything.",
		badge: "ack", suppress: true},
	{name: IntentNotForMe, description: "Informational, or addressed to someone other than `viewer`.",
		badge: "not for me", suppress: true},
}

// Intents lists the classification options in order.
var Intents = func() []string {
	out := make([]string, len(intents))
	for i, in := range intents {
		out[i] = in.name
	}
	return out
}()

func lookupIntent(name string) (intent, bool) {
	i := slices.IndexFunc(intents, func(in intent) bool { return in.name == name })
	if i < 0 {
		return intent{}, false
	}
	return intents[i], true
}

var criteria = func() jev.Criteria {
	c := make(jev.Criteria, len(intents))
	for i, in := range intents {
		c[i] = jev.Option{Label: in.name, Description: in.description}
	}
	return c
}()

// intentValue is the value of a comment-level KeyIntent annotation.
type intentValue struct {
	Intent        string             `json:"intent"`
	Probability   float64            `json:"probability"`
	Probabilities map[string]float64 `json:"probabilities"`
	// Attention is false for intents that need nothing from the viewer; see derive.AnnotationAttention.
	Attention *bool `json:"attention,omitempty"`
}

// attentionValue is the value of the PR-level KeyAttention annotation; see derive.AnnotationScore and
// derive.AnnotationAttention.
type attentionValue struct {
	Score     int      `json:"score"`
	Attention []string `json:"attention,omitempty"`
	Badge     string   `json:"badge"`
	Level     string   `json:"level"`
}

// candidates returns the comments of pr worth classifying for viewer, oldest first: by someone else,
// not a bot, not yet handled by the viewer (derive.Handled), and in a thread the viewer took part in,
// mentioning the viewer, or top-level on a PR the viewer is directly requested to review or has
// reviewed.
func candidates(pr model.PR, viewer model.Viewer) []model.Comment {
	mine := derive.MyThreads(pr, viewer)
	reviewer := slices.ContainsFunc(pr.ReviewRequests, func(r model.ReviewRequest) bool { return isViewer(r.Login, viewer) }) ||
		slices.ContainsFunc(pr.Reviews, func(r model.Review) bool { return isViewer(r.Author, viewer) })
	var out []model.Comment
	for _, c := range pr.Comments {
		if isViewer(c.Author, viewer) || isBot(c.Author) || derive.Handled(pr, viewer, c) {
			continue
		}
		if c.ThreadID != "" && mine[c.ThreadID] || derive.Mentions(c.Body, viewer.Login) ||
			c.Kind != model.CommentInline && reviewer {
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b model.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

func isViewer(login string, viewer model.Viewer) bool {
	return login != "" && strings.EqualFold(login, viewer.Login)
}

func isBot(login string) bool {
	return strings.HasSuffix(strings.ToLower(login), "[bot]")
}

// maxBodyBytes caps each comment body sent to Jev; a pasted log says nothing more about intent.
const maxBodyBytes = 4000

type prState struct {
	PullRequest struct {
		Title  string `json:"title"`
		Author string `json:"author"`
	} `json:"pull_request"`
	Viewer string `json:"viewer"`
}

type commentInstructions struct {
	Comment  commentView   `json:"comment"`
	Thread   []threadEntry `json:"thread,omitempty"`
	Question string        `json:"question"`
}

type commentView struct {
	Author         string `json:"author"`
	Kind           string `json:"kind"`
	Path           string `json:"path,omitempty"`
	Body           string `json:"body"`
	MentionsViewer bool   `json:"mentions_viewer"`
}

type threadEntry struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

var kindNames = map[model.CommentKind]string{
	model.CommentIssue:  "conversation comment",
	model.CommentReview: "review summary",
	model.CommentInline: "inline review comment",
}

// request builds one Choice question per comment in todo, keyed by comment ID. The state holds what
// all questions share; each question carries its comment and that comment's context, so Jev judges
// one comment per question instead of picking it out of a crowded state.
func request(pr model.PR, viewer model.Viewer, todo []model.Comment) jev.Request {
	var st prState
	st.PullRequest.Title = pr.Title
	st.PullRequest.Author = pr.Author
	st.Viewer = viewer.Login
	qs := make(map[string]jev.Question, len(todo))
	for _, c := range todo {
		in := commentInstructions{
			Comment: commentView{
				Author:         c.Author,
				Kind:           kindNames[c.Kind],
				Path:           c.Path,
				Body:           truncate(c.Body),
				MentionsViewer: derive.Mentions(c.Body, viewer.Login),
			},
			Thread: thread(pr, viewer, c),
		}
		in.Question = "What does `comment` want from `viewer` on `pull_request`?"
		if len(in.Thread) > 0 {
			in.Question += " `thread` holds the earlier comments it follows, oldest first."
		}
		qs[c.ID] = jev.Choice(in, criteria)
	}
	return jev.Request{State: st, Questions: qs}
}

// thread returns the context c replies to: for an inline comment the earlier comments of its review
// thread, for a top-level comment the viewer's latest earlier top-level comment or review summary.
func thread(pr model.PR, viewer model.Viewer, c model.Comment) []threadEntry {
	var out []threadEntry
	if c.ThreadID != "" {
		for _, o := range pr.Comments {
			if o.ThreadID == c.ThreadID && o.ID != c.ID && o.CreatedAt.Before(c.CreatedAt) {
				out = append(out, threadEntry{Author: o.Author, Body: truncate(o.Body)})
			}
		}
		return out
	}
	var latest *model.Comment
	var at time.Time
	for i, o := range pr.Comments {
		if o.Kind != model.CommentInline && isViewer(o.Author, viewer) && o.CreatedAt.Before(c.CreatedAt) &&
			o.CreatedAt.After(at) {
			latest, at = &pr.Comments[i], o.CreatedAt
		}
	}
	if latest != nil {
		out = append(out, threadEntry{Author: latest.Author, Body: truncate(latest.Body)})
	}
	return out
}

func truncate(s string) string {
	if len(s) <= maxBodyBytes {
		return s
	}
	cut := maxBodyBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// classification turns Jev's answer into an annotation value: below threshold the intent is unclear.
func classification(a jev.Answer, threshold float64) (intentValue, error) {
	if a.Type != "choice" {
		return intentValue{}, fmt.Errorf("answer of type %q, want choice", a.Type)
	}
	in, ok := lookupIntent(a.Choice)
	if !ok {
		return intentValue{}, fmt.Errorf("answer %q is no option", a.Choice)
	}
	v := intentValue{Intent: in.name, Probability: a.Probabilities[a.Choice], Probabilities: a.Probabilities}
	switch {
	case v.Probability < threshold:
		v.Intent = IntentUnclear
	case in.suppress:
		v.Attention = new(bool)
	}
	return v, nil
}

// result computes the PR-level annotation from the intents of the current candidates, or nil when
// nothing is needed; a comment the viewer has handled is no candidate, so its intent adds nothing even
// though its annotation stays. Each intent counts once. An intent with weight 0 contributes nothing; otherwise
// it adds its weight and, if it has one, its reason. A re-review request adds nothing when the core
// already reports changes since the viewer's review (coreReasons), which owns that signal.
func result(cands []model.Comment, values map[string]intentValue, coreReasons []string,
	weights map[string]int) *attentionValue {
	present := make(map[string]bool)
	for _, c := range cands {
		if v, ok := values[c.ID]; ok {
			present[v.Intent] = true
		}
	}
	var res attentionValue
	var badges []string
	level := "info"
	for _, in := range intents {
		w := weights[in.name]
		if !present[in.name] || w == 0 {
			continue
		}
		if in.name == IntentRereviewRequest && slices.Contains(coreReasons, derive.ReasonRereview) {
			continue
		}
		res.Score += w
		if in.reason != "" {
			res.Attention = append(res.Attention, in.reason)
		}
		badges = append(badges, in.badge)
		if in.name == IntentQuestionForMe || in.name == IntentPushback {
			level = "warn"
		}
	}
	if len(badges) == 0 {
		return nil
	}
	res.Badge = strings.Join(badges, ", ")
	res.Level = level
	return &res
}
