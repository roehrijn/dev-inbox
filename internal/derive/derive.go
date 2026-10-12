// Package derive computes inbox sections and domain events from PR snapshots. It is pure: no I/O.
package derive

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
)

// Classify assigns a PR to an inbox section for viewer. Closed and merged PRs get SectionNone.
func Classify(pr model.PR, viewer model.Viewer) model.Classification {
	if pr.State != model.PRStateOpen {
		return model.Classification{}
	}
	if !isViewer(pr.Author, viewer) {
		var requested, direct bool
		for _, req := range pr.ReviewRequests {
			rel, dir := viewerRequest(req, viewer)
			requested = requested || rel
			direct = direct || dir
		}
		if requested {
			return model.Classification{Section: model.SectionReviewRequested, Direct: direct}
		}
		return model.Classification{Section: model.SectionInvolved}
	}
	if pr.IsDraft {
		return model.Classification{Section: model.SectionDrafts}
	}
	if reasons := needsActionReasons(pr, viewer); len(reasons) > 0 {
		return model.Classification{Section: model.SectionNeedsAction, Reasons: reasons}
	}
	if readyToMerge(pr) {
		return model.Classification{Section: model.SectionReadyToMerge}
	}
	return model.Classification{Section: model.SectionWaitingForReview}
}

func needsActionReasons(pr model.PR, viewer model.Viewer) []string {
	var reasons []string
	if pr.Checks == model.ChecksFailure {
		reasons = append(reasons, "checks failing")
	}
	if pr.ReviewDecision == "CHANGES_REQUESTED" {
		reasons = append(reasons, "changes requested")
	}
	if pr.Mergeable == "CONFLICTING" {
		reasons = append(reasons, "merge conflict")
	}
	if hasNewComments(pr, viewer) {
		reasons = append(reasons, "new comments")
	}
	return reasons
}

func readyToMerge(pr model.PR) bool {
	if pr.InMergeQueue {
		return true
	}
	if pr.MergeStateStatus != "CLEAN" && pr.MergeStateStatus != "HAS_HOOKS" {
		return false
	}
	switch pr.ReviewDecision {
	case "APPROVED":
		return true
	case "":
		return len(pr.ReviewRequests) == 0 &&
			slices.ContainsFunc(pr.Reviews, func(r model.Review) bool { return r.State == "APPROVED" })
	}
	return false
}

// hasNewComments reports whether someone other than the viewer, and not a bot, commented after the
// viewer's last activity. A push counts as activity: it is the viewer's answer to earlier comments.
func hasNewComments(pr model.PR, viewer model.Viewer) bool {
	last := LastActivity(pr, viewer)
	if pr.LastPushAt.After(last) {
		last = pr.LastPushAt
	}
	return slices.ContainsFunc(pr.Comments, func(c model.Comment) bool {
		return !isViewer(c.Author, viewer) && !isBot(c.Author) && c.CreatedAt.After(last)
	})
}

func isViewer(login string, viewer model.Viewer) bool {
	return sameUser(login, viewer.Login)
}

// sameUser reports whether a and b are the same login, ignoring case; an empty login is nobody.
func sameUser(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}

func isBot(login string) bool {
	return strings.HasSuffix(strings.ToLower(login), "[bot]")
}

// viewerRequest reports whether req names the viewer or one of their teams, and whether it names
// the viewer personally.
func viewerRequest(req model.ReviewRequest, viewer model.Viewer) (relevant, direct bool) {
	direct = isViewer(req.Login, viewer)
	team := req.Team != "" && slices.ContainsFunc(viewer.Teams, func(t string) bool {
		return strings.EqualFold(t, req.Team)
	})
	return direct || team, direct
}

func viewerRequests(reqs []model.ReviewRequest, viewer model.Viewer) []model.ReviewRequest {
	var out []model.ReviewRequest
	for _, req := range reqs {
		if relevant, _ := viewerRequest(req, viewer); relevant {
			out = append(out, req)
		}
	}
	return out
}

type requestKey struct{ login, team string }

func keyOf(req model.ReviewRequest) requestKey {
	return requestKey{strings.ToLower(req.Login), strings.ToLower(req.Team)}
}

// except returns the requests in a that are not in b.
func except(a, b []model.ReviewRequest) []model.ReviewRequest {
	skip := make(map[requestKey]bool, len(b))
	for _, req := range b {
		skip[keyOf(req)] = true
	}
	var out []model.ReviewRequest
	for _, req := range a {
		if !skip[keyOf(req)] {
			out = append(out, req)
		}
	}
	return out
}

// change is an event type with its event-specific Change, before it is wrapped by model.NewEvent.
type change struct {
	typ  string
	data any
}

// Attention reasons of foreign PRs, see docs/proposals/attention.md.
const (
	ReasonReviewRequested     = "review requested"
	ReasonRereview            = "changes since your review"
	ReasonThreadReply         = "reply to your comment"
	ReasonMentioned           = "mentioned you"
	ReasonTeamReviewRequested = "team review requested"
)

// TeamReviewMaxAge is how long a PR may have been idle (see model.PRStatus.UpdatedAt) and still count
// as waiting for a review from one of the viewer's teams.
const TeamReviewMaxAge = 14 * 24 * time.Hour

// Annotation value fields with a meaning to derive. A PR-level annotation contributes AnnotationScore
// and AnnotationAttention only while it is newer than the viewer's last activity; a comment-level
// annotation with AnnotationAttention set to false excludes that comment from the thread-reply and
// mention signals.
const (
	AnnotationScore     = "score"
	AnnotationAttention = "attention"
)

// Limits on what a PR-level annotation contributes.
const (
	maxAnnotationScore   = 100
	maxAnnotationReasons = 5
	maxReasonLen         = 60 // characters
)

// LastActivity is the time of the viewer's latest submitted review, comment or inline comment on pr;
// zero if there is none.
func LastActivity(pr model.PR, viewer model.Viewer) time.Time {
	var last time.Time
	for _, r := range pr.Reviews {
		if isViewer(r.Author, viewer) && r.SubmittedAt.After(last) {
			last = r.SubmittedAt
		}
	}
	for _, c := range pr.Comments {
		if isViewer(c.Author, viewer) && c.CreatedAt.After(last) {
			last = c.CreatedAt
		}
	}
	return last
}

// AcknowledgingReactions are the reactions by which the viewer marks a comment as handled.
var AcknowledgingReactions = []string{"THUMBS_UP", "HEART", "HOORAY", "ROCKET"}

// Handled reports whether the viewer has dealt with comment c: an acknowledging reaction on it, a
// later reply of the viewer in the same review thread or, for top-level comments, a later top-level
// comment of the viewer; in both cases also a later formal review of the viewer (approval, change
// request, dismissal, or a review with a summary body). The viewer's own comments are handled. See
// docs/proposals/attention.md.
func Handled(pr model.PR, viewer model.Viewer, c model.Comment) bool {
	if isViewer(c.Author, viewer) || slices.ContainsFunc(c.ViewerReactions, func(r string) bool {
		return slices.Contains(AcknowledgingReactions, r)
	}) {
		return true
	}
	if slices.ContainsFunc(pr.Comments, func(o model.Comment) bool {
		if !isViewer(o.Author, viewer) || !o.CreatedAt.After(c.CreatedAt) {
			return false
		}
		if c.Kind == model.CommentInline {
			return o.Kind == model.CommentInline && c.ThreadID != "" && o.ThreadID == c.ThreadID
		}
		return o.Kind == model.CommentIssue
	}) {
		return true
	}
	return slices.ContainsFunc(pr.Reviews, func(r model.Review) bool {
		return isViewer(r.Author, viewer) && r.SubmittedAt.After(c.CreatedAt) && formalReview(pr, r)
	})
}

// formalReview reports whether r is more than the bodiless COMMENTED review that GitHub records for
// each bare thread reply. A review body is in pr.Comments as a CommentReview with the review's ID.
func formalReview(pr model.PR, r model.Review) bool {
	switch r.State {
	case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		return true
	case "COMMENTED":
		return slices.ContainsFunc(pr.Comments, func(c model.Comment) bool {
			return c.Kind == model.CommentReview && c.ID == r.ID
		})
	}
	return false
}

// MyThreads returns the IDs of the review threads the viewer started or replied in.
func MyThreads(pr model.PR, viewer model.Viewer) map[string]bool {
	threads := map[string]bool{}
	for _, c := range pr.Comments {
		if c.ThreadID != "" && isViewer(c.Author, viewer) {
			threads[c.ThreadID] = true
		}
	}
	return threads
}

// attention returns why the foreign open PR pr needs the viewer, core signals first, and the terms
// this adds to Score: the weight of each core signal (a direct review request is already weighted by
// Direct), the score of each fresh PR-level annotation, and NeedsMe if there is any reason.
func attention(pr model.PR, direct bool, in Inputs) ([]string, []model.ScoreTerm) {
	w, viewer := in.Priority.Weights, in.Viewer
	last := LastActivity(pr, viewer)
	mine := MyThreads(pr, viewer)
	quiet := quietComments(in.Annotations)
	reply := func(c model.Comment) bool { return mine[c.ThreadID] && !isViewer(c.Author, viewer) && !isBot(c.Author) }
	open := func(c model.Comment) bool { return !quiet[c.ID] && !Handled(pr, viewer, c) }

	var reasons []string
	var terms []model.ScoreTerm
	switch {
	case direct:
		reasons = append(reasons, ReasonReviewRequested)
	case teamReview(pr, in):
		reasons = append(reasons, ReasonTeamReviewRequested)
		terms = addTerm(terms, ReasonTeamReviewRequested, w.TeamReview)
	}
	switch {
	case rereview(pr, viewer, mine, slices.ContainsFunc(pr.Comments, func(c model.Comment) bool {
		return reply(c) && c.CreatedAt.After(last)
	})):
		reasons = append(reasons, ReasonRereview)
		terms = addTerm(terms, ReasonRereview, w.Rereview)
	case slices.ContainsFunc(pr.Comments, func(c model.Comment) bool { return reply(c) && open(c) }):
		reasons = append(reasons, ReasonThreadReply)
		terms = addTerm(terms, ReasonThreadReply, w.ThreadReply)
	}
	if slices.ContainsFunc(pr.Comments, func(c model.Comment) bool {
		return Mentions(c.Body, viewer.Login) && open(c)
	}) {
		reasons = append(reasons, ReasonMentioned)
		terms = addTerm(terms, ReasonMentioned, w.Mentioned)
	}

	for _, a := range in.Annotations {
		if a.CommentID != "" || a.Key == UrgencyKey || !a.UpdatedAt.After(last) {
			continue
		}
		fields := annotationFields(a)
		var n int
		if json.Unmarshal(fields[AnnotationScore], &n) == nil {
			terms = addTerm(terms, a.Key, max(-maxAnnotationScore, min(n, maxAnnotationScore)))
		}
		var list []string
		if json.Unmarshal(fields[AnnotationAttention], &list) == nil {
			for _, r := range list[:min(len(list), maxAnnotationReasons)] {
				if r = truncate(strings.TrimSpace(r), maxReasonLen); r != "" && !slices.Contains(reasons, r) {
					reasons = append(reasons, r)
				}
			}
		}
	}

	if len(reasons) > 0 {
		terms = addTerm(terms, "needs you", w.NeedsMe)
	}
	return reasons, terms
}

// addTerm appends a term of points to terms unless points is 0.
func addTerm(terms []model.ScoreTerm, term string, points int) []model.ScoreTerm {
	if points == 0 {
		return terms
	}
	return append(terms, model.ScoreTerm{Term: term, Points: points})
}

func sumTerms(terms []model.ScoreTerm) int {
	var n int
	for _, t := range terms {
		n += t.Points
	}
	return n
}

// teamReview reports whether pr waits for a first human review that one of the viewer's teams was asked
// for, and is ready for it: not a draft, updated within TeamReviewMaxAge, checks not failing, no merge
// conflict, and not authored by a bot or a greylisted login. Bots, greylisted logins and the author
// (GitHub records the author's thread replies as COMMENTED reviews) are not reviewers. The caller has
// ruled out a direct request, so a request that names the viewer names a team. A zero in.Now disables
// the signal.
func teamReview(pr model.PR, in Inputs) bool {
	greylist := in.Priority.Greylist
	if in.Now.IsZero() || pr.IsDraft || in.Now.Sub(pr.UpdatedAt) > TeamReviewMaxAge ||
		pr.Checks == model.ChecksFailure || pr.Mergeable == "CONFLICTING" ||
		isBot(pr.Author) || greylisted(pr.Author, greylist) {
		return false
	}
	if len(viewerRequests(pr.ReviewRequests, in.Viewer)) == 0 {
		return false
	}
	return !slices.ContainsFunc(pr.Reviews, func(r model.Review) bool {
		return !isBot(r.Author) && !greylisted(r.Author, greylist) && !sameUser(r.Author, pr.Author)
	})
}

// rereview reports whether the head differs from the commit of the viewer's latest review or inline
// comment, whichever is later, and that review requested changes or was dismissed, someone else
// replied in one of the viewer's threads since the viewer's last activity (replied), or someone else
// resolved one of them. An unknown commit never differs. Resolutions carry no time, so a resolved
// thread counts for as long as the head differs.
func rereview(pr model.PR, viewer model.Viewer, mine map[string]bool, replied bool) bool {
	var head, state string
	var headAt time.Time
	for _, r := range pr.Reviews {
		if isViewer(r.Author, viewer) && !r.SubmittedAt.Before(headAt) {
			head, headAt, state = r.CommitSHA, r.SubmittedAt, r.State
		}
	}
	for _, c := range pr.Comments {
		if c.Kind == model.CommentInline && isViewer(c.Author, viewer) && !c.CreatedAt.Before(headAt) {
			head, headAt = c.CommitSHA, c.CreatedAt
		}
	}
	if head == "" || head == pr.HeadSHA {
		return false
	}
	return state == "CHANGES_REQUESTED" || state == "DISMISSED" || replied ||
		slices.ContainsFunc(pr.ReviewThreads, func(t model.ReviewThread) bool {
			return mine[t.ID] && t.IsResolved && !isViewer(t.ResolvedBy, viewer)
		})
}

// Mentions reports whether body mentions login as "@login", case-insensitively, but not inside an
// e-mail address or as the prefix of a longer login.
func Mentions(body, login string) bool {
	if login == "" {
		return false
	}
	body, mention := strings.ToLower(body), "@"+strings.ToLower(login)
	for i := 0; ; {
		j := strings.Index(body[i:], mention)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(mention)
		if (start == 0 || !isWordByte(body[start-1])) &&
			(end == len(body) || (!isWordByte(body[end]) && body[end] != '-')) {
			return true
		}
		i = start + 1
	}
}

func isWordByte(b byte) bool {
	return b == '_' || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// quietComments returns the IDs of the comments that a comment-level annotation excludes from the
// thread-reply and mention signals by setting AnnotationAttention to false.
func quietComments(annotations []model.Annotation) map[string]bool {
	quiet := map[string]bool{}
	for _, a := range annotations {
		var on *bool
		if a.CommentID != "" && json.Unmarshal(annotationFields(a)[AnnotationAttention], &on) == nil &&
			on != nil && !*on {
			quiet[a.CommentID] = true
		}
	}
	return quiet
}

// annotationFields decodes the value of a, which has fields only if it is a JSON object.
func annotationFields(a model.Annotation) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(a.Value, &fields) != nil {
		return nil
	}
	return fields
}

// truncate shortens s to at most n characters.
func truncate(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// UrgencyKey is the PR-level annotation key whose presence marks a PR as urgent.
const UrgencyKey = "linear.urgency"

// HiddenKey is the PR-level annotation by which the viewer hides a PR until something new happens in
// it. Its value is the HiddenMark of the snapshot the viewer saw when hiding it; see hidden.
const HiddenKey = "devinbox.hidden"

// HiddenMark records what the viewer had seen of a PR when hiding it.
type HiddenMark struct {
	Seen  time.Time `json:"seen"` // FetchedAt of the snapshot
	Head  string    `json:"head"`
	Draft bool      `json:"draft"`
	// Requested is true when review was requested from the viewer personally.
	Requested bool `json:"requested"`
}

// HiddenAnnotation returns the annotation that hides pr, classified as cls, until something new
// happens in it.
func HiddenAnnotation(pr model.PR, cls model.Classification) model.Annotation {
	value, _ := json.Marshal(HiddenMark{Seen: pr.FetchedAt, Head: pr.HeadSHA, Draft: pr.IsDraft, Requested: cls.Direct})
	return model.Annotation{PR: pr.Key, Key: HiddenKey, Value: value}
}

// hidden reports whether the viewer hid pr and nothing new happened since: the head is unchanged (no
// commits, rebase or force-push), it was not marked ready for review, review was not newly requested
// from the viewer personally (direct), and nobody else commented or reviewed after the snapshot the
// viewer saw. Comments and reviews of bots and greylisted logins are not new.
func hidden(pr model.PR, direct bool, in Inputs) bool {
	i := slices.IndexFunc(in.Annotations, func(a model.Annotation) bool {
		return a.CommentID == "" && a.Key == HiddenKey
	})
	if i < 0 {
		return false
	}
	var m HiddenMark
	if json.Unmarshal(in.Annotations[i].Value, &m) != nil || m.Head != pr.HeadSHA ||
		m.Draft && !pr.IsDraft || direct && !m.Requested {
		return false
	}
	news := func(login string, at time.Time) bool {
		return at.After(m.Seen) && !isViewer(login, in.Viewer) && !isBot(login) &&
			!greylisted(login, in.Priority.Greylist)
	}
	return !slices.ContainsFunc(pr.Comments, func(c model.Comment) bool { return news(c.Author, c.CreatedAt) }) &&
		!slices.ContainsFunc(pr.Reviews, func(r model.Review) bool { return news(r.Author, r.SubmittedAt) })
}

// Weights are the points each priority term adds to the score; see Score.
type Weights struct {
	Urgent          int
	Greylist        int
	Direct          int
	ChecksRunning   int
	ChecksSuccess   int
	PerFailingCheck int
	MergeConflict   int
	Draft           int
	NeedsMe         int
	Rereview        int
	ThreadReply     int
	Mentioned       int
	TeamReview      int
}

// DefaultWeights apply unless the config file overrides them.
var DefaultWeights = Weights{
	Urgent:          20,
	Greylist:        -15,
	Direct:          10,
	ChecksRunning:   5,
	ChecksSuccess:   20,
	PerFailingCheck: -10,
	MergeConflict:   -40,
	Draft:           -5,
	NeedsMe:         20,
	Rereview:        30,
	ThreadReply:     15,
	Mentioned:       20,
	TeamReview:      5,
}

// TeamBoost adds Points to the score of PRs authored by a member of Team.
type TeamBoost struct {
	Team    string   // "org/slug"
	Points  int      // may be negative
	Members []string // logins, including the members of child teams
}

// Priority is the configured part of the score inputs.
type Priority struct {
	// Teams boost PRs of their members; an author in several of them gets the highest Points.
	Teams []TeamBoost
	// Greylist holds logins matched case-insensitively, with or without the "[bot]" suffix.
	Greylist []string
	Weights  Weights
}

// Inputs is everything besides the PR snapshot that classification and scoring depend on.
type Inputs struct {
	Viewer   model.Viewer
	Priority Priority
	// Annotations of the PR and of its comments. PR-level ones (CommentID empty) affect the score and
	// add attention reasons; comment-level ones can only exclude their comment from attention signals.
	Annotations []model.Annotation
	// Now is the time that age-based signals measure against. Derive never reads the clock; a zero Now
	// disables those signals.
	Now time.Time
}

// Score sums the priority terms for pr; see ScoreTerms.
func Score(pr model.PR, cls model.Classification, in Inputs) int {
	return sumTerms(ScoreTerms(pr, cls, in))
}

// ScoreTerms returns the non-zero priority terms for pr, weighted by in.Priority.Weights: Urgent if a
// PR-level UrgencyKey annotation exists, the highest TeamBoost.Points of the teams the author is a
// member of, Greylist if the author is greylisted, Direct if review is requested from the viewer
// personally (cls.Direct), ChecksRunning if any check is still running, ChecksSuccess if the checks
// rollup is successful (none for a PR without checks), PerFailingCheck for each failing check,
// MergeConflict if GitHub reports the PR as CONFLICTING (UNKNOWN and MERGEABLE add nothing), and Draft
// if the PR is a draft. Evaluate adds the attention terms of foreign PRs.
func ScoreTerms(pr model.PR, cls model.Classification, in Inputs) []model.ScoreTerm {
	w := in.Priority.Weights
	var terms []model.ScoreTerm
	if slices.ContainsFunc(in.Annotations, func(a model.Annotation) bool {
		return a.CommentID == "" && a.Key == UrgencyKey
	}) {
		terms = addTerm(terms, "urgent", w.Urgent)
	}
	if t, ok := teamBoost(pr.Author, in.Priority.Teams); ok {
		terms = addTerm(terms, "team "+t.Team, t.Points)
	}
	if greylisted(pr.Author, in.Priority.Greylist) {
		terms = addTerm(terms, "greylisted author", w.Greylist)
	}
	if cls.Direct {
		terms = addTerm(terms, "review requested from you", w.Direct)
	}
	if pr.ChecksRunning > 0 {
		terms = addTerm(terms, "checks running", w.ChecksRunning)
	}
	if pr.Checks == model.ChecksSuccess {
		terms = addTerm(terms, "checks passed", w.ChecksSuccess)
	}
	switch pr.ChecksFailing {
	case 0:
	case 1:
		terms = addTerm(terms, "1 failing check", w.PerFailingCheck)
	default:
		terms = addTerm(terms, fmt.Sprintf("%d failing checks", pr.ChecksFailing), w.PerFailingCheck*pr.ChecksFailing)
	}
	if pr.Mergeable == "CONFLICTING" {
		terms = addTerm(terms, "merge conflict", w.MergeConflict)
	}
	if pr.IsDraft {
		terms = addTerm(terms, "draft", w.Draft)
	}
	return terms
}

// teamBoost returns the team with the highest Points that login is a member of.
func teamBoost(login string, teams []TeamBoost) (TeamBoost, bool) {
	var best TeamBoost
	found := false
	for _, t := range teams {
		if (!found || t.Points > best.Points) && slices.ContainsFunc(t.Members, func(m string) bool {
			return login != "" && strings.EqualFold(m, login)
		}) {
			best, found = t, true
		}
	}
	return best, found
}

// greylisted reports whether login is on list, which matches case-insensitively and with or without
// the "[bot]" suffix.
func greylisted(login string, list []string) bool {
	login = trimBot(login)
	return login != "" && slices.ContainsFunc(list, func(g string) bool { return trimBot(g) == login })
}

// trimBot lowercases login and drops a trailing "[bot]".
func trimBot(login string) string {
	return strings.TrimSuffix(strings.ToLower(login), "[bot]")
}

// Evaluate classifies pr, sets its score with its terms and whether it is hidden. A foreign open PR also gets its
// attention reasons, and NeedsMe if it has any; see attention.
func Evaluate(pr model.PR, in Inputs) model.Classification {
	cls := Classify(pr, in.Viewer)
	var extra []model.ScoreTerm
	if cls.Section == model.SectionReviewRequested || cls.Section == model.SectionInvolved {
		cls.Reasons, extra = attention(pr, cls.Direct, in)
		cls.NeedsMe = len(cls.Reasons) > 0
	}
	cls.ScoreTerms = append(ScoreTerms(pr, cls, in), extra...)
	cls.Score = sumTerms(cls.ScoreTerms)
	cls.Hidden = hidden(pr, cls.Direct, in)
	return cls
}

// Diff derives domain events from the stored snapshot old (nil for a newly discovered PR) and the
// fresh snapshot cur, in the order consumers should see them.
func Diff(old *model.PR, cur model.PR, in Inputs) ([]model.Event, error) {
	viewer := in.Viewer
	cls := Evaluate(cur, in)

	var changes []change
	if old == nil {
		changes = append(changes, change{typ: model.EventDiscovered})
		changes = appendRequests(changes, model.EventReviewRequested, viewerRequests(cur.ReviewRequests, viewer), viewer)
	} else {
		changes = diffChanges(*old, cur, viewer, cls.Section)
		if len(changes) > 0 || prChanged(*old, cur) || reactionsChanged(*old, cur) {
			changes = append(changes, change{typ: model.EventUpdated})
		}
	}

	var events []model.Event
	for _, c := range changes {
		ev, err := model.NewEvent(c.typ, cur.Key, model.EventData{PR: cur, Classification: cls, Change: c.data})
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// diffChanges lists every change except pr.updated and discovery, in event order. section is the
// classification of cur.
func diffChanges(old, cur model.PR, viewer model.Viewer, section model.Section) []change {
	var cs []change
	if typ := stateEvent(old.State, cur.State); typ != "" {
		cs = append(cs, change{typ: typ})
	}

	oldReqs, curReqs := viewerRequests(old.ReviewRequests, viewer), viewerRequests(cur.ReviewRequests, viewer)
	cs = appendRequests(cs, model.EventReviewRequestRemoved, except(oldReqs, curReqs), viewer)
	cs = appendRequests(cs, model.EventReviewRequested, except(curReqs, oldReqs), viewer)

	if old.HeadSHA != cur.HeadSHA {
		cs = append(cs, change{model.EventCommitsPushed, model.ChangeCommits{Before: old.HeadSHA, After: cur.HeadSHA}})
	}

	for _, r := range unseen(old.Reviews, cur.Reviews,
		func(r model.Review) string { return r.ID },
		func(r model.Review) time.Time { return r.SubmittedAt }) {
		cs = append(cs, change{model.EventReviewSubmitted, r})
	}
	for _, c := range unseen(old.Comments, cur.Comments,
		func(c model.Comment) string { return c.ID },
		func(c model.Comment) time.Time { return c.CreatedAt }) {
		cs = append(cs, change{model.EventCommentAdded, c})
	}

	if old.Checks != cur.Checks {
		chg := model.ChangeChecks{Before: old.Checks, After: cur.Checks}
		switch cur.Checks {
		case model.ChecksFailure:
			cs = append(cs, change{model.EventChecksFailed, chg})
		case model.ChecksSuccess:
			cs = append(cs, change{model.EventChecksPassed, chg})
		}
	}

	if old.IsDraft != cur.IsDraft {
		cs = append(cs, change{model.EventDraftChanged, model.ChangeDraft{IsDraft: cur.IsDraft}})
	}

	if from := Classify(old, viewer).Section; from != section {
		cs = append(cs, change{model.EventSectionChanged, model.ChangeSection{From: from, To: section}})
	}
	return cs
}

func stateEvent(old, cur model.PRState) string {
	if old == cur {
		return ""
	}
	switch cur {
	case model.PRStateMerged:
		return model.EventMerged
	case model.PRStateClosed:
		return model.EventClosed
	case model.PRStateOpen:
		return model.EventReopened
	}
	return ""
}

func appendRequests(cs []change, typ string, reqs []model.ReviewRequest, viewer model.Viewer) []change {
	for _, req := range reqs {
		_, direct := viewerRequest(req, viewer)
		cs = append(cs, change{typ, model.ChangeReviewRequest{ReviewRequest: req, Direct: direct}})
	}
	return cs
}

// unseen returns the items of cur whose ID is not in old, oldest first.
func unseen[T any](old, cur []T, id func(T) string, at func(T) time.Time) []T {
	seen := make(map[string]bool, len(old))
	for _, o := range old {
		seen[id(o)] = true
	}
	var out []T
	for _, c := range cur {
		if !seen[id(c)] {
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b T) int { return at(a).Compare(at(b)) })
	return out
}

// prChanged reports whether two snapshots differ in anything but Comments and FetchedAt.
func prChanged(old, cur model.PR) bool {
	a, b := reflect.ValueOf(old), reflect.ValueOf(cur)
	for i := range a.NumField() {
		switch a.Type().Field(i).Name {
		case "Comments", "FetchedAt":
			continue
		}
		if !equal(a.Field(i), b.Field(i)) {
			return true
		}
	}
	return false
}

// reactionsChanged reports whether the viewer's reactions differ on a comment that both snapshots
// contain; a reaction can make the comment Handled.
func reactionsChanged(old, cur model.PR) bool {
	before := make(map[string][]string, len(old.Comments))
	for _, c := range old.Comments {
		before[c.ID] = c.ViewerReactions
	}
	return slices.ContainsFunc(cur.Comments, func(c model.Comment) bool {
		r, ok := before[c.ID]
		return ok && !slices.Equal(r, c.ViewerReactions)
	})
}

var timeType = reflect.TypeFor[time.Time]()

// equal is a deep comparison that treats nil and empty slices as equal and times by instant, so
// that a snapshot read back from the store matches a freshly fetched one.
func equal(a, b reflect.Value) bool {
	switch {
	case a.Type() == timeType:
		return a.Interface().(time.Time).Equal(b.Interface().(time.Time))
	case a.Kind() == reflect.Struct:
		for i := range a.NumField() {
			if !equal(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	case a.Kind() == reflect.Slice:
		if a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !equal(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}
