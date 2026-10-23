package derive

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
)

var (
	t0     = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	viewer = model.Viewer{Login: "viewer", Teams: []string{"acme-co/core"}}
	inputs = Inputs{Viewer: viewer, Priority: Priority{Weights: DefaultWeights}}
)

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

// basePR is an open PR by someone else without requests, reviews or comments.
func basePR(mut ...func(*model.PR)) model.PR {
	pr := model.PR{
		Key:        model.PRKey{Owner: "acme-co", Repo: "repo", Number: 7},
		PRStatus:   model.PRStatus{State: model.PRStateOpen, HeadSHA: "aaa", UpdatedAt: at(0)},
		Author:     "someone",
		LastPushAt: at(0),
		FetchedAt:  at(1),
	}
	for _, m := range mut {
		m(&pr)
	}
	return pr
}

// minePR is basePR authored by the viewer.
func minePR(mut ...func(*model.PR)) model.PR {
	return basePR(append([]func(*model.PR){func(pr *model.PR) { pr.Author = "viewer" }}, mut...)...)
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		pr   model.PR
		want model.Classification
	}{
		{
			name: "merged is none",
			pr:   minePR(func(pr *model.PR) { pr.State = model.PRStateMerged }),
			want: model.Classification{},
		},
		{
			name: "closed is none even with a request for the viewer",
			pr: basePR(func(pr *model.PR) {
				pr.State = model.PRStateClosed
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
			}),
			want: model.Classification{},
		},
		{
			name: "direct request",
			pr:   basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}} }),
			want: model.Classification{Section: model.SectionReviewRequested, Direct: true},
		},
		{
			name: "direct request compares logins case-insensitively",
			pr:   basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "VieWer"}} }),
			want: model.Classification{Section: model.SectionReviewRequested, Direct: true},
		},
		{
			name: "team request is not direct and compares case-insensitively",
			pr:   basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Team: "Acme-CO/Core"}} }),
			want: model.Classification{Section: model.SectionReviewRequested},
		},
		{
			name: "direct wins over a team request for the same PR",
			pr: basePR(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/core"}, {Login: "viewer"}}
			}),
			want: model.Classification{Section: model.SectionReviewRequested, Direct: true},
		},
		{
			name: "request for another user and another team is involved",
			pr: basePR(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Login: "other"}, {Team: "acme-co/other"}}
			}),
			want: model.Classification{Section: model.SectionInvolved},
		},
		{
			name: "no requests is involved",
			pr:   basePR(),
			want: model.Classification{Section: model.SectionInvolved},
		},
		{
			name: "someone else's draft with a request is still review_requested",
			pr: basePR(func(pr *model.PR) {
				pr.IsDraft = true
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
			}),
			want: model.Classification{Section: model.SectionReviewRequested, Direct: true},
		},
		{
			name: "someone else's PR with failing checks stays involved",
			pr:   basePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure }),
			want: model.Classification{Section: model.SectionInvolved},
		},
		{
			name: "author compares case-insensitively",
			pr:   basePR(func(pr *model.PR) { pr.Author = "VIEWER" }),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "own PR ignores a request naming the viewer",
			pr:   minePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}} }),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "draft",
			pr:   minePR(func(pr *model.PR) { pr.IsDraft = true }),
			want: model.Classification{Section: model.SectionDrafts},
		},
		{
			name: "draft with failing checks is drafts",
			pr: minePR(func(pr *model.PR) {
				pr.IsDraft = true
				pr.Checks = model.ChecksFailure
				pr.Mergeable = "CONFLICTING"
			}),
			want: model.Classification{Section: model.SectionDrafts},
		},
		{
			name: "needs action: checks failing",
			pr:   minePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure }),
			want: model.Classification{Section: model.SectionNeedsAction, Reasons: []string{"checks failing"}},
		},
		{
			name: "needs action: changes requested",
			pr:   minePR(func(pr *model.PR) { pr.ReviewDecision = "CHANGES_REQUESTED" }),
			want: model.Classification{Section: model.SectionNeedsAction, Reasons: []string{"changes requested"}},
		},
		{
			name: "needs action: merge conflict",
			pr:   minePR(func(pr *model.PR) { pr.Mergeable = "CONFLICTING" }),
			want: model.Classification{Section: model.SectionNeedsAction, Reasons: []string{"merge conflict"}},
		},
		{
			name: "needs action: new comments",
			pr: minePR(func(pr *model.PR) {
				pr.Comments = []model.Comment{{ID: "c1", Author: "other", CreatedAt: at(5)}}
			}),
			want: model.Classification{Section: model.SectionNeedsAction, Reasons: []string{"new comments"}},
		},
		{
			name: "comments of greylisted logins are not new",
			pr: minePR(func(pr *model.PR) {
				pr.Comments = []model.Comment{{ID: "c1", Kind: model.CommentReview, Author: "loft-bot", CreatedAt: at(5)}}
			}),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "needs action lists all reasons in table order",
			pr: minePR(func(pr *model.PR) {
				pr.Checks = model.ChecksFailure
				pr.ReviewDecision = "CHANGES_REQUESTED"
				pr.Mergeable = "CONFLICTING"
				pr.Comments = []model.Comment{{ID: "c1", Author: "other", CreatedAt: at(5)}}
			}),
			want: model.Classification{
				Section: model.SectionNeedsAction,
				Reasons: []string{"checks failing", "changes requested", "merge conflict", "new comments"},
			},
		},
		{
			name: "approved with failing checks is needs_action",
			pr: minePR(func(pr *model.PR) {
				pr.Checks = model.ChecksFailure
				pr.ReviewDecision = "APPROVED"
				pr.MergeStateStatus = "CLEAN"
			}),
			want: model.Classification{Section: model.SectionNeedsAction, Reasons: []string{"checks failing"}},
		},
		{
			name: "merge queue with a conflict is needs_action",
			pr: minePR(func(pr *model.PR) {
				pr.InMergeQueue = true
				pr.Mergeable = "CONFLICTING"
			}),
			want: model.Classification{Section: model.SectionNeedsAction, Reasons: []string{"merge conflict"}},
		},
		{
			name: "in merge queue",
			pr:   minePR(func(pr *model.PR) { pr.InMergeQueue = true }),
			want: model.Classification{Section: model.SectionReadyToMerge},
		},
		{
			name: "in merge queue even when not clean or approved",
			pr: minePR(func(pr *model.PR) {
				pr.InMergeQueue = true
				pr.MergeStateStatus = "BLOCKED"
				pr.ReviewDecision = "REVIEW_REQUIRED"
			}),
			want: model.Classification{Section: model.SectionReadyToMerge},
		},
		{
			name: "clean and approved",
			pr: minePR(func(pr *model.PR) {
				pr.MergeStateStatus = "CLEAN"
				pr.ReviewDecision = "APPROVED"
			}),
			want: model.Classification{Section: model.SectionReadyToMerge},
		},
		{
			name: "has hooks and approved",
			pr: minePR(func(pr *model.PR) {
				pr.MergeStateStatus = "HAS_HOOKS"
				pr.ReviewDecision = "APPROVED"
			}),
			want: model.Classification{Section: model.SectionReadyToMerge},
		},
		{
			name: "clean, no decision, approved review, no pending requests",
			pr: minePR(func(pr *model.PR) {
				pr.MergeStateStatus = "CLEAN"
				pr.Reviews = []model.Review{{ID: "r1", Author: "other", State: "APPROVED", SubmittedAt: at(2)}}
			}),
			want: model.Classification{Section: model.SectionReadyToMerge},
		},
		{
			name: "no decision, approved review, a pending request keeps it waiting",
			pr: minePR(func(pr *model.PR) {
				pr.MergeStateStatus = "CLEAN"
				pr.Reviews = []model.Review{{ID: "r1", Author: "other", State: "APPROVED", SubmittedAt: at(2)}}
				pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/other"}}
			}),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "no decision and no approved review",
			pr: minePR(func(pr *model.PR) {
				pr.MergeStateStatus = "CLEAN"
				pr.Reviews = []model.Review{{ID: "r1", Author: "other", State: "COMMENTED", SubmittedAt: at(2)}}
			}),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "no decision and no reviews",
			pr:   minePR(func(pr *model.PR) { pr.MergeStateStatus = "CLEAN" }),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "review required with an approved review is waiting",
			pr: minePR(func(pr *model.PR) {
				pr.MergeStateStatus = "CLEAN"
				pr.ReviewDecision = "REVIEW_REQUIRED"
				pr.Reviews = []model.Review{{ID: "r1", Author: "other", State: "APPROVED", SubmittedAt: at(2)}}
			}),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "approved but blocked is waiting",
			pr: minePR(func(pr *model.PR) {
				pr.MergeStateStatus = "BLOCKED"
				pr.ReviewDecision = "APPROVED"
			}),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "approved but behind is waiting",
			pr: minePR(func(pr *model.PR) {
				pr.MergeStateStatus = "BEHIND"
				pr.ReviewDecision = "APPROVED"
			}),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
		{
			name: "otherwise waiting",
			pr:   minePR(),
			want: model.Classification{Section: model.SectionWaitingForReview},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.pr, viewer, []string{"Loft-Bot"}); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Classify = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestClassifyNewComments(t *testing.T) {
	comment := func(id, author string, min int) model.Comment {
		return model.Comment{ID: id, Kind: model.CommentIssue, Author: author, CreatedAt: at(min)}
	}
	tests := []struct {
		name     string
		push     int
		comments []model.Comment
		reviews  []model.Review
		want     model.Section
	}{
		{
			name:     "comment by someone else after the push",
			push:     0,
			comments: []model.Comment{comment("c1", "other", 5)},
			want:     model.SectionNeedsAction,
		},
		{
			name:     "comment before the push is answered by it",
			push:     10,
			comments: []model.Comment{comment("c1", "other", 5)},
			want:     model.SectionWaitingForReview,
		},
		{
			name:     "comment at the push time is not newer",
			push:     5,
			comments: []model.Comment{comment("c1", "other", 5)},
			want:     model.SectionWaitingForReview,
		},
		{
			name:     "comment after an earlier push still counts",
			push:     5,
			comments: []model.Comment{comment("c1", "other", 6)},
			want:     model.SectionNeedsAction,
		},
		{
			name:     "viewer's own comment after others' resets",
			comments: []model.Comment{comment("c1", "other", 5), comment("c2", "viewer", 6)},
			want:     model.SectionWaitingForReview,
		},
		{
			name:     "viewer's own comment compares case-insensitively",
			comments: []model.Comment{comment("c1", "other", 5), comment("c2", "ViEwEr", 6)},
			want:     model.SectionWaitingForReview,
		},
		{
			name:     "comment after the viewer's own comment counts again",
			comments: []model.Comment{comment("c1", "viewer", 5), comment("c2", "other", 6)},
			want:     model.SectionNeedsAction,
		},
		{
			name:     "viewer's own comment before the others' does not reset",
			comments: []model.Comment{comment("c1", "viewer", 4), comment("c2", "other", 5)},
			want:     model.SectionNeedsAction,
		},
		{
			name:     "viewer's own review after others' comment resets",
			comments: []model.Comment{comment("c1", "other", 5)},
			reviews:  []model.Review{{ID: "r1", Author: "viewer", State: "COMMENTED", SubmittedAt: at(6)}},
			want:     model.SectionWaitingForReview,
		},
		{
			name:     "someone else's review does not reset",
			comments: []model.Comment{comment("c1", "other", 5)},
			reviews:  []model.Review{{ID: "r1", Author: "third", State: "COMMENTED", SubmittedAt: at(6)}},
			want:     model.SectionNeedsAction,
		},
		{
			name:     "bot comment is ignored",
			comments: []model.Comment{comment("c1", "github-actions[bot]", 5)},
			want:     model.SectionWaitingForReview,
		},
		{
			name:     "bot suffix compares case-insensitively",
			comments: []model.Comment{comment("c1", "Copilot[BOT]", 5)},
			want:     model.SectionWaitingForReview,
		},
		{
			name:     "human after a bot counts",
			comments: []model.Comment{comment("c1", "dependabot[bot]", 5), comment("c2", "other", 6)},
			want:     model.SectionNeedsAction,
		},
		{
			name: "no comments",
			want: model.SectionWaitingForReview,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := minePR(func(pr *model.PR) {
				pr.LastPushAt = at(tt.push)
				pr.Comments = tt.comments
				pr.Reviews = tt.reviews
			})
			if got := Classify(pr, viewer, nil).Section; got != tt.want {
				t.Fatalf("section = %q, want %q", got, tt.want)
			}
		})
	}
}

// eventData is how a consumer decodes model.EventData, with Change left for the typed decode.
type eventData struct {
	PR model.PR `json:"pr"`
	model.Classification
	Change json.RawMessage `json:"change"`
}

// want is an expected event: its type and the typed Change it must carry (nil for none).
type want struct {
	typ    string
	change any
}

func types(events []model.Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

func wantTypes(ws []want) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.typ
	}
	return out
}

// checkEvents asserts the events match ws in type and order, carry cur's key, snapshot and the
// classification wantCls, and that each Change decodes into the type of the expected one.
// withoutTerms checks that the score terms of cls sum to its score and returns cls without them, for
// tests about everything else.
func withoutTerms(t *testing.T, cls model.Classification) model.Classification {
	t.Helper()
	if n := sumTerms(cls.ScoreTerms); n != cls.Score {
		t.Errorf("score terms %+v sum to %d, score is %d", cls.ScoreTerms, n, cls.Score)
	}
	cls.ScoreTerms = nil
	return cls
}

func checkEvents(t *testing.T, events []model.Event, ws []want, cur model.PR, wantCls model.Classification) {
	t.Helper()
	if got, wantT := types(events), wantTypes(ws); !slices.Equal(got, wantT) {
		t.Fatalf("event types = %v, want %v", got, wantT)
	}
	for i, ev := range events {
		if ev.PR != cur.Key {
			t.Errorf("event %d (%s) PR = %v, want %v", i, ev.Type, ev.PR, cur.Key)
		}
		if ev.ID == "" || ev.Time.IsZero() {
			t.Errorf("event %d (%s) has no ID or time", i, ev.Type)
		}
		var data eventData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("event %d (%s) data: %v", i, ev.Type, err)
		}
		if data.PR.Key != cur.Key || data.PR.HeadSHA != cur.HeadSHA || data.PR.State != cur.State {
			t.Errorf("event %d (%s) snapshot = %+v, want cur", i, ev.Type, data.PR)
		}
		if len(data.PR.Comments) != 0 {
			t.Errorf("event %d (%s) snapshot carries %d comments", i, ev.Type, len(data.PR.Comments))
		}
		if !reflect.DeepEqual(withoutTerms(t, data.Classification), wantCls) {
			t.Errorf("event %d (%s) classification = %+v, want %+v", i, ev.Type, data.Classification, wantCls)
		}
		if ws[i].change == nil {
			if len(data.Change) != 0 {
				t.Errorf("event %d (%s) change = %s, want none", i, ev.Type, data.Change)
			}
			continue
		}
		got := reflect.New(reflect.TypeOf(ws[i].change))
		if err := json.Unmarshal(data.Change, got.Interface()); err != nil {
			t.Fatalf("event %d (%s) change %s: %v", i, ev.Type, data.Change, err)
		}
		if !reflect.DeepEqual(got.Elem().Interface(), ws[i].change) {
			t.Errorf("event %d (%s) change = %+v, want %+v", i, ev.Type, got.Elem().Interface(), ws[i].change)
		}
	}
}

func TestDiff(t *testing.T) {
	review := func(id, author, state string, min int) model.Review {
		return model.Review{ID: id, Author: author, State: state, SubmittedAt: at(min)}
	}
	comment := func(id, author string, min int) model.Comment {
		return model.Comment{ID: id, Kind: model.CommentIssue, Author: author, Body: id, CreatedAt: at(min)}
	}
	requestChange := func(req model.ReviewRequest, direct bool) model.ChangeReviewRequest {
		return model.ChangeReviewRequest{ReviewRequest: req, Direct: direct}
	}
	// mention is a comment that needs the viewer unless their reactions acknowledge it.
	mention := func(reactions ...string) model.Comment {
		c := comment("c1", "other", 5)
		c.Body, c.ViewerReactions = "@viewer ping", reactions
		return c
	}
	withComments := func(cs ...model.Comment) model.PR {
		return basePR(func(pr *model.PR) { pr.Comments = cs })
	}

	direct := []string{ReasonReviewRequested}
	directScore := DefaultWeights.Direct + DefaultWeights.NeedsMe
	mentioned := []string{ReasonMentioned}
	mentionedScore := DefaultWeights.Mentioned + DefaultWeights.NeedsMe

	tests := []struct {
		name  string
		old   *model.PR
		cur   model.PR
		want  []want
		score int // expected score of cur, e.g. directScore for a request naming the viewer personally
		// attention are the expected attention reasons of a foreign cur; they replace Classify's reasons
		// and set NeedsMe.
		attention []string
	}{
		{
			name: "discovery emits only discovered",
			cur: basePR(func(pr *model.PR) {
				pr.Comments = []model.Comment{comment("c1", "other", 5)}
				pr.Reviews = []model.Review{review("r1", "other", "APPROVED", 4)}
				pr.CheckRuns = []model.CheckRun{{Name: "ci", Conclusion: "FAILURE"}}
				pr.Checks = model.ChecksFailure
				pr.ReviewRequests = []model.ReviewRequest{{Login: "other"}, {Team: "acme-co/other"}}
			}),
			want: []want{{typ: model.EventDiscovered}},
		},
		{
			name: "discovery of a closed PR emits only discovered",
			cur:  basePR(func(pr *model.PR) { pr.State = model.PRStateMerged }),
			want: []want{{typ: model.EventDiscovered}},
		},
		{
			name: "discovery adds review_requested for the viewer and their teams",
			cur: basePR(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{
					{Login: "other"},
					{Team: "acme-co/core"},
					{Login: "Viewer"},
					{Team: "acme-co/other"},
				}
			}),
			want: []want{
				{model.EventDiscovered, nil},
				{model.EventReviewRequested, requestChange(model.ReviewRequest{Team: "acme-co/core"}, false)},
				{model.EventReviewRequested, requestChange(model.ReviewRequest{Login: "Viewer"}, true)},
			},
			score:     directScore,
			attention: direct,
		},
		{
			name: "identical snapshots emit nothing",
			old:  ptr(basePR()),
			cur:  basePR(),
			want: nil,
		},
		{
			name: "fetchedAt and comment bodies alone emit nothing",
			old:  ptr(basePR(func(pr *model.PR) { pr.Comments = []model.Comment{comment("c1", "other", 5)} })),
			cur: basePR(func(pr *model.PR) {
				pr.FetchedAt = at(60)
				c := comment("c1", "other", 5)
				c.Body = "edited"
				pr.Comments = []model.Comment{c}
			}),
			want: nil,
		},
		{
			name: "acknowledging reaction is an update that clears attention",
			old:  ptr(withComments(mention())),
			cur:  withComments(mention("THUMBS_UP")),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name:      "removed reaction is an update that restores attention",
			old:       ptr(withComments(mention("HEART"))),
			cur:       withComments(mention()),
			want:      []want{{model.EventUpdated, nil}},
			score:     mentionedScore,
			attention: mentioned,
		},
		{
			name:      "any reaction change is an update",
			old:       ptr(withComments(mention("EYES"))),
			cur:       withComments(mention("EYES", "CONFUSED")),
			want:      []want{{model.EventUpdated, nil}},
			score:     mentionedScore,
			attention: mentioned,
		},
		{
			name: "unchanged reactions emit nothing",
			old:  ptr(withComments(mention("THUMBS_UP"))),
			cur:  withComments(mention("THUMBS_UP")),
			want: nil,
		},
		{
			name: "nil versus empty reactions emit nothing",
			old:  ptr(withComments(mention([]string{}...))),
			cur:  withComments(mention()),
			want: nil,
		},
		{
			name: "reactions on a new comment add no second update",
			old:  ptr(basePR()),
			cur:  withComments(mention("ROCKET")),
			want: []want{{model.EventCommentAdded, mention("ROCKET")}, {model.EventUpdated, nil}},
		},
		{
			name: "reactions on a vanished comment emit nothing",
			old:  ptr(withComments(mention("THUMBS_UP"))),
			cur:  basePR(),
			want: nil,
		},
		{
			name: "nil versus empty slices and zone-shifted times emit nothing",
			old: ptr(basePR(func(pr *model.PR) {
				pr.Labels = []string{}
				pr.ReviewRequests = []model.ReviewRequest{}
				pr.Reviews = []model.Review{review("r1", "other", "APPROVED", 4)}
			})),
			cur: basePR(func(pr *model.PR) {
				pr.Labels = nil
				pr.ReviewRequests = nil
				r := review("r1", "other", "APPROVED", 4)
				r.SubmittedAt = r.SubmittedAt.In(time.FixedZone("CEST", 2*60*60))
				pr.Reviews = []model.Review{r}
				pr.UpdatedAt = pr.UpdatedAt.In(time.FixedZone("PST", -8*60*60))
			}),
			want: nil,
		},
		{
			name: "merged",
			old:  ptr(minePR()),
			cur:  minePR(func(pr *model.PR) { pr.State = model.PRStateMerged }),
			want: []want{
				{model.EventMerged, nil},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionWaitingForReview, To: model.SectionNone}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "closed",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.State = model.PRStateClosed }),
			want: []want{
				{model.EventClosed, nil},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionInvolved, To: model.SectionNone}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "reopened",
			old:  ptr(basePR(func(pr *model.PR) { pr.State = model.PRStateClosed })),
			cur:  basePR(),
			want: []want{
				{model.EventReopened, nil},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionNone, To: model.SectionInvolved}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "review requested from the viewer",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}} }),
			want: []want{
				{model.EventReviewRequested, requestChange(model.ReviewRequest{Login: "viewer"}, true)},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionInvolved, To: model.SectionReviewRequested}},
				{model.EventUpdated, nil},
			},
			score:     directScore,
			attention: direct,
		},
		{
			name: "review requested from a viewer team",
			old:  ptr(basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}} })),
			cur: basePR(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}, {Team: "ACME-CO/core"}}
			}),
			want: []want{
				{model.EventReviewRequested, requestChange(model.ReviewRequest{Team: "ACME-CO/core"}, false)},
				{model.EventUpdated, nil},
			},
			score:     directScore,
			attention: direct,
		},
		{
			name: "requests for others are not events but still update the PR",
			old:  ptr(basePR()),
			cur: basePR(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Login: "other"}, {Team: "acme-co/other"}}
			}),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name: "review request removed",
			old:  ptr(basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}} })),
			cur:  basePR(),
			want: []want{
				{model.EventReviewRequestRemoved, requestChange(model.ReviewRequest{Login: "viewer"}, true)},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionReviewRequested, To: model.SectionInvolved}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "team review request removed",
			old:  ptr(basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/core"}} })),
			cur:  basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "other"}} }),
			want: []want{
				{model.EventReviewRequestRemoved, requestChange(model.ReviewRequest{Team: "acme-co/core"}, false)},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionReviewRequested, To: model.SectionInvolved}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "review request swapped from team to viewer",
			old:  ptr(basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/core"}} })),
			cur:  basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}} }),
			want: []want{
				{model.EventReviewRequestRemoved, requestChange(model.ReviewRequest{Team: "acme-co/core"}, false)},
				{model.EventReviewRequested, requestChange(model.ReviewRequest{Login: "viewer"}, true)},
				{model.EventUpdated, nil},
			},
			score:     directScore,
			attention: direct,
		},
		{
			name: "commits pushed",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.HeadSHA = "bbb" }),
			want: []want{
				{model.EventCommitsPushed, model.ChangeCommits{Before: "aaa", After: "bbb"}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "review submitted, oldest first, skipping known reviews",
			old:  ptr(basePR(func(pr *model.PR) { pr.Reviews = []model.Review{review("r1", "a", "COMMENTED", 1)} })),
			cur: basePR(func(pr *model.PR) {
				pr.Reviews = []model.Review{
					review("r3", "c", "APPROVED", 9),
					review("r1", "a", "COMMENTED", 1),
					review("r2", "viewer", "CHANGES_REQUESTED", 5),
				}
			}),
			want: []want{
				{model.EventReviewSubmitted, review("r2", "viewer", "CHANGES_REQUESTED", 5)},
				{model.EventReviewSubmitted, review("r3", "c", "APPROVED", 9)},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "review state change on a known review is only an update",
			old:  ptr(basePR(func(pr *model.PR) { pr.Reviews = []model.Review{review("r1", "a", "APPROVED", 1)} })),
			cur:  basePR(func(pr *model.PR) { pr.Reviews = []model.Review{review("r1", "a", "DISMISSED", 1)} }),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name: "comment added, oldest first, skipping known comments",
			old:  ptr(basePR(func(pr *model.PR) { pr.Comments = []model.Comment{comment("c1", "a", 1)} })),
			cur: basePR(func(pr *model.PR) {
				pr.Comments = []model.Comment{comment("c1", "a", 1), comment("c3", "b", 9), comment("c2", "viewer", 5)}
			}),
			want: []want{
				{model.EventCommentAdded, comment("c2", "viewer", 5)},
				{model.EventCommentAdded, comment("c3", "b", 9)},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "bot comments are still comment_added",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.Comments = []model.Comment{comment("c1", "ci[bot]", 5)} }),
			want: []want{
				{model.EventCommentAdded, comment("c1", "ci[bot]", 5)},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "comment added on a PR the viewer authored moves it to needs_action",
			old:  ptr(minePR()),
			cur:  minePR(func(pr *model.PR) { pr.Comments = []model.Comment{comment("c1", "other", 5)} }),
			want: []want{
				{model.EventCommentAdded, comment("c1", "other", 5)},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionWaitingForReview, To: model.SectionNeedsAction}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "checks failed",
			old:  ptr(basePR(func(pr *model.PR) { pr.Checks = model.ChecksPending })),
			cur:  basePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure }),
			want: []want{
				{model.EventChecksFailed, model.ChangeChecks{Before: model.ChecksPending, After: model.ChecksFailure}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "checks failed scores each failing check",
			old:  ptr(basePR(func(pr *model.PR) { pr.Checks = model.ChecksPending; pr.ChecksRunning = 2 })),
			cur:  basePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure; pr.ChecksFailing = 2 }),
			want: []want{
				{model.EventChecksFailed, model.ChangeChecks{Before: model.ChecksPending, After: model.ChecksFailure}},
				{model.EventUpdated, nil},
			},
			score: 2 * DefaultWeights.PerFailingCheck,
		},
		{
			name: "checks failed from success",
			old:  ptr(basePR(func(pr *model.PR) { pr.Checks = model.ChecksSuccess })),
			cur:  basePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure }),
			want: []want{
				{model.EventChecksFailed, model.ChangeChecks{Before: model.ChecksSuccess, After: model.ChecksFailure}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "checks passed",
			old:  ptr(basePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure })),
			cur:  basePR(func(pr *model.PR) { pr.Checks = model.ChecksSuccess }),
			want: []want{
				{model.EventChecksPassed, model.ChangeChecks{Before: model.ChecksFailure, After: model.ChecksSuccess}},
				{model.EventUpdated, nil},
			},
			score: DefaultWeights.ChecksSuccess,
		},
		{
			name: "checks passed on a first rollup",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.Checks = model.ChecksSuccess }),
			want: []want{
				{model.EventChecksPassed, model.ChangeChecks{Before: model.ChecksNone, After: model.ChecksSuccess}},
				{model.EventUpdated, nil},
			},
			score: DefaultWeights.ChecksSuccess,
		},
		{
			name: "checks going pending is only an update",
			old:  ptr(basePR(func(pr *model.PR) { pr.Checks = model.ChecksSuccess })),
			cur:  basePR(func(pr *model.PR) { pr.Checks = model.ChecksPending }),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name: "check run change under an unchanged rollup is only an update",
			old: ptr(basePR(func(pr *model.PR) {
				pr.Checks = model.ChecksPending
				pr.CheckRuns = []model.CheckRun{{Name: "ci", Status: "QUEUED"}}
			})),
			cur: basePR(func(pr *model.PR) {
				pr.Checks = model.ChecksPending
				pr.CheckRuns = []model.CheckRun{{Name: "ci", Status: "IN_PROGRESS"}}
			}),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name: "marked ready for review",
			old:  ptr(minePR(func(pr *model.PR) { pr.IsDraft = true })),
			cur:  minePR(),
			want: []want{
				{model.EventDraftChanged, model.ChangeDraft{IsDraft: false}},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionDrafts, To: model.SectionWaitingForReview}},
				{model.EventUpdated, nil},
			},
		},
		{
			name: "converted to draft",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.IsDraft = true }),
			want: []want{
				{model.EventDraftChanged, model.ChangeDraft{IsDraft: true}},
				{model.EventUpdated, nil},
			},
			score: DefaultWeights.Draft,
		},
		{
			name: "section changed without another trigger",
			old:  ptr(minePR(func(pr *model.PR) { pr.MergeStateStatus = "BLOCKED"; pr.ReviewDecision = "APPROVED" })),
			cur:  minePR(func(pr *model.PR) { pr.MergeStateStatus = "CLEAN"; pr.ReviewDecision = "APPROVED" }),
			want: []want{
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionWaitingForReview, To: model.SectionReadyToMerge}},
				{model.EventUpdated, nil},
			},
		},
		{
			name:  "same section with different reasons is no section change",
			old:   ptr(minePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure })),
			cur:   minePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure; pr.Mergeable = "CONFLICTING" }),
			want:  []want{{model.EventUpdated, nil}},
			score: DefaultWeights.MergeConflict,
		},
		{
			name: "title change is only an update",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.Title = "new title" }),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name: "label change is only an update",
			old:  ptr(basePR(func(pr *model.PR) { pr.Labels = []string{"a"} })),
			cur:  basePR(func(pr *model.PR) { pr.Labels = []string{"a", "b"} }),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name: "updatedAt change is only an update",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.UpdatedAt = at(30) }),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name: "review decision change is only an update",
			old:  ptr(basePR()),
			cur:  basePR(func(pr *model.PR) { pr.ReviewDecision = "APPROVED" }),
			want: []want{{model.EventUpdated, nil}},
		},
		{
			name: "every kind of change in the documented order",
			old: ptr(basePR(func(pr *model.PR) {
				pr.IsDraft = true
				pr.Checks = model.ChecksPending
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
				pr.Reviews = []model.Review{review("r1", "a", "COMMENTED", 1)}
				pr.Comments = []model.Comment{comment("c1", "a", 1)}
			})),
			cur: basePR(func(pr *model.PR) {
				pr.State = model.PRStateMerged
				pr.IsDraft = false
				pr.HeadSHA = "bbb"
				pr.Checks = model.ChecksFailure
				pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/core"}}
				pr.Reviews = []model.Review{review("r1", "a", "COMMENTED", 1), review("r3", "c", "APPROVED", 9), review("r2", "b", "COMMENTED", 5)}
				pr.Comments = []model.Comment{comment("c1", "a", 1), comment("c3", "c", 9), comment("c2", "b", 5)}
			}),
			want: []want{
				{model.EventMerged, nil},
				{model.EventReviewRequestRemoved, requestChange(model.ReviewRequest{Login: "viewer"}, true)},
				{model.EventReviewRequested, requestChange(model.ReviewRequest{Team: "acme-co/core"}, false)},
				{model.EventCommitsPushed, model.ChangeCommits{Before: "aaa", After: "bbb"}},
				{model.EventReviewSubmitted, review("r2", "b", "COMMENTED", 5)},
				{model.EventReviewSubmitted, review("r3", "c", "APPROVED", 9)},
				{model.EventCommentAdded, comment("c2", "b", 5)},
				{model.EventCommentAdded, comment("c3", "c", 9)},
				{model.EventChecksFailed, model.ChangeChecks{Before: model.ChecksPending, After: model.ChecksFailure}},
				{model.EventDraftChanged, model.ChangeDraft{IsDraft: false}},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionReviewRequested, To: model.SectionNone}},
				{model.EventUpdated, nil},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := Diff(tt.old, tt.cur, inputs)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			wantCls := Classify(tt.cur, viewer, nil)
			wantCls.Score = tt.score
			if tt.attention != nil {
				wantCls.Reasons, wantCls.NeedsMe = tt.attention, true
			}
			checkEvents(t, events, tt.want, tt.cur, wantCls)
		})
	}
}

func TestDiffDoesNotMutateInputs(t *testing.T) {
	old := basePR(func(pr *model.PR) { pr.Comments = []model.Comment{{ID: "c1", CreatedAt: at(1)}} })
	cur := basePR(func(pr *model.PR) {
		pr.Comments = []model.Comment{{ID: "c3", CreatedAt: at(9)}, {ID: "c2", CreatedAt: at(5)}, {ID: "c1", CreatedAt: at(1)}}
		pr.Reviews = []model.Review{{ID: "r2", SubmittedAt: at(9)}, {ID: "r1", SubmittedAt: at(5)}}
	})
	wantCur, wantOld := clonePR(cur), clonePR(old)
	if _, err := Diff(&old, cur, inputs); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !reflect.DeepEqual(cur, wantCur) || !reflect.DeepEqual(old, wantOld) {
		t.Fatal("Diff mutated its inputs")
	}
}

func clonePR(pr model.PR) model.PR {
	pr.Comments = slices.Clone(pr.Comments)
	pr.Reviews = slices.Clone(pr.Reviews)
	return pr
}

// teamPoints is the boost of team in tests.
const teamPoints = 30

// team is one boosted team of the given members.
func team(points int, members ...string) []TeamBoost {
	return []TeamBoost{{Team: "acme-co/core", Points: points, Members: members}}
}

func annotation(commentID, key string) model.Annotation {
	return model.Annotation{PR: basePR().Key, CommentID: commentID, Key: key, Value: json.RawMessage(`true`)}
}

func byAuthor(author string) model.PR { return basePR(func(pr *model.PR) { pr.Author = author }) }

func withChecks(state model.ChecksState, running, failing int) model.PR {
	return basePR(func(pr *model.PR) {
		pr.Checks, pr.ChecksRunning, pr.ChecksFailing = state, running, failing
	})
}

func withMergeable(state string) model.PR {
	return basePR(func(pr *model.PR) { pr.Mergeable = state })
}

func TestScore(t *testing.T) {
	direct := model.Classification{Section: model.SectionReviewRequested, Direct: true}
	urgent := []model.Annotation{annotation("", UrgencyKey)}

	tests := []struct {
		name string
		pr   model.PR
		cls  model.Classification
		in   Inputs
		want int
	}{
		{
			name: "empty config scores zero",
			pr:   basePR(),
			in:   Inputs{Viewer: viewer},
			want: 0,
		},
		{
			name: "urgent",
			pr:   basePR(),
			in:   Inputs{Annotations: urgent},
			want: DefaultWeights.Urgent,
		},
		{
			name: "urgent among other annotations counts once",
			pr:   basePR(),
			in: Inputs{Annotations: []model.Annotation{
				annotation("", "linear.team"),
				annotation("", UrgencyKey),
				annotation("", UrgencyKey),
			}},
			want: DefaultWeights.Urgent,
		},
		{
			name: "comment-level urgency is ignored",
			pr:   basePR(),
			in:   Inputs{Annotations: []model.Annotation{annotation("c1", UrgencyKey)}},
			want: 0,
		},
		{
			name: "comment-level urgency does not mask a PR-level one",
			pr:   basePR(),
			in:   Inputs{Annotations: []model.Annotation{annotation("c1", UrgencyKey), annotation("", UrgencyKey)}},
			want: DefaultWeights.Urgent,
		},
		{
			name: "urgency key must match exactly",
			pr:   basePR(),
			in: Inputs{Annotations: []model.Annotation{
				annotation("", "Linear.Urgency"),
				annotation("", "linear.urgency.level"),
				annotation("", "urgency"),
			}},
			want: 0,
		},
		{
			name: "preferred",
			pr:   byAuthor("alice"),
			in:   Inputs{Priority: Priority{Teams: team(teamPoints, "bob", "alice")}},
			want: teamPoints,
		},
		{
			name: "preferred compares case-insensitively",
			pr:   byAuthor("Alice"),
			in:   Inputs{Priority: Priority{Teams: team(teamPoints, "aLICE")}},
			want: teamPoints,
		},
		{
			name: "author outside the preferred team",
			pr:   byAuthor("carol"),
			in:   Inputs{Priority: Priority{Teams: team(teamPoints, "alice", "bob")}},
			want: 0,
		},
		{
			name: "empty author is not preferred",
			pr:   byAuthor(""),
			in:   Inputs{Priority: Priority{Teams: team(teamPoints, "")}},
			want: 0,
		},
		{
			name: "greylisted",
			pr:   byAuthor("noisy"),
			in:   Inputs{Priority: Priority{Greylist: []string{"quiet", "noisy"}}},
			want: DefaultWeights.Greylist,
		},
		{
			name: "greylist entry without suffix matches a bot author",
			pr:   byAuthor("dependabot[bot]"),
			in:   Inputs{Priority: Priority{Greylist: []string{"dependabot"}}},
			want: DefaultWeights.Greylist,
		},
		{
			name: "greylist entry with suffix matches an author without it",
			pr:   byAuthor("dependabot"),
			in:   Inputs{Priority: Priority{Greylist: []string{"dependabot[bot]"}}},
			want: DefaultWeights.Greylist,
		},
		{
			name: "greylist matches with suffix on both sides",
			pr:   byAuthor("dependabot[bot]"),
			in:   Inputs{Priority: Priority{Greylist: []string{"dependabot[bot]"}}},
			want: DefaultWeights.Greylist,
		},
		{
			name: "greylist compares case-insensitively",
			pr:   byAuthor("dependabot"),
			in:   Inputs{Priority: Priority{Greylist: []string{"Dependabot[bot]"}}},
			want: DefaultWeights.Greylist,
		},
		{
			name: "greylist compares the bot suffix case-insensitively",
			pr:   byAuthor("Renovate[BOT]"),
			in:   Inputs{Priority: Priority{Greylist: []string{"renovate"}}},
			want: DefaultWeights.Greylist,
		},
		{
			name: "greylist does not match a different login sharing a prefix",
			pr:   byAuthor("dependabot-preview[bot]"),
			in:   Inputs{Priority: Priority{Greylist: []string{"dependabot[bot]"}}},
			want: 0,
		},
		{
			name: "greylist only strips a trailing suffix",
			pr:   byAuthor("my[bot]helper"),
			in:   Inputs{Priority: Priority{Greylist: []string{"my"}}},
			want: 0,
		},
		{
			name: "empty greylist entry does not match an empty author",
			pr:   byAuthor(""),
			in:   Inputs{Priority: Priority{Greylist: []string{"", "[bot]"}}},
			want: 0,
		},
		{
			name: "direct",
			pr:   basePR(),
			cls:  direct,
			in:   Inputs{Viewer: viewer},
			want: DefaultWeights.Direct,
		},
		{
			name: "team request is not direct",
			pr:   basePR(),
			cls:  model.Classification{Section: model.SectionReviewRequested},
			in:   Inputs{Viewer: viewer},
			want: 0,
		},
		{
			name: "preferred, direct and urgent",
			pr:   byAuthor("alice"),
			cls:  direct,
			in:   Inputs{Priority: Priority{Teams: team(teamPoints, "alice")}, Annotations: urgent},
			want: 60,
		},
		{
			name: "greylisted bot",
			pr:   byAuthor("dependabot[bot]"),
			in:   Inputs{Priority: Priority{Greylist: []string{"dependabot"}}},
			want: -15,
		},
		{
			name: "greylisted bot with a direct request",
			pr:   byAuthor("dependabot[bot]"),
			cls:  direct,
			in:   Inputs{Priority: Priority{Greylist: []string{"dependabot"}}},
			want: -5,
		},
		{
			name: "greylisted bot that is also urgent",
			pr:   byAuthor("dependabot[bot]"),
			in:   Inputs{Priority: Priority{Greylist: []string{"dependabot"}}, Annotations: urgent},
			want: 5,
		},
		{
			name: "author both preferred and greylisted",
			pr:   byAuthor("alice"),
			in:   Inputs{Priority: Priority{Teams: team(teamPoints, "alice"), Greylist: []string{"alice"}}},
			want: 15,
		},
		{
			name: "all four terms",
			pr:   byAuthor("alice"),
			cls:  direct,
			in: Inputs{
				Priority:    Priority{Teams: team(teamPoints, "alice"), Greylist: []string{"alice"}},
				Annotations: urgent,
			},
			want: 45,
		},
		{
			name: "no checks score zero",
			pr:   withChecks(model.ChecksNone, 0, 0),
			want: 0,
		},
		{
			name: "pending rollup without running checks scores zero",
			pr:   withChecks(model.ChecksPending, 0, 0),
			want: 0,
		},
		{
			name: "failure rollup without failing checks scores zero",
			pr:   withChecks(model.ChecksFailure, 0, 0),
			want: 0,
		},
		{
			name: "running check",
			pr:   withChecks(model.ChecksPending, 1, 0),
			want: DefaultWeights.ChecksRunning,
		},
		{
			name: "many running checks count once",
			pr:   withChecks(model.ChecksPending, 12, 0),
			want: DefaultWeights.ChecksRunning,
		},
		{
			name: "successful rollup",
			pr:   withChecks(model.ChecksSuccess, 0, 0),
			want: DefaultWeights.ChecksSuccess,
		},
		{
			name: "failing check",
			pr:   withChecks(model.ChecksFailure, 0, 1),
			want: DefaultWeights.PerFailingCheck,
		},
		{
			name: "failing checks add up",
			pr:   withChecks(model.ChecksFailure, 0, 3),
			want: 3 * DefaultWeights.PerFailingCheck,
		},
		{
			name: "many failing checks",
			pr:   withChecks(model.ChecksFailure, 0, 50),
			want: -500,
		},
		{
			name: "two failing and one running check",
			pr:   withChecks(model.ChecksPending, 1, 2),
			want: -15,
		},
		{
			name: "running check under a successful rollup",
			pr:   withChecks(model.ChecksSuccess, 1, 0),
			want: DefaultWeights.ChecksSuccess + DefaultWeights.ChecksRunning,
		},
		{
			name: "failing check under a successful rollup",
			pr:   withChecks(model.ChecksSuccess, 0, 1),
			want: DefaultWeights.ChecksSuccess + DefaultWeights.PerFailingCheck,
		},
		{
			name: "successful checks and urgent",
			pr:   withChecks(model.ChecksSuccess, 0, 0),
			in:   Inputs{Annotations: urgent},
			want: DefaultWeights.Urgent + DefaultWeights.ChecksSuccess,
		},
		{
			name: "successful checks on a greylisted bot",
			pr: basePR(func(pr *model.PR) {
				pr.Author = "dependabot[bot]"
				pr.Checks = model.ChecksSuccess
			}),
			in:   Inputs{Priority: Priority{Greylist: []string{"dependabot"}}},
			want: DefaultWeights.Greylist + DefaultWeights.ChecksSuccess,
		},
		{
			name: "failing checks outweigh a direct request",
			pr:   withChecks(model.ChecksFailure, 0, 2),
			cls:  direct,
			in:   Inputs{Viewer: viewer},
			want: DefaultWeights.Direct + 2*DefaultWeights.PerFailingCheck,
		},
		{
			name: "failing checks pull a greylisted PR further down",
			pr: basePR(func(pr *model.PR) {
				pr.Author = "noisy"
				pr.Checks, pr.ChecksFailing = model.ChecksFailure, 3
			}),
			in:   Inputs{Priority: Priority{Greylist: []string{"noisy"}}},
			want: DefaultWeights.Greylist + 3*DefaultWeights.PerFailingCheck,
		},
		{
			name: "merge conflict",
			pr:   withMergeable("CONFLICTING"),
			want: DefaultWeights.MergeConflict,
		},
		{
			name: "mergeable adds nothing",
			pr:   withMergeable("MERGEABLE"),
			want: 0,
		},
		{
			name: "unknown mergeability adds nothing",
			pr:   withMergeable("UNKNOWN"),
			want: 0,
		},
		{
			name: "unset mergeability adds nothing",
			pr:   withMergeable(""),
			want: 0,
		},
		{
			name: "merge conflict and failing checks",
			pr: basePR(func(pr *model.PR) {
				pr.Mergeable = "CONFLICTING"
				pr.Checks, pr.ChecksFailing = model.ChecksFailure, 2
			}),
			want: DefaultWeights.MergeConflict + 2*DefaultWeights.PerFailingCheck,
		},
		{
			name: "merge conflict on an urgent PR",
			pr:   withMergeable("CONFLICTING"),
			in:   Inputs{Annotations: urgent},
			want: DefaultWeights.Urgent + DefaultWeights.MergeConflict,
		},
		{
			name: "merge conflict under a successful rollup",
			pr: basePR(func(pr *model.PR) {
				pr.Mergeable = "CONFLICTING"
				pr.Checks = model.ChecksSuccess
			}),
			want: DefaultWeights.ChecksSuccess + DefaultWeights.MergeConflict,
		},
		{
			name: "draft",
			pr:   basePR(func(pr *model.PR) { pr.IsDraft = true }),
			want: DefaultWeights.Draft,
		},
		{
			name: "all terms",
			pr: basePR(func(pr *model.PR) {
				pr.Author = "alice"
				pr.Checks, pr.ChecksRunning, pr.ChecksFailing = model.ChecksPending, 1, 2
				pr.Mergeable = "CONFLICTING"
				pr.IsDraft = true
			}),
			cls: direct,
			in: Inputs{
				Priority:    Priority{Teams: team(teamPoints, "alice"), Greylist: []string{"alice"}},
				Annotations: urgent,
			},
			want: 45 + DefaultWeights.ChecksRunning + 2*DefaultWeights.PerFailingCheck + DefaultWeights.MergeConflict +
				DefaultWeights.Draft,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.Priority.Weights = DefaultWeights
			if got := Score(tt.pr, tt.cls, tt.in); got != tt.want {
				t.Fatalf("Score = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestScoreWeights(t *testing.T) {
	// Distinct weights make a wrongly wired term show up in the sum.
	w := Weights{
		Urgent:          7,
		Greylist:        -13,
		Direct:          17,
		ChecksRunning:   19,
		ChecksSuccess:   23,
		PerFailingCheck: -29,
		MergeConflict:   -31,
		Draft:           -37,
	}
	custom := func(mut func(*Weights)) Weights {
		out := w
		mut(&out)
		return out
	}
	urgent := []model.Annotation{annotation("", UrgencyKey)}
	direct := model.Classification{Section: model.SectionReviewRequested, Direct: true}
	alice := []string{"alice"}
	// everything triggers every term: 7+11-13+17+19+23-2*29-31-37.
	everything := basePR(func(pr *model.PR) {
		pr.Author = "alice"
		pr.Checks, pr.ChecksRunning, pr.ChecksFailing = model.ChecksSuccess, 1, 2
		pr.Mergeable = "CONFLICTING"
		pr.IsDraft = true
	})
	const everythingScore = -62
	all := func(weights Weights) Inputs {
		return Inputs{
			Priority:    Priority{Teams: team(11, alice...), Greylist: alice, Weights: weights},
			Annotations: urgent,
		}
	}

	tests := []struct {
		name string
		pr   model.PR
		cls  model.Classification
		in   Inputs
		want int
	}{
		{
			name: "urgent",
			pr:   basePR(),
			in:   Inputs{Priority: Priority{Weights: w}, Annotations: urgent},
			want: w.Urgent,
		},
		{
			name: "preferred team",
			pr:   byAuthor("alice"),
			in:   Inputs{Priority: Priority{Teams: team(11, alice...), Weights: w}},
			want: 11,
		},
		{
			name: "greylist",
			pr:   byAuthor("alice"),
			in:   Inputs{Priority: Priority{Greylist: alice, Weights: w}},
			want: w.Greylist,
		},
		{
			name: "direct",
			pr:   basePR(),
			cls:  direct,
			in:   Inputs{Priority: Priority{Weights: w}},
			want: w.Direct,
		},
		{
			name: "checks running",
			pr:   withChecks(model.ChecksPending, 1, 0),
			in:   Inputs{Priority: Priority{Weights: w}},
			want: w.ChecksRunning,
		},
		{
			name: "checks success",
			pr:   withChecks(model.ChecksSuccess, 0, 0),
			in:   Inputs{Priority: Priority{Weights: w}},
			want: w.ChecksSuccess,
		},
		{
			name: "each failing check",
			pr:   withChecks(model.ChecksFailure, 0, 3),
			in:   Inputs{Priority: Priority{Weights: w}},
			want: 3 * w.PerFailingCheck,
		},
		{
			name: "merge conflict",
			pr:   withMergeable("CONFLICTING"),
			in:   Inputs{Priority: Priority{Weights: w}},
			want: w.MergeConflict,
		},
		{
			name: "draft",
			pr:   basePR(func(pr *model.PR) { pr.IsDraft = true }),
			in:   Inputs{Priority: Priority{Weights: w}},
			want: w.Draft,
		},
		{
			name: "mergeable adds nothing",
			pr:   withMergeable("MERGEABLE"),
			in:   Inputs{Priority: Priority{Weights: w}},
			want: 0,
		},
		{
			name: "unknown mergeability adds nothing",
			pr:   withMergeable("UNKNOWN"),
			in:   Inputs{Priority: Priority{Weights: w}},
			want: 0,
		},
		{
			name: "all terms",
			pr:   everything,
			cls:  direct,
			in:   all(w),
			want: everythingScore,
		},
		{
			name: "zero weights leave only the team boost",
			pr:   everything,
			cls:  direct,
			in:   all(Weights{}),
			want: 11,
		},
		{
			name: "unset weights do not fall back to the defaults",
			pr:   everything,
			cls:  direct,
			in:   all(Weights{Urgent: 5}),
			want: 5 + 11,
		},
		{
			name: "zero urgent disables only urgent",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.Urgent = 0 })),
			want: everythingScore - w.Urgent,
		},
		{
			name: "zero greylist disables only greylist",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.Greylist = 0 })),
			want: everythingScore - w.Greylist,
		},
		{
			name: "zero direct disables only direct",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.Direct = 0 })),
			want: everythingScore - w.Direct,
		},
		{
			name: "zero checks running disables only checks running",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.ChecksRunning = 0 })),
			want: everythingScore - w.ChecksRunning,
		},
		{
			name: "zero checks success disables only checks success",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.ChecksSuccess = 0 })),
			want: everythingScore - w.ChecksSuccess,
		},
		{
			name: "zero per failing check disables only failing checks",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.PerFailingCheck = 0 })),
			want: everythingScore - 2*w.PerFailingCheck,
		},
		{
			name: "zero merge conflict disables only merge conflict",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.MergeConflict = 0 })),
			want: everythingScore - w.MergeConflict,
		},
		{
			name: "draft disabled",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.Draft = 0 })),
			want: everythingScore - w.Draft,
		},
		{
			name: "zero merge conflict adds nothing to a conflicting PR",
			pr:   withMergeable("CONFLICTING"),
			in:   Inputs{Priority: Priority{Weights: Weights{}}},
			want: 0,
		},
		{
			name: "negative urgent",
			pr:   basePR(),
			in:   Inputs{Priority: Priority{Weights: Weights{Urgent: -20}}, Annotations: urgent},
			want: -20,
		},
		{
			name: "negative direct",
			pr:   basePR(),
			cls:  direct,
			in:   Inputs{Priority: Priority{Weights: Weights{Direct: -10}}},
			want: -10,
		},
		{
			name: "positive greylist",
			pr:   byAuthor("alice"),
			in:   Inputs{Priority: Priority{Greylist: alice, Weights: Weights{Greylist: 15}}},
			want: 15,
		},
		{
			name: "positive per failing check",
			pr:   withChecks(model.ChecksFailure, 0, 2),
			in:   Inputs{Priority: Priority{Weights: Weights{PerFailingCheck: 5}}},
			want: 10,
		},
		{
			name: "negative checks success",
			pr:   withChecks(model.ChecksSuccess, 0, 0),
			in:   Inputs{Priority: Priority{Weights: Weights{ChecksSuccess: -4}}},
			want: -4,
		},
		{
			name: "positive merge conflict",
			pr:   withMergeable("CONFLICTING"),
			in:   Inputs{Priority: Priority{Weights: Weights{MergeConflict: 8}}},
			want: 8,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Score(tt.pr, tt.cls, tt.in); got != tt.want {
				t.Fatalf("Score = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name string
		pr   model.PR
		in   Inputs
		want model.Classification
	}{
		{
			name: "empty config keeps a zero score",
			pr:   basePR(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/core"}} }),
			in:   inputs,
			want: model.Classification{Section: model.SectionReviewRequested},
		},
		{
			name: "score from the classification and the config",
			pr: basePR(func(pr *model.PR) {
				pr.Author = "alice"
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
			}),
			in: Inputs{
				Viewer:      viewer,
				Priority:    Priority{Teams: team(teamPoints, "alice"), Weights: DefaultWeights},
				Annotations: []model.Annotation{annotation("", UrgencyKey)},
			},
			want: model.Classification{
				Section: model.SectionReviewRequested,
				Direct:  true,
				Reasons: []string{ReasonReviewRequested},
				NeedsMe: true,
				Score:   60 + DefaultWeights.NeedsMe,
			},
		},
		{
			name: "score uses the configured weights",
			pr: basePR(func(pr *model.PR) {
				pr.Author = "alice"
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
			}),
			in: Inputs{
				Viewer: viewer,
				Priority: Priority{
					Teams:   team(2, "alice"),
					Weights: Weights{Urgent: 1, Direct: 4, NeedsMe: 8},
				},
				Annotations: []model.Annotation{annotation("", UrgencyKey)},
			},
			want: model.Classification{
				Section: model.SectionReviewRequested,
				Direct:  true,
				Reasons: []string{ReasonReviewRequested},
				NeedsMe: true,
				Score:   15,
			},
		},
		{
			name: "own PR gets no attention reasons, needs-me or annotation contributions",
			pr: minePR(func(pr *model.PR) {
				pr.Comments = []model.Comment{{ID: "c1", Kind: model.CommentIssue, Author: "other", Body: "@viewer?", CreatedAt: at(5)}}
			}),
			in: Inputs{
				Viewer:   viewer,
				Priority: Priority{Weights: DefaultWeights},
				Annotations: []model.Annotation{{
					PR: basePR().Key, Key: "jev.attention", UpdatedAt: at(10),
					Value: json.RawMessage(`{"score":50,"attention":["question for you"]}`),
				}},
			},
			want: model.Classification{Section: model.SectionNeedsAction, Reasons: []string{"new comments"}},
		},
		{
			name: "closed foreign PR gets no attention",
			pr: basePR(func(pr *model.PR) {
				pr.State = model.PRStateClosed
				pr.Comments = []model.Comment{{ID: "c1", Kind: model.CommentIssue, Author: "other", Body: "@viewer?", CreatedAt: at(5)}}
			}),
			in: Inputs{
				Viewer:   viewer,
				Priority: Priority{Weights: DefaultWeights},
				Annotations: []model.Annotation{{
					PR: basePR().Key, Key: "jev.attention", UpdatedAt: at(10),
					Value: json.RawMessage(`{"score":50,"attention":["question for you"]}`),
				}},
			},
			want: model.Classification{},
		},
		{
			name: "own PR keeps its reasons next to the score",
			pr:   minePR(func(pr *model.PR) { pr.Checks = model.ChecksFailure }),
			in: Inputs{
				Viewer:      viewer,
				Priority:    Priority{Weights: DefaultWeights},
				Annotations: []model.Annotation{annotation("", UrgencyKey)},
			},
			want: model.Classification{Section: model.SectionNeedsAction, Reasons: []string{"checks failing"}, Score: DefaultWeights.Urgent},
		},
		{
			name: "own PR with failing checks scores each of them",
			pr: minePR(func(pr *model.PR) {
				pr.Checks, pr.ChecksFailing, pr.ChecksRunning = model.ChecksFailure, 2, 1
			}),
			in: inputs,
			want: model.Classification{
				Section: model.SectionNeedsAction,
				Reasons: []string{"checks failing"},
				Score:   2*DefaultWeights.PerFailingCheck + DefaultWeights.ChecksRunning,
			},
		},
		{
			name: "successful checks add to the score",
			pr:   basePR(func(pr *model.PR) { pr.Checks = model.ChecksSuccess }),
			in:   inputs,
			want: model.Classification{Section: model.SectionInvolved, Score: DefaultWeights.ChecksSuccess},
		},
		{
			name: "closed PR is not direct but still scored",
			pr: basePR(func(pr *model.PR) {
				pr.State = model.PRStateClosed
				pr.Author = "dependabot[bot]"
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
			}),
			in:   Inputs{Viewer: viewer, Priority: Priority{Greylist: []string{"dependabot"}, Weights: DefaultWeights}},
			want: model.Classification{Score: DefaultWeights.Greylist},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withoutTerms(t, Evaluate(tt.pr, tt.in)); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Evaluate = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDiffEventsCarryClassification(t *testing.T) {
	in := Inputs{
		Viewer:      viewer,
		Priority:    Priority{Teams: team(teamPoints, "alice"), Weights: DefaultWeights},
		Annotations: []model.Annotation{annotation("", UrgencyKey)},
	}
	direct := model.ChangeReviewRequest{ReviewRequest: model.ReviewRequest{Login: "viewer"}, Direct: true}
	requested := func(pr *model.PR) {
		pr.Author = "alice"
		pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
	}
	reviewed := func(pr *model.PR) {
		pr.Author = "alice"
		pr.Reviews = []model.Review{{ID: "r1", Author: "viewer", State: "CHANGES_REQUESTED", SubmittedAt: at(2), CommitSHA: "aaa"}}
	}
	base := DefaultWeights.Urgent + teamPoints + DefaultWeights.NeedsMe
	requestedCls := model.Classification{
		Section: model.SectionReviewRequested,
		Direct:  true,
		Reasons: []string{ReasonReviewRequested},
		NeedsMe: true,
		Score:   base + DefaultWeights.Direct,
	}

	tests := []struct {
		name string
		old  *model.PR
		cur  model.PR
		want []want
		cls  model.Classification
	}{
		{
			name: "discovery",
			cur:  basePR(requested),
			want: []want{{model.EventDiscovered, nil}, {model.EventReviewRequested, direct}},
			cls:  requestedCls,
		},
		{
			name: "update",
			old:  ptr(basePR(func(pr *model.PR) { pr.Author = "alice" })),
			cur:  basePR(requested),
			want: []want{
				{model.EventReviewRequested, direct},
				{model.EventSectionChanged, model.ChangeSection{From: model.SectionInvolved, To: model.SectionReviewRequested}},
				{model.EventUpdated, nil},
			},
			cls: requestedCls,
		},
		{
			name: "push after the viewer requested changes",
			old:  ptr(basePR(reviewed)),
			cur:  basePR(reviewed, func(pr *model.PR) { pr.HeadSHA = "bbb" }),
			want: []want{
				{model.EventCommitsPushed, model.ChangeCommits{Before: "aaa", After: "bbb"}},
				{model.EventUpdated, nil},
			},
			cls: model.Classification{
				Section: model.SectionInvolved,
				Reasons: []string{ReasonRereview},
				NeedsMe: true,
				Score:   base + DefaultWeights.Rereview,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := Diff(tt.old, tt.cur, in)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			checkEvents(t, events, tt.want, tt.cur, tt.cls)
		})
	}
}

func TestLastActivity(t *testing.T) {
	tests := []struct {
		name     string
		reviews  []model.Review
		comments []model.Comment
		want     time.Time
	}{
		{name: "no activity is zero"},
		{
			name:     "others' activity is ignored",
			reviews:  []model.Review{{ID: "r1", Author: "other", SubmittedAt: at(3)}},
			comments: []model.Comment{{ID: "c1", Kind: model.CommentIssue, Author: "other", CreatedAt: at(4)}},
		},
		{
			name:    "latest review",
			reviews: []model.Review{{ID: "r1", Author: "viewer", SubmittedAt: at(6)}, {ID: "r2", Author: "viewer", SubmittedAt: at(3)}},
			comments: []model.Comment{
				{ID: "c1", Kind: model.CommentIssue, Author: "viewer", CreatedAt: at(4)},
				{ID: "c2", Kind: model.CommentIssue, Author: "other", CreatedAt: at(9)},
			},
			want: at(6),
		},
		{
			name:    "latest inline comment, compared case-insensitively",
			reviews: []model.Review{{ID: "r1", Author: "viewer", SubmittedAt: at(3)}},
			comments: []model.Comment{
				{ID: "c1", Kind: model.CommentIssue, Author: "viewer", CreatedAt: at(4)},
				{ID: "c2", Kind: model.CommentInline, Author: "VIEWER", ThreadID: "t1", CreatedAt: at(5)},
			},
			want: at(5),
		},
		{
			name:     "latest review body",
			comments: []model.Comment{{ID: "c1", Kind: model.CommentReview, Author: "viewer", CreatedAt: at(7)}},
			want:     at(7),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := basePR(func(pr *model.PR) { pr.Reviews, pr.Comments = tt.reviews, tt.comments })
			if got := LastActivity(pr, viewer); !got.Equal(tt.want) {
				t.Fatalf("LastActivity = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMyThreads(t *testing.T) {
	pr := basePR(func(pr *model.PR) {
		pr.Comments = []model.Comment{
			{ID: "c1", Kind: model.CommentInline, Author: "viewer", ThreadID: "started"},
			{ID: "c2", Kind: model.CommentInline, Author: "other", ThreadID: "started"},
			{ID: "c3", Kind: model.CommentInline, Author: "other", ThreadID: "replied"},
			{ID: "c4", Kind: model.CommentInline, Author: "Viewer", ThreadID: "replied"},
			{ID: "c5", Kind: model.CommentInline, Author: "other", ThreadID: "foreign"},
			{ID: "c6", Kind: model.CommentIssue, Author: "viewer"},
		}
	})
	want := map[string]bool{"started": true, "replied": true}
	if got := MyThreads(pr, viewer); !maps.Equal(got, want) {
		t.Fatalf("MyThreads = %v, want %v", got, want)
	}
	if got := MyThreads(basePR(), viewer); len(got) != 0 {
		t.Fatalf("MyThreads without comments = %v, want none", got)
	}
}

func TestHandled(t *testing.T) {
	inline := func(id, author, thread string, min int) model.Comment {
		return model.Comment{ID: id, Kind: model.CommentInline, Author: author, ThreadID: thread, CreatedAt: at(min)}
	}
	issue := func(id, author string, min int) model.Comment {
		return model.Comment{ID: id, Kind: model.CommentIssue, Author: author, CreatedAt: at(min)}
	}
	review := func(id, author, state string, min int) model.Review {
		return model.Review{ID: id, Author: author, State: state, SubmittedAt: at(min)}
	}
	// summary is the body of review id, which the fetcher stores as a comment with the review's ID.
	summary := func(id, author string, min int) model.Comment {
		return model.Comment{ID: id, Kind: model.CommentReview, Author: author, Body: "summary", CreatedAt: at(min)}
	}
	reacted := func(c model.Comment, reactions ...string) model.Comment {
		c.ViewerReactions = reactions
		return c
	}

	// Others' comments at minute 5: an inline reply in thread t1, a top-level comment and a review summary.
	reply := inline("reply", "author", "t1", 5)
	question := issue("question", "author", 5)
	theirSummary := summary("theirs", "author", 5)

	tests := []struct {
		name     string
		c        model.Comment
		comments []model.Comment // besides c
		reviews  []model.Review
		want     bool
	}{
		{name: "untouched inline reply", c: reply},
		{name: "untouched top-level comment", c: question},
		{name: "untouched review summary", c: theirSummary},

		{name: "thumbs up", c: reacted(reply, "THUMBS_UP"), want: true},
		{name: "heart", c: reacted(question, "HEART"), want: true},
		{name: "hooray", c: reacted(theirSummary, "HOORAY"), want: true},
		{name: "rocket", c: reacted(reply, "ROCKET"), want: true},
		{name: "eyes", c: reacted(reply, "EYES")},
		{name: "thumbs down", c: reacted(reply, "THUMBS_DOWN")},
		{name: "confused and laugh", c: reacted(question, "CONFUSED", "LAUGH")},
		{name: "one acknowledging reaction among others", c: reacted(reply, "EYES", "ROCKET"), want: true},

		{name: "later reply in the same thread", c: reply, comments: []model.Comment{inline("me", "viewer", "t1", 6)}, want: true},
		{
			name: "later reply in the same thread compares logins case-insensitively", c: reply,
			comments: []model.Comment{inline("me", "Viewer", "t1", 6)}, want: true,
		},
		{name: "later reply in another thread", c: reply, comments: []model.Comment{inline("me", "viewer", "t2", 6)}},
		{name: "earlier reply in the same thread", c: reply, comments: []model.Comment{inline("me", "viewer", "t1", 4)}},
		{name: "later reply by someone else", c: reply, comments: []model.Comment{inline("me", "other", "t1", 6)}},
		{name: "later top-level comment for an inline reply", c: reply, comments: []model.Comment{issue("me", "viewer", 6)}},
		{
			name: "inline comments without a thread do not share one", c: inline("loose", "author", "", 5),
			comments: []model.Comment{inline("me", "viewer", "", 6)},
		},

		{name: "later top-level comment", c: question, comments: []model.Comment{issue("me", "viewer", 6)}, want: true},
		{name: "later top-level comment on a review summary", c: theirSummary, comments: []model.Comment{issue("me", "viewer", 6)}, want: true},
		{name: "earlier top-level comment", c: question, comments: []model.Comment{issue("me", "viewer", 4)}},
		{name: "top-level comment at the same time", c: question, comments: []model.Comment{issue("me", "viewer", 5)}},
		{name: "later top-level comment by someone else", c: question, comments: []model.Comment{issue("me", "other", 6)}},
		{name: "later inline comment for a top-level comment", c: question, comments: []model.Comment{inline("me", "viewer", "t1", 6)}},

		{name: "later approval of an inline reply", c: reply, reviews: []model.Review{review("r", "viewer", "APPROVED", 6)}, want: true},
		{name: "later change request", c: question, reviews: []model.Review{review("r", "viewer", "CHANGES_REQUESTED", 6)}, want: true},
		{name: "later dismissed review", c: theirSummary, reviews: []model.Review{review("r", "viewer", "DISMISSED", 6)}, want: true},
		{
			name: "later review with a summary", c: reply,
			reviews: []model.Review{review("r", "viewer", "COMMENTED", 6)}, comments: []model.Comment{summary("r", "viewer", 6)},
			want: true,
		},
		{name: "later bare thread-reply review", c: question, reviews: []model.Review{review("r", "viewer", "COMMENTED", 6)}},
		{
			name: "later bare review next to the summary of an earlier one", c: question,
			reviews:  []model.Review{review("r1", "viewer", "COMMENTED", 4), review("r2", "viewer", "COMMENTED", 6)},
			comments: []model.Comment{summary("r1", "viewer", 4)},
		},
		{name: "earlier approval", c: reply, reviews: []model.Review{review("r", "viewer", "APPROVED", 4)}},
		{name: "later approval by someone else", c: reply, reviews: []model.Review{review("r", "other", "APPROVED", 6)}},

		{name: "own inline comment", c: inline("mine", "viewer", "t1", 5), want: true},
		{name: "own top-level comment compares logins case-insensitively", c: issue("mine", "VIEWER", 5), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := basePR(func(pr *model.PR) {
				pr.Comments = append([]model.Comment{tt.c}, tt.comments...)
				pr.Reviews = tt.reviews
			})
			if got := Handled(pr, viewer, tt.c); got != tt.want {
				t.Fatalf("Handled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMentions(t *testing.T) {
	tests := []struct {
		login, body string
		want        bool
	}{
		{"viewer", "@viewer", true},
		{"viewer", "@viewer2 and later @viewer", true},
		{"viewer", "ä@viewer", true},
		{"", "@ hello", false},
		{"viewer", "@Viewer please have a look", true},
		{"viewer", "cc @VIEWER.", true},
		{"viewer", "(@viewer)", true},
		{"viewer", "@other, @viewer: thoughts?", true},
		{"viewer", "first line\n@viewer second line", true},
		{"jan", "@jan", true},
		{"jan", "@janroehrich", false},
		{"jan", "@jan-roehrich", false},
		{"jan", "@jan_r", false},
		{"jan", "@jan2", false},
		{"viewer", "mail me@viewer.dev", false},
		{"viewer", "viewer", false},
		{"viewer", "@ viewer", false},
		{"viewer", "", false},
	}
	for _, tt := range tests {
		if got := Mentions(tt.body, tt.login); got != tt.want {
			t.Errorf("mention of %q in %q = %v, want %v", tt.login, tt.body, got, tt.want)
		}
	}
}

func TestAttention(t *testing.T) {
	w := DefaultWeights
	review := func(state string, min int, sha string) model.Review {
		return model.Review{ID: "r" + state, Author: "viewer", State: state, SubmittedAt: at(min), CommitSHA: sha}
	}
	inline := func(id, author, thread string, min int, sha string) model.Comment {
		return model.Comment{ID: id, Kind: model.CommentInline, Author: author, ThreadID: thread, CommitSHA: sha, CreatedAt: at(min)}
	}
	issue := func(id, author, body string, min int) model.Comment {
		return model.Comment{ID: id, Kind: model.CommentIssue, Author: author, Body: body, CreatedAt: at(min)}
	}
	note := func(commentID, value string, min int) model.Annotation {
		return model.Annotation{
			PR: basePR().Key, CommentID: commentID, Key: "jev.attention", Value: json.RawMessage(value), UpdatedAt: at(min),
		}
	}
	quiet := func(commentID string) model.Annotation {
		return note(commentID, `{"intent":"ack","attention":false}`, 10)
	}
	reacted := func(c model.Comment, reactions ...string) model.Comment {
		c.ViewerReactions = reactions
		return c
	}

	// The viewer reviewed commit aaa at minute 2 with an inline finding that starts thread t1; the
	// author answers in t1 at minute 5. Cases with head "bbb" saw a push since.
	commented := []model.Review{review("COMMENTED", 2, "aaa")}
	finding := inline("finding", "viewer", "t1", 2, "aaa")
	answer := inline("answer", "author", "t1", 5, "")
	mention := issue("mention", "author", "@viewer what do you think?", 6)

	tests := []struct {
		name        string
		head        string // HeadSHA if set, otherwise "aaa"
		requests    []model.ReviewRequest
		reviews     []model.Review
		comments    []model.Comment
		threads     []model.ReviewThread
		annotations []model.Annotation
		mut         func(*model.PR)
		reasons     []string
		score       int
	}{
		{
			name: "no activity needs nothing",
		},
		{
			name:     "direct review request",
			requests: []model.ReviewRequest{{Login: "viewer"}},
			reasons:  []string{ReasonReviewRequested},
			score:    w.Direct + w.NeedsMe,
		},
		{
			name:     "team-only review request gives no reason without Now",
			requests: []model.ReviewRequest{{Team: "acme-co/core"}},
		},

		{
			name:    "changes requested and pushed",
			head:    "bbb",
			reviews: []model.Review{review("CHANGES_REQUESTED", 2, "aaa")},
			reasons: []string{ReasonRereview},
			score:   w.Rereview + w.NeedsMe,
		},
		{
			name:    "dismissed and pushed",
			head:    "bbb",
			reviews: []model.Review{review("DISMISSED", 2, "aaa")},
			reasons: []string{ReasonRereview},
			score:   w.Rereview + w.NeedsMe,
		},
		{
			name:    "changes requested without a push",
			reviews: []model.Review{review("CHANGES_REQUESTED", 2, "aaa")},
		},
		{
			name:    "approved and pushed",
			head:    "bbb",
			reviews: []model.Review{review("APPROVED", 2, "aaa")},
		},
		{
			name:    "only the latest review's state counts",
			head:    "bbb",
			reviews: []model.Review{review("CHANGES_REQUESTED", 2, "aaa"), review("APPROVED", 3, "aaa")},
		},
		{
			name:    "unknown review commit disables re-review",
			head:    "bbb",
			reviews: []model.Review{review("CHANGES_REQUESTED", 2, "")},
		},
		{
			name:     "a later inline comment on the head is my head",
			head:     "bbb",
			reviews:  []model.Review{review("CHANGES_REQUESTED", 2, "aaa")},
			comments: []model.Comment{inline("c1", "viewer", "t1", 5, "bbb")},
		},
		{
			name:     "a later inline comment with an unknown commit disables re-review",
			head:     "bbb",
			reviews:  []model.Review{review("CHANGES_REQUESTED", 2, "aaa")},
			comments: []model.Comment{inline("c1", "viewer", "t1", 5, "")},
		},
		{
			name:     "a later review is my head",
			head:     "bbb",
			reviews:  []model.Review{review("CHANGES_REQUESTED", 5, "aaa")},
			comments: []model.Comment{inline("c1", "viewer", "t1", 2, "bbb")},
			reasons:  []string{ReasonRereview},
			score:    w.Rereview + w.NeedsMe,
		},
		{
			name:     "requested changes survive my later top-level comment",
			head:     "bbb",
			reviews:  []model.Review{review("CHANGES_REQUESTED", 2, "aaa")},
			comments: []model.Comment{issue("c1", "viewer", "looks good now", 7)},
			reasons:  []string{ReasonRereview},
			score:    w.Rereview + w.NeedsMe,
		},
		{
			name:     "reply in my thread and pushed",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding, answer},
			reasons:  []string{ReasonRereview},
			score:    w.Rereview + w.NeedsMe,
		},
		{
			name:    "reply in a thread I replied in and pushed",
			head:    "bbb",
			reviews: commented,
			comments: []model.Comment{
				inline("theirs", "author", "t1", 1, "aaa"),
				finding,
				answer,
			},
			reasons: []string{ReasonRereview},
			score:   w.Rereview + w.NeedsMe,
		},
		{
			name:     "my reply on the old commit after theirs clears re-review",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding, answer, inline("ok", "viewer", "t1", 7, "aaa")},
		},
		{
			name:     "my top-level comment after their reply clears re-review but leaves the reply open",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding, answer, issue("ok", "viewer", "looks good now", 7)},
			reasons:  []string{ReasonThreadReply},
			score:    w.ThreadReply + w.NeedsMe,
		},
		{
			name:     "my formal review after their reply clears re-review and the reply",
			head:     "bbb",
			reviews:  []model.Review{commented[0], review("APPROVED", 7, "aaa")},
			comments: []model.Comment{finding, answer},
		},
		{
			name:     "bot reply in my thread is ignored",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding, inline("bot", "coderabbitai[bot]", "t1", 5, "")},
		},
		{
			name:     "reply in someone else's thread is ignored",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding, inline("other", "author", "t2", 5, "")},
		},
		{
			name:     "my thread resolved by someone else and pushed",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding},
			threads:  []model.ReviewThread{{ID: "t1", IsResolved: true, ResolvedBy: "author"}},
			reasons:  []string{ReasonRereview},
			score:    w.Rereview + w.NeedsMe,
		},
		{
			name:     "my thread resolved by someone else counts despite my later activity",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding, issue("c1", "viewer", "one more thing", 7)},
			threads:  []model.ReviewThread{{ID: "t1", IsResolved: true, ResolvedBy: "author"}},
			reasons:  []string{ReasonRereview},
			score:    w.Rereview + w.NeedsMe,
		},
		{
			name:     "my thread resolved by me",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding},
			threads:  []model.ReviewThread{{ID: "t1", IsResolved: true, ResolvedBy: "Viewer"}},
		},
		{
			name:     "my thread resolved by someone else without a push",
			reviews:  commented,
			comments: []model.Comment{finding},
			threads:  []model.ReviewThread{{ID: "t1", IsResolved: true, ResolvedBy: "author"}},
		},
		{
			name:     "someone else's thread resolved",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding},
			threads:  []model.ReviewThread{{ID: "t2", IsResolved: true, ResolvedBy: "author"}},
		},
		{
			name:     "my unresolved thread",
			head:     "bbb",
			reviews:  commented,
			comments: []model.Comment{finding},
			threads:  []model.ReviewThread{{ID: "t1"}},
		},

		{
			name:     "reply in my thread",
			reviews:  commented,
			comments: []model.Comment{finding, answer},
			reasons:  []string{ReasonThreadReply},
			score:    w.ThreadReply + w.NeedsMe,
		},
		{
			name:     "reply in my thread with unknown commits",
			head:     "bbb",
			reviews:  []model.Review{review("COMMENTED", 2, "")},
			comments: []model.Comment{inline("finding", "viewer", "t1", 2, ""), answer},
			reasons:  []string{ReasonThreadReply},
			score:    w.ThreadReply + w.NeedsMe,
		},
		{
			name:     "reply answered by my later reply in the thread",
			reviews:  commented,
			comments: []model.Comment{finding, answer, inline("ok", "viewer", "t1", 7, "aaa")},
		},
		{
			name:     "reply not answered by my later top-level comment",
			reviews:  commented,
			comments: []model.Comment{finding, answer, issue("ok", "viewer", "thanks", 7)},
			reasons:  []string{ReasonThreadReply},
			score:    w.ThreadReply + w.NeedsMe,
		},
		{
			name:     "pushback acknowledged by a thumbs up",
			reviews:  commented,
			comments: []model.Comment{finding, reacted(inline("pushback", "author", "t1", 5, ""), "THUMBS_UP")},
		},
		{
			name:     "reply with a non-acknowledging reaction",
			reviews:  commented,
			comments: []model.Comment{finding, reacted(answer, "EYES")},
			reasons:  []string{ReasonThreadReply},
			score:    w.ThreadReply + w.NeedsMe,
		},
		{
			name:     "acknowledging one reply leaves another open",
			reviews:  commented,
			comments: []model.Comment{finding, reacted(answer, "HEART"), inline("more", "author", "t1", 6, "")},
			reasons:  []string{ReasonThreadReply},
			score:    w.ThreadReply + w.NeedsMe,
		},
		{
			// GitHub records my bare reply in t2 as a COMMENTED review without body.
			name:    "my reply in another thread leaves the reply open",
			reviews: []model.Review{commented[0], {ID: "bare", Author: "viewer", State: "COMMENTED", SubmittedAt: at(7)}},
			comments: []model.Comment{
				finding, answer,
				inline("finding2", "viewer", "t2", 2, "aaa"),
				inline("answer2", "author", "t2", 6, ""),
				inline("ok", "viewer", "t2", 7, "aaa"),
			},
			reasons: []string{ReasonThreadReply},
			score:   w.ThreadReply + w.NeedsMe,
		},
		{
			name:    "my review with a summary answers every reply",
			reviews: []model.Review{commented[0], {ID: "summary", Author: "viewer", State: "COMMENTED", SubmittedAt: at(7)}},
			comments: []model.Comment{
				finding, answer,
				{ID: "summary", Kind: model.CommentReview, Author: "viewer", Body: "all good", CreatedAt: at(7)},
			},
		},
		{
			name:     "bot reply",
			reviews:  commented,
			comments: []model.Comment{finding, inline("bot", "Copilot[BOT]", "t1", 5, "")},
		},
		{
			name:        "reply suppressed by a comment annotation",
			reviews:     commented,
			comments:    []model.Comment{finding, answer},
			annotations: []model.Annotation{quiet("answer")},
		},
		{
			name:        "only attention false suppresses",
			reviews:     commented,
			comments:    []model.Comment{finding, answer},
			annotations: []model.Annotation{note("answer", `{"intent":"question_for_me","attention":true}`, 10)},
			reasons:     []string{ReasonThreadReply},
			score:       w.ThreadReply + w.NeedsMe,
		},
		{
			name:        "suppressed reply still counts for re-review",
			head:        "bbb",
			reviews:     commented,
			comments:    []model.Comment{finding, answer},
			annotations: []model.Annotation{quiet("answer")},
			reasons:     []string{ReasonRereview},
			score:       w.Rereview + w.NeedsMe,
		},

		{
			name:     "mention",
			comments: []model.Comment{mention},
			reasons:  []string{ReasonMentioned},
			score:    w.Mentioned + w.NeedsMe,
		},
		{
			name: "mention in an inline comment",
			comments: []model.Comment{{
				ID: "c1", Kind: model.CommentInline, Author: "author", ThreadID: "t9", Body: "cc @Viewer", CreatedAt: at(5),
			}},
			reasons: []string{ReasonMentioned},
			score:   w.Mentioned + w.NeedsMe,
		},
		{
			name:     "mention answered by my later top-level comment",
			comments: []model.Comment{mention, issue("ok", "viewer", "on it", 7)},
		},
		{
			name:     "mention not answered by my earlier top-level comment",
			comments: []model.Comment{issue("ok", "viewer", "on it", 4), mention},
			reasons:  []string{ReasonMentioned},
			score:    w.Mentioned + w.NeedsMe,
		},
		{
			name:     "mention acknowledged by a reaction",
			comments: []model.Comment{reacted(mention, "HOORAY")},
		},
		{
			name:     "mention with a thumbs down",
			comments: []model.Comment{reacted(mention, "THUMBS_DOWN")},
			reasons:  []string{ReasonMentioned},
			score:    w.Mentioned + w.NeedsMe,
		},
		{
			name:    "mention answered by my later review with a summary",
			reviews: []model.Review{{ID: "summary", Author: "viewer", State: "COMMENTED", SubmittedAt: at(7)}},
			comments: []model.Comment{
				mention,
				{ID: "summary", Kind: model.CommentReview, Author: "viewer", Body: "replied inline", CreatedAt: at(7)},
			},
		},
		{
			name:     "mention not answered by my bare thread reply",
			reviews:  []model.Review{{ID: "bare", Author: "viewer", State: "COMMENTED", SubmittedAt: at(7)}},
			comments: []model.Comment{mention, inline("ok", "viewer", "t2", 7, "aaa")},
			reasons:  []string{ReasonMentioned},
			score:    w.Mentioned + w.NeedsMe,
		},
		{
			name: "inline mention answered in its thread",
			comments: []model.Comment{
				{ID: "c1", Kind: model.CommentInline, Author: "author", ThreadID: "t9", Body: "cc @Viewer", CreatedAt: at(5)},
				inline("ok", "viewer", "t9", 7, "aaa"),
			},
		},
		{
			name: "inline mention not answered by my later top-level comment",
			comments: []model.Comment{
				{ID: "c1", Kind: model.CommentInline, Author: "author", ThreadID: "t9", Body: "cc @Viewer", CreatedAt: at(5)},
				issue("ok", "viewer", "on it", 7),
			},
			reasons: []string{ReasonMentioned},
			score:   w.Mentioned + w.NeedsMe,
		},
		{
			name:     "my own mention",
			comments: []model.Comment{issue("c1", "viewer", "note to @viewer", 5)},
		},
		{
			name:     "mention of a longer login",
			comments: []model.Comment{issue("c1", "author", "@viewer2 and @viewer-bot", 5)},
		},
		{
			name:        "mention suppressed by a comment annotation",
			comments:    []model.Comment{mention},
			annotations: []model.Annotation{quiet("mention")},
		},
		{
			name:     "mention by a bot",
			comments: []model.Comment{issue("c1", "release[bot]", "@viewer please approve the release", 5)},
			reasons:  []string{ReasonMentioned},
			score:    w.Mentioned + w.NeedsMe,
		},

		{
			name:     "core reasons in order, re-review replacing thread reply",
			head:     "bbb",
			requests: []model.ReviewRequest{{Login: "viewer"}},
			reviews:  []model.Review{review("CHANGES_REQUESTED", 2, "aaa")},
			comments: []model.Comment{finding, answer, mention},
			reasons:  []string{ReasonReviewRequested, ReasonRereview, ReasonMentioned},
			score:    w.Direct + w.Rereview + w.Mentioned + w.NeedsMe,
		},
		{
			name:     "thread reply and mention",
			reviews:  commented,
			comments: []model.Comment{finding, answer, mention},
			reasons:  []string{ReasonThreadReply, ReasonMentioned},
			score:    w.ThreadReply + w.Mentioned + w.NeedsMe,
		},
		{
			name:     "needs me with a negative total",
			comments: []model.Comment{mention},
			mut: func(pr *model.PR) {
				pr.Mergeable = "CONFLICTING"
				pr.Checks, pr.ChecksFailing = model.ChecksFailure, 3
			},
			reasons: []string{ReasonMentioned},
			score:   w.MergeConflict + 3*w.PerFailingCheck + w.Mentioned + w.NeedsMe,
		},

		{
			name:        "fresh annotation adds score and reasons",
			reviews:     commented,
			annotations: []model.Annotation{note("", `{"score":25,"attention":["question for you"]}`, 3)},
			reasons:     []string{"question for you"},
			score:       25 + w.NeedsMe,
		},
		{
			name:        "stale annotation contributes nothing",
			reviews:     commented,
			annotations: []model.Annotation{note("", `{"score":25,"attention":["question for you"]}`, 1)},
		},
		{
			name:        "annotation from the time of my last activity is stale",
			reviews:     commented,
			annotations: []model.Annotation{note("", `{"score":25,"attention":["question for you"]}`, 2)},
		},
		{
			name:        "my top-level comment retires an annotation",
			comments:    []model.Comment{issue("ok", "viewer", "answered", 7)},
			annotations: []model.Annotation{note("", `{"score":25,"attention":["question for you"]}`, 6)},
		},
		{
			name:        "without activity every annotation is fresh",
			annotations: []model.Annotation{note("", `{"score":25,"attention":["question for you"]}`, 0)},
			reasons:     []string{"question for you"},
			score:       25 + w.NeedsMe,
		},
		{
			name:        "score alone does not need me",
			annotations: []model.Annotation{note("", `{"score":10}`, 3)},
			score:       10,
		},
		{
			name:        "score is clamped to 100",
			annotations: []model.Annotation{note("", `{"score":1000}`, 3)},
			score:       100,
		},
		{
			name:        "score is clamped to -100 and the PR still needs me",
			annotations: []model.Annotation{note("", `{"score":-1000,"attention":["pushback on your comment"]}`, 3)},
			reasons:     []string{"pushback on your comment"},
			score:       -100 + w.NeedsMe,
		},
		{
			name: "each annotation is clamped on its own",
			annotations: []model.Annotation{
				note("", `{"score":150}`, 3),
				note("", `{"score":-30}`, 3),
			},
			score: 70,
		},
		{
			name: "score must be an integer",
			annotations: []model.Annotation{
				note("", `{"score":12.5,"attention":["question for you"]}`, 3),
				note("", `{"score":"12"}`, 3),
			},
			reasons: []string{"question for you"},
			score:   w.NeedsMe,
		},
		{
			name: "at most five reasons per annotation, each truncated to 60 characters",
			annotations: []model.Annotation{note("", `{"attention":["a","b","c","d","`+
				strings.Repeat("x", 70)+`","f"]}`, 3)},
			reasons: []string{"a", "b", "c", "d", strings.Repeat("x", 60)},
			score:   w.NeedsMe,
		},
		{
			name:        "truncation counts characters, not bytes",
			annotations: []model.Annotation{note("", `{"attention":["`+strings.Repeat("ä", 61)+`"]}`, 3)},
			reasons:     []string{strings.Repeat("ä", 60)},
			score:       w.NeedsMe,
		},
		{
			name:        "blank reasons are skipped",
			annotations: []model.Annotation{note("", `{"attention":[""," ","  spaced  "]}`, 3)},
			reasons:     []string{"spaced"},
			score:       w.NeedsMe,
		},
		{
			name:     "reasons are deduplicated against core reasons and across annotations",
			comments: []model.Comment{mention},
			annotations: []model.Annotation{
				note("", `{"attention":["mentioned you","pushback","pushback"]}`, 10),
				note("", `{"attention":["pushback","question for you"]}`, 10),
			},
			reasons: []string{ReasonMentioned, "pushback", "question for you"},
			score:   w.Mentioned + w.NeedsMe,
		},
		{
			name: "malformed attention lists are ignored",
			annotations: []model.Annotation{
				note("", `{"attention":["question for you",5]}`, 3),
				note("", `{"attention":"question for you"}`, 3),
			},
		},
		{
			name: "values that are no object contribute nothing",
			annotations: []model.Annotation{
				note("", `true`, 3),
				note("", `"question for you"`, 3),
				note("", `["question for you"]`, 3),
			},
		},
		{
			name:        "comment-level annotation adds neither score nor reasons",
			annotations: []model.Annotation{note("c9", `{"score":50,"attention":["question for you"]}`, 3)},
		},
		{
			name:        "PR-level attention false suppresses nothing",
			comments:    []model.Comment{mention},
			annotations: []model.Annotation{note("", `{"attention":false}`, 10)},
			reasons:     []string{ReasonMentioned},
			score:       w.Mentioned + w.NeedsMe,
		},
		{
			name:    "urgency keeps its fixed weight, also when stale",
			reviews: commented,
			annotations: []model.Annotation{{
				PR: basePR().Key, Key: UrgencyKey, UpdatedAt: at(1),
				Value: json.RawMessage(`{"score":50,"attention":["urgent"]}`),
			}},
			score: w.Urgent,
		},
		{
			name:    "fresh urgency adds no contribution of its own",
			reviews: commented,
			annotations: []model.Annotation{{
				PR: basePR().Key, Key: UrgencyKey, UpdatedAt: at(10),
				Value: json.RawMessage(`{"score":50,"attention":["urgent"]}`),
			}},
			score: w.Urgent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := basePR(func(pr *model.PR) {
				if tt.head != "" {
					pr.HeadSHA = tt.head
				}
				pr.ReviewRequests, pr.Reviews, pr.Comments, pr.ReviewThreads = tt.requests, tt.reviews, tt.comments, tt.threads
			})
			if tt.mut != nil {
				tt.mut(&pr)
			}
			got := Evaluate(pr, Inputs{Viewer: viewer, Priority: Priority{Weights: w}, Annotations: tt.annotations})
			if !slices.Equal(got.Reasons, tt.reasons) {
				t.Errorf("reasons = %q, want %q", got.Reasons, tt.reasons)
			}
			if wantNeedsMe := len(tt.reasons) > 0; got.NeedsMe != wantNeedsMe {
				t.Errorf("needsMe = %v, want %v", got.NeedsMe, wantNeedsMe)
			}
			if got.Score != tt.score {
				t.Errorf("score = %d, want %d", got.Score, tt.score)
			}
		})
	}
}

func TestTeamReview(t *testing.T) {
	now := t0.Add(30 * 24 * time.Hour)
	w := DefaultWeights
	team := []model.ReviewRequest{{Team: "acme-co/core"}}
	review := func(author, state string) model.Review {
		return model.Review{ID: author + state, Author: author, State: state, SubmittedAt: now.Add(-time.Hour)}
	}
	// ready is a foreign PR that waits for a reviewer from the viewer's team and passes every condition.
	ready := func(mut ...func(*model.PR)) model.PR {
		return basePR(append([]func(*model.PR){func(pr *model.PR) {
			pr.ReviewRequests = team
			pr.UpdatedAt = now.Add(-time.Hour)
		}}, mut...)...)
	}
	wait := []string{ReasonTeamReviewRequested}
	waitScore := w.TeamReview + w.NeedsMe

	tests := []struct {
		name      string
		pr        model.PR
		greylist  []string
		roles     []string
		weights   *Weights
		zeroNow   bool
		reasons   []string
		wantScore int
	}{
		{name: "waiting for a reviewer", pr: ready(), reasons: wait, wantScore: waitScore},
		{
			name: "team names match case-insensitively",
			pr: ready(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Team: "ACME-co/Core"}}
			}),
			reasons: wait, wantScore: waitScore,
		},
		{
			name: "request for another team",
			pr: ready(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/docs"}}
			}),
		},
		{
			name: "request for someone else",
			pr: ready(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Login: "carol"}}
			}),
		},
		{name: "no request", pr: ready(func(pr *model.PR) { pr.ReviewRequests = nil })},
		{
			name: "one of several requests is my team",
			pr: ready(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Login: "carol"}, {Team: "acme-co/docs"}, {Team: "acme-co/core"}}
			}),
			reasons: wait, wantScore: waitScore,
		},
		{
			name: "direct request wins and earns no team weight",
			pr: ready(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/core"}, {Login: "viewer"}}
			}),
			reasons: []string{ReasonReviewRequested}, wantScore: w.Direct + w.NeedsMe,
		},
		{
			name:    "request for a role",
			pr:      ready(),
			roles:   []string{"ACME-co/Core"},
			reasons: []string{ReasonRoleReviewRequested}, wantScore: w.RoleReview + w.NeedsMe,
		},
		{
			name:    "a role does not make other teams roles",
			pr:      ready(),
			roles:   []string{"acme-co/staff"},
			reasons: wait, wantScore: waitScore,
		},
		{
			name: "team request wins over a role request",
			pr: ready(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/staff"}, {Team: "acme-co/core"}}
			}),
			roles:   []string{"acme-co/staff"},
			reasons: wait, wantScore: waitScore,
		},
		{
			name:  "a role I do not have",
			pr:    ready(func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/staff"}} }),
			roles: []string{"acme-co/staff"},
		},
		{
			name: "direct request wins over a role request",
			pr: ready(func(pr *model.PR) {
				pr.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/core"}, {Login: "viewer"}}
			}),
			roles:   []string{"acme-co/core"},
			reasons: []string{ReasonReviewRequested}, wantScore: w.Direct + w.NeedsMe,
		},
		{
			name:  "a review ends the wait for a role",
			pr:    ready(func(pr *model.PR) { pr.Reviews = []model.Review{review("carol", "COMMENTED")} }),
			roles: []string{"acme-co/core"},
		},
		{
			name:      "the role weight is configurable",
			pr:        ready(),
			roles:     []string{"acme-co/core"},
			weights:   &Weights{TeamReview: 7, RoleReview: 2, NeedsMe: 11},
			reasons:   []string{ReasonRoleReviewRequested},
			wantScore: 2 + 11,
		},
		{name: "own PR", pr: ready(func(pr *model.PR) { pr.Author = "viewer" })},
		{name: "merged PR", pr: ready(func(pr *model.PR) { pr.State = model.PRStateMerged })},

		{name: "draft", pr: ready(func(pr *model.PR) { pr.IsDraft = true })},
		{name: "bot author", pr: ready(func(pr *model.PR) { pr.Author = "dependabot[bot]" })},
		{name: "greylisted author", pr: ready(func(pr *model.PR) { pr.Author = "Renovate" }), greylist: []string{"renovate"}},
		{
			name:     "greylist matches the author with or without the bot suffix",
			pr:       ready(func(pr *model.PR) { pr.Author = "mend" }),
			greylist: []string{"mend[bot]"},
		},
		{name: "author is not greylisted", pr: ready(), greylist: []string{"renovate"}, reasons: wait, wantScore: waitScore},

		{
			name:    "updated exactly at the age limit",
			pr:      ready(func(pr *model.PR) { pr.UpdatedAt = now.Add(-TeamReviewMaxAge) }),
			reasons: wait, wantScore: waitScore,
		},
		{name: "updated just past the age limit", pr: ready(func(pr *model.PR) { pr.UpdatedAt = now.Add(-TeamReviewMaxAge - time.Nanosecond) })},
		{name: "never updated", pr: ready(func(pr *model.PR) { pr.UpdatedAt = time.Time{} })},
		{
			name:    "updated after Now",
			pr:      ready(func(pr *model.PR) { pr.UpdatedAt = now.Add(time.Minute) }),
			reasons: wait, wantScore: waitScore,
		},
		{name: "zero Now disables the signal", pr: ready(), zeroNow: true},

		{
			name: "review by another human",
			pr: ready(func(pr *model.PR) {
				pr.Reviews = []model.Review{review("carol", "COMMENTED")}
			}),
		},
		{
			name: "approval by another human",
			pr: ready(func(pr *model.PR) {
				pr.Reviews = []model.Review{review("carol", "APPROVED")}
			}),
		},
		{
			name: "changes requested by another human",
			pr: ready(func(pr *model.PR) {
				pr.Reviews = []model.Review{review("carol", "CHANGES_REQUESTED")}
			}),
		},
		{
			name: "my own review ends the wait",
			pr: ready(func(pr *model.PR) {
				pr.Reviews = []model.Review{review("viewer", "COMMENTED")}
			}),
		},
		{
			name: "the author's thread replies are not a review",
			pr: ready(func(pr *model.PR) {
				pr.Reviews = []model.Review{review("Someone", "COMMENTED"), review("someone", "COMMENTED")}
			}),
			reasons: wait, wantScore: waitScore,
		},
		{
			name: "bot reviews do not count",
			pr: ready(func(pr *model.PR) {
				pr.Reviews = []model.Review{review("cursor[bot]", "COMMENTED"), review("copilot-pull-request-reviewer[bot]", "APPROVED")}
			}),
			reasons: wait, wantScore: waitScore,
		},
		{
			name: "greylisted reviewers do not count",
			pr: ready(func(pr *model.PR) {
				pr.Reviews = []model.Review{review("Renovate", "APPROVED"), review("mend", "COMMENTED")}
			}),
			greylist: []string{"renovate", "mend[bot]"},
			reasons:  wait, wantScore: waitScore,
		},
		{
			name: "a human review among ignored ones ends the wait",
			pr: ready(func(pr *model.PR) {
				pr.Reviews = []model.Review{review("cursor[bot]", "COMMENTED"), review("someone", "COMMENTED"), review("carol", "COMMENTED")}
			}),
		},

		{name: "failing checks", pr: ready(func(pr *model.PR) { pr.Checks, pr.ChecksFailing = model.ChecksFailure, 1 })},
		{
			name:    "pending checks are fine",
			pr:      ready(func(pr *model.PR) { pr.Checks, pr.ChecksRunning = model.ChecksPending, 1 }),
			reasons: wait, wantScore: waitScore + w.ChecksRunning,
		},
		{
			name:    "passing checks are fine",
			pr:      ready(func(pr *model.PR) { pr.Checks = model.ChecksSuccess }),
			reasons: wait, wantScore: waitScore + w.ChecksSuccess,
		},
		{name: "merge conflict", pr: ready(func(pr *model.PR) { pr.Mergeable = "CONFLICTING" })},
		{
			name:    "unknown mergeability is fine",
			pr:      ready(func(pr *model.PR) { pr.Mergeable = "UNKNOWN" }),
			reasons: wait, wantScore: waitScore,
		},

		{
			name: "adds to other signals",
			pr: ready(func(pr *model.PR) {
				pr.Comments = []model.Comment{{ID: "c1", Kind: model.CommentIssue, Author: "someone", Body: "@viewer ptal", CreatedAt: now.Add(-time.Minute)}}
			}),
			reasons: []string{ReasonTeamReviewRequested, ReasonMentioned}, wantScore: w.TeamReview + w.Mentioned + w.NeedsMe,
		},
		{
			name:      "the weight is configurable",
			pr:        ready(),
			weights:   &Weights{TeamReview: 7, NeedsMe: 11, Direct: 3},
			reasons:   wait,
			wantScore: 7 + 11,
		},
		{
			name:      "weight zero keeps the reason and the needs-me bonus",
			pr:        ready(),
			weights:   &Weights{NeedsMe: 11},
			reasons:   wait,
			wantScore: 11,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Inputs{Viewer: viewer, Priority: Priority{Greylist: tt.greylist, Roles: tt.roles, Weights: w}, Now: now}
			if tt.weights != nil {
				in.Priority.Weights = *tt.weights
			}
			if tt.zeroNow {
				in.Now = time.Time{}
			}
			got := Evaluate(tt.pr, in)
			if !slices.Equal(got.Reasons, tt.reasons) {
				t.Errorf("reasons = %q, want %q", got.Reasons, tt.reasons)
			}
			if wantNeedsMe := len(tt.reasons) > 0; got.NeedsMe != wantNeedsMe {
				t.Errorf("needsMe = %v, want %v", got.NeedsMe, wantNeedsMe)
			}
			wantScore := tt.wantScore
			if len(tt.reasons) == 0 {
				wantScore = Score(tt.pr, got, in)
			}
			if got.Score != wantScore {
				t.Errorf("score = %d, want %d", got.Score, wantScore)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestHiddenAnnotation(t *testing.T) {
	pr := basePR(func(pr *model.PR) { pr.IsDraft = true })
	a := HiddenAnnotation(pr, model.Classification{Direct: true})
	if a.PR != pr.Key || a.Key != HiddenKey || a.CommentID != "" {
		t.Fatalf("annotation = %+v", a)
	}
	var m HiddenMark
	if err := json.Unmarshal(a.Value, &m); err != nil {
		t.Fatal(err)
	}
	if want := (HiddenMark{Seen: pr.FetchedAt, Head: "aaa", Draft: true, Requested: true}); m != want {
		t.Fatalf("mark = %+v, want %+v", m, want)
	}
}

func TestHidden(t *testing.T) {
	comment := func(author string, minutes int) func(*model.PR) {
		return func(pr *model.PR) {
			pr.Comments = append(pr.Comments, model.Comment{ID: "c", Kind: model.CommentIssue, Author: author, CreatedAt: at(minutes)})
		}
	}
	review := func(author string, minutes int) func(*model.PR) {
		return func(pr *model.PR) {
			pr.Reviews = append(pr.Reviews, model.Review{ID: "r", Author: author, State: "COMMENTED", SubmittedAt: at(minutes)})
		}
	}
	draft := func(pr *model.PR) { pr.IsDraft = true }
	requested := func(pr *model.PR) { pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}} }
	// seen is basePR's FetchedAt, at(1).
	hide := func(pr model.PR) []model.Annotation {
		return []model.Annotation{HiddenAnnotation(pr, Classify(pr, viewer, nil))}
	}
	in := Inputs{Viewer: viewer, Priority: Priority{Greylist: []string{"acme-bot"}, Weights: DefaultWeights}}

	tests := []struct {
		name        string
		pr          model.PR // current snapshot
		annotations []model.Annotation
		want        bool
	}{
		{name: "not hidden", pr: basePR()},
		{name: "unchanged", pr: basePR(), annotations: hide(basePR()), want: true},
		{name: "own PR unchanged", pr: minePR(), annotations: hide(minePR()), want: true},
		{name: "new commits or rebase", pr: basePR(func(pr *model.PR) { pr.HeadSHA = "bbb" }), annotations: hide(basePR())},
		{name: "comment by someone else after the seen snapshot", pr: basePR(comment("alice", 2)), annotations: hide(basePR())},
		{name: "comment on own PR", pr: minePR(comment("alice", 2)), annotations: hide(minePR())},
		{name: "comment already seen", pr: basePR(comment("alice", 1)), annotations: hide(basePR()), want: true},
		{name: "own comment", pr: basePR(comment("Viewer", 2)), annotations: hide(basePR()), want: true},
		{name: "bot comment", pr: basePR(comment("github-actions[bot]", 2)), annotations: hide(basePR()), want: true},
		{name: "greylisted comment", pr: basePR(comment("acme-bot", 2)), annotations: hide(basePR()), want: true},
		{name: "review by someone else", pr: basePR(review("alice", 2)), annotations: hide(basePR())},
		{name: "review already seen", pr: basePR(review("alice", 0)), annotations: hide(basePR()), want: true},
		{name: "own review", pr: basePR(review("viewer", 2)), annotations: hide(basePR()), want: true},
		{name: "marked ready for review", pr: basePR(), annotations: hide(basePR(draft))},
		{name: "converted to draft", pr: basePR(draft), annotations: hide(basePR()), want: true},
		{name: "review newly requested from the viewer", pr: basePR(requested), annotations: hide(basePR())},
		{name: "review request seen", pr: basePR(requested), annotations: hide(basePR(requested)), want: true},
		{name: "request removed", pr: basePR(), annotations: hide(basePR(requested)), want: true},
		{
			name: "checks, conflicts and updates are not new",
			pr: basePR(func(pr *model.PR) {
				pr.Checks, pr.ChecksFailing, pr.Mergeable, pr.UpdatedAt = model.ChecksFailure, 2, "CONFLICTING", at(30)
			}),
			annotations: hide(basePR()),
			want:        true,
		},
		{
			name: "comment-level annotation does not hide",
			pr:   basePR(),
			annotations: []model.Annotation{func() model.Annotation {
				a := HiddenAnnotation(basePR(), model.Classification{})
				a.CommentID = "c"
				return a
			}()},
		},
		{name: "value that is no mark", pr: basePR(), annotations: []model.Annotation{annotation("", HiddenKey)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := in
			in.Annotations = tt.annotations
			if got := Evaluate(tt.pr, in).Hidden; got != tt.want {
				t.Fatalf("Hidden = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHiddenAttentionUnhides(t *testing.T) {
	// The PR was fetched at(1) and hidden at(5).
	hide := HiddenAnnotation(basePR(), model.Classification{})
	hide.UpdatedAt = at(5)
	ann := func(commentID, key, value string, minutes int) model.Annotation {
		return model.Annotation{PR: basePR().Key, CommentID: commentID, Key: key, Value: json.RawMessage(value), UpdatedAt: at(minutes)}
	}
	const attn = `{"attention":["tagged in #eng"]}`
	tests := []struct {
		name string
		a    model.Annotation
		want bool // hidden
	}{
		{name: "attention newer than the hide", a: ann("", "slack.mentions", attn, 6)},
		{name: "attention with score newer than the hide", a: ann("", "jev.attention", `{"score":5,"attention":["question"]}`, 6)},
		{name: "attention older than the hide but newer than the fetch", a: ann("", "slack.mentions", attn, 4), want: true},
		{name: "attention as old as the hide", a: ann("", "slack.mentions", attn, 5), want: true},
		{name: "newer annotation without attention", a: ann("", "slack.mentions", `{"badge":"x"}`, 6), want: true},
		{name: "newer annotation with empty attention", a: ann("", "slack.mentions", `{"attention":[]}`, 6), want: true},
		{name: "newer annotation with only a score", a: ann("", "jev.attention", `{"score":50}`, 6), want: true},
		{name: "newer annotation that is no object", a: ann("", "slack.mentions", `["tagged"]`, 6), want: true},
		{name: "newer comment-level attention", a: ann("c", "slack.mentions", attn, 6), want: true},
		{name: "newer urgency with attention", a: ann("", UrgencyKey, attn, 6), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Inputs{Viewer: viewer, Priority: Priority{Weights: DefaultWeights}, Annotations: []model.Annotation{hide, tt.a}}
			if got := Evaluate(basePR(), in).Hidden; got != tt.want {
				t.Fatalf("Hidden = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTeamBoost(t *testing.T) {
	teams := []TeamBoost{
		{Team: "acme-co/eng", Points: 10, Members: []string{"alice", "bob", "carol"}},
		{Team: "acme-co/core", Points: 30, Members: []string{"Alice"}},
		{Team: "acme-co/sales", Points: -20, Members: []string{"bob", "dave"}},
	}
	tests := []struct {
		author string
		want   int
	}{
		{author: "carol", want: 10},
		{author: "ALICE", want: 30}, // highest of eng and core, not the sum
		{author: "bob", want: 10},   // highest of eng and sales
		{author: "dave", want: -20}, // a lone negative team still applies
		{author: "eve", want: 0},    // in no team
		{author: "", want: 0},       // unknown author
	}
	for _, tt := range tests {
		t.Run(tt.author, func(t *testing.T) {
			if got := Score(byAuthor(tt.author), model.Classification{}, Inputs{Priority: Priority{Teams: teams}}); got != tt.want {
				t.Fatalf("Score = %d, want %d", got, tt.want)
			}
		})
	}
	if got := Score(byAuthor(""), model.Classification{}, Inputs{Priority: Priority{Teams: team(5, "")}}); got != 0 {
		t.Fatalf("empty author matched an empty member: Score = %d", got)
	}
}

func TestScoreTerms(t *testing.T) {
	term := func(name string, points int) model.ScoreTerm { return model.ScoreTerm{Term: name, Points: points} }
	in := Inputs{
		Viewer: viewer,
		Priority: Priority{
			Teams: []TeamBoost{
				{Team: "acme-co/eng", Points: 10, Members: []string{"alice"}},
				{Team: "acme-co/core", Points: 30, Members: []string{"alice"}},
			},
			Greylist: []string{"noisy"},
			Weights:  DefaultWeights,
		},
	}
	w := DefaultWeights
	tests := []struct {
		name        string
		pr          model.PR
		annotations []model.Annotation
		want        []model.ScoreTerm
	}{
		{name: "nothing", pr: basePR()},
		{
			name: "own PR with every non-attention term",
			pr: minePR(func(pr *model.PR) {
				pr.Checks, pr.ChecksRunning, pr.ChecksFailing = model.ChecksSuccess, 1, 3
				pr.Mergeable, pr.IsDraft = "CONFLICTING", true
			}),
			annotations: []model.Annotation{annotation("", UrgencyKey)},
			want: []model.ScoreTerm{
				term("urgent", w.Urgent), term("checks running", w.ChecksRunning), term("checks passed", w.ChecksSuccess),
				term("3 failing checks", 3*w.PerFailingCheck), term("merge conflict", w.MergeConflict), term("draft", w.Draft),
			},
		},
		{
			name: "one failing check, greylisted author",
			pr:   basePR(func(pr *model.PR) { pr.Author, pr.ChecksFailing = "noisy", 1 }),
			want: []model.ScoreTerm{term("greylisted author", w.Greylist), term("1 failing check", w.PerFailingCheck)},
		},
		{
			name: "best team, direct request, annotation and needs-me",
			pr: basePR(func(pr *model.PR) {
				pr.Author = "Alice"
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
			}),
			annotations: []model.Annotation{{
				PR: basePR().Key, Key: "jev.attention", UpdatedAt: at(10),
				Value: json.RawMessage(`{"score":500,"attention":["question for you"]}`),
			}},
			want: []model.ScoreTerm{
				term("team acme-co/core", 30), term("review requested from you", w.Direct),
				term("jev.attention", maxAnnotationScore), term("needs you", w.NeedsMe),
			},
		},
		{
			name: "mention",
			pr: basePR(func(pr *model.PR) {
				pr.Comments = []model.Comment{{ID: "c1", Kind: model.CommentIssue, Author: "bob", Body: "@viewer?", CreatedAt: at(5)}}
			}),
			want: []model.ScoreTerm{term(ReasonMentioned, w.Mentioned), term("needs you", w.NeedsMe)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := in
			in.Annotations = tt.annotations
			got := Evaluate(tt.pr, in)
			if !reflect.DeepEqual(got.ScoreTerms, tt.want) {
				t.Fatalf("terms = %+v, want %+v", got.ScoreTerms, tt.want)
			}
			if n := sumTerms(got.ScoreTerms); n != got.Score {
				t.Fatalf("terms sum to %d, score is %d", n, got.Score)
			}
		})
	}

	zero := in
	zero.Priority.Weights = Weights{}
	if got := Evaluate(minePR(func(pr *model.PR) { pr.IsDraft = true }), zero).ScoreTerms; got != nil {
		t.Fatalf("zero weights: terms = %+v, want none", got)
	}
}
