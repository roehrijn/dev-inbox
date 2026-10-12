// Package derive computes inbox sections and domain events from PR snapshots. It is pure: no I/O.
package derive

import (
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
	last := pr.LastPushAt
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
	return slices.ContainsFunc(pr.Comments, func(c model.Comment) bool {
		return !isViewer(c.Author, viewer) && !isBot(c.Author) && c.CreatedAt.After(last)
	})
}

func isViewer(login string, viewer model.Viewer) bool {
	return login != "" && strings.EqualFold(login, viewer.Login)
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

// UrgencyKey is the PR-level annotation key whose presence marks a PR as urgent.
const UrgencyKey = "linear.urgency"

// Weights are the points each priority term adds to the score; see Score.
type Weights struct {
	Urgent          int
	PreferredTeam   int
	Greylist        int
	Direct          int
	ChecksRunning   int
	ChecksSuccess   int
	PerFailingCheck int
	MergeConflict   int
}

// DefaultWeights apply unless the config file overrides them.
var DefaultWeights = Weights{
	Urgent:          20,
	PreferredTeam:   30,
	Greylist:        -15,
	Direct:          10,
	ChecksRunning:   5,
	ChecksSuccess:   20,
	PerFailingCheck: -10,
	MergeConflict:   -40,
}

// Priority is the configured part of the score inputs.
type Priority struct {
	PreferredTeam    string   // "org/slug"
	PreferredMembers []string // logins of PreferredTeam's members
	// Greylist holds logins matched case-insensitively, with or without the "[bot]" suffix.
	Greylist []string
	Weights  Weights
}

// Inputs is everything besides the PR snapshot that classification and scoring depend on.
type Inputs struct {
	Viewer   model.Viewer
	Priority Priority
	// Annotations of the PR; only PR-level ones (CommentID empty) affect the score.
	Annotations []model.Annotation
}

// Score sums the priority terms for pr, weighted by in.Priority.Weights: Urgent if a PR-level
// UrgencyKey annotation exists, PreferredTeam if the author is a member of the preferred team,
// Greylist if the author is greylisted, Direct if review is requested from the viewer personally
// (cls.Direct), ChecksRunning if any check is still running, ChecksSuccess if the checks rollup is
// successful (none for a PR without checks), PerFailingCheck for each failing check, and
// MergeConflict if GitHub reports the PR as CONFLICTING (UNKNOWN and MERGEABLE add nothing).
func Score(pr model.PR, cls model.Classification, in Inputs) int {
	w := in.Priority.Weights
	var score int
	if slices.ContainsFunc(in.Annotations, func(a model.Annotation) bool {
		return a.CommentID == "" && a.Key == UrgencyKey
	}) {
		score += w.Urgent
	}
	if pr.Author != "" && slices.ContainsFunc(in.Priority.PreferredMembers, func(m string) bool {
		return strings.EqualFold(m, pr.Author)
	}) {
		score += w.PreferredTeam
	}
	if author := trimBot(pr.Author); author != "" && slices.ContainsFunc(in.Priority.Greylist, func(g string) bool {
		return trimBot(g) == author
	}) {
		score += w.Greylist
	}
	if cls.Direct {
		score += w.Direct
	}
	if pr.ChecksRunning > 0 {
		score += w.ChecksRunning
	}
	if pr.Checks == model.ChecksSuccess {
		score += w.ChecksSuccess
	}
	score += w.PerFailingCheck * pr.ChecksFailing
	if pr.Mergeable == "CONFLICTING" {
		score += w.MergeConflict
	}
	return score
}

// trimBot lowercases login and drops a trailing "[bot]".
func trimBot(login string) string {
	return strings.TrimSuffix(strings.ToLower(login), "[bot]")
}

// Evaluate classifies pr and sets its score.
func Evaluate(pr model.PR, in Inputs) model.Classification {
	cls := Classify(pr, in.Viewer)
	cls.Score = Score(pr, cls, in)
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
		if len(changes) > 0 || prChanged(*old, cur) {
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
