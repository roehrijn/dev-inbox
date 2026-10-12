package derive

import (
	"encoding/json"
	"reflect"
	"slices"
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
			if got := Classify(tt.pr, viewer); !reflect.DeepEqual(got, tt.want) {
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
			if got := Classify(pr, viewer).Section; got != tt.want {
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

// checkEvents asserts the events match ws in type and order, carry cur's key, snapshot and
// classification with the given score, and that each Change decodes into the type of the expected one.
func checkEvents(t *testing.T, events []model.Event, ws []want, cur model.PR, in Inputs, score int) {
	t.Helper()
	if got, wantT := types(events), wantTypes(ws); !slices.Equal(got, wantT) {
		t.Fatalf("event types = %v, want %v", got, wantT)
	}
	wantCls := Classify(cur, in.Viewer)
	wantCls.Score = score
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
		if !reflect.DeepEqual(data.Classification, wantCls) {
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

	tests := []struct {
		name  string
		old   *model.PR
		cur   model.PR
		want  []want
		score int // expected score of cur, e.g. DefaultWeights.Direct for a request naming the viewer personally
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
			score: DefaultWeights.Direct,
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
			score: DefaultWeights.Direct,
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
			score: DefaultWeights.Direct,
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
			score: DefaultWeights.Direct,
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
			checkEvents(t, events, tt.want, tt.cur, inputs, tt.score)
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
			in:   Inputs{Priority: Priority{PreferredMembers: []string{"bob", "alice"}}},
			want: DefaultWeights.PreferredTeam,
		},
		{
			name: "preferred compares case-insensitively",
			pr:   byAuthor("Alice"),
			in:   Inputs{Priority: Priority{PreferredMembers: []string{"aLICE"}}},
			want: DefaultWeights.PreferredTeam,
		},
		{
			name: "author outside the preferred team",
			pr:   byAuthor("carol"),
			in:   Inputs{Priority: Priority{PreferredTeam: "acme-co/core", PreferredMembers: []string{"alice", "bob"}}},
			want: 0,
		},
		{
			name: "empty author is not preferred",
			pr:   byAuthor(""),
			in:   Inputs{Priority: Priority{PreferredMembers: []string{""}}},
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
			in:   Inputs{Priority: Priority{PreferredMembers: []string{"alice"}}, Annotations: urgent},
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
			in:   Inputs{Priority: Priority{PreferredMembers: []string{"alice"}, Greylist: []string{"alice"}}},
			want: 15,
		},
		{
			name: "all four terms",
			pr:   byAuthor("alice"),
			cls:  direct,
			in: Inputs{
				Priority:    Priority{PreferredMembers: []string{"alice"}, Greylist: []string{"alice"}},
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
			name: "all terms",
			pr: basePR(func(pr *model.PR) {
				pr.Author = "alice"
				pr.Checks, pr.ChecksRunning, pr.ChecksFailing = model.ChecksPending, 1, 2
				pr.Mergeable = "CONFLICTING"
			}),
			cls: direct,
			in: Inputs{
				Priority:    Priority{PreferredMembers: []string{"alice"}, Greylist: []string{"alice"}},
				Annotations: urgent,
			},
			want: 45 + DefaultWeights.ChecksRunning + 2*DefaultWeights.PerFailingCheck + DefaultWeights.MergeConflict,
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
		PreferredTeam:   11,
		Greylist:        -13,
		Direct:          17,
		ChecksRunning:   19,
		ChecksSuccess:   23,
		PerFailingCheck: -29,
		MergeConflict:   -31,
	}
	custom := func(mut func(*Weights)) Weights {
		out := w
		mut(&out)
		return out
	}
	urgent := []model.Annotation{annotation("", UrgencyKey)}
	direct := model.Classification{Section: model.SectionReviewRequested, Direct: true}
	alice := []string{"alice"}
	// everything triggers every term: 7+11-13+17+19+23-2*29-31.
	everything := basePR(func(pr *model.PR) {
		pr.Author = "alice"
		pr.Checks, pr.ChecksRunning, pr.ChecksFailing = model.ChecksSuccess, 1, 2
		pr.Mergeable = "CONFLICTING"
	})
	const everythingScore = -25
	all := func(weights Weights) Inputs {
		return Inputs{
			Priority:    Priority{PreferredMembers: alice, Greylist: alice, Weights: weights},
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
			in:   Inputs{Priority: Priority{PreferredMembers: alice, Weights: w}},
			want: w.PreferredTeam,
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
			name: "zero weights score zero",
			pr:   everything,
			cls:  direct,
			in:   all(Weights{}),
			want: 0,
		},
		{
			name: "unset weights do not fall back to the defaults",
			pr:   everything,
			cls:  direct,
			in:   all(Weights{Urgent: 5}),
			want: 5,
		},
		{
			name: "zero urgent disables only urgent",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.Urgent = 0 })),
			want: everythingScore - w.Urgent,
		},
		{
			name: "zero preferred team disables only preferred team",
			pr:   everything,
			cls:  direct,
			in:   all(custom(func(w *Weights) { w.PreferredTeam = 0 })),
			want: everythingScore - w.PreferredTeam,
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
				Priority:    Priority{PreferredMembers: []string{"alice"}, Weights: DefaultWeights},
				Annotations: []model.Annotation{annotation("", UrgencyKey)},
			},
			want: model.Classification{Section: model.SectionReviewRequested, Direct: true, Score: 60},
		},
		{
			name: "score uses the configured weights",
			pr: basePR(func(pr *model.PR) {
				pr.Author = "alice"
				pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
			}),
			in: Inputs{
				Viewer:      viewer,
				Priority:    Priority{PreferredMembers: []string{"alice"}, Weights: Weights{Urgent: 1, PreferredTeam: 2, Direct: 4}},
				Annotations: []model.Annotation{annotation("", UrgencyKey)},
			},
			want: model.Classification{Section: model.SectionReviewRequested, Direct: true, Score: 7},
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
			if got := Evaluate(tt.pr, tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Evaluate = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDiffEventsCarryScore(t *testing.T) {
	in := Inputs{
		Viewer:      viewer,
		Priority:    Priority{PreferredTeam: "acme-co/core", PreferredMembers: []string{"alice"}, Weights: DefaultWeights},
		Annotations: []model.Annotation{annotation("", UrgencyKey)},
	}
	direct := model.ChangeReviewRequest{ReviewRequest: model.ReviewRequest{Login: "viewer"}, Direct: true}
	requested := func(pr *model.PR) {
		pr.Author = "alice"
		pr.ReviewRequests = []model.ReviewRequest{{Login: "viewer"}}
	}
	wantScore := DefaultWeights.Urgent + DefaultWeights.PreferredTeam + DefaultWeights.Direct

	tests := []struct {
		name string
		old  *model.PR
		cur  model.PR
		want []want
	}{
		{
			name: "discovery",
			cur:  basePR(requested),
			want: []want{{model.EventDiscovered, nil}, {model.EventReviewRequested, direct}},
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
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := Diff(tt.old, tt.cur, in)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			checkEvents(t, events, tt.want, tt.cur, in, wantScore)
			for i, ev := range events {
				var data eventData
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatalf("event %d (%s) data: %v", i, ev.Type, err)
				}
				if data.Score != wantScore {
					t.Errorf("event %d (%s) score = %d, want %d", i, ev.Type, data.Score, wantScore)
				}
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
