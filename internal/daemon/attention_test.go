package daemon

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/github"
	"github.com/roehrijn/dev-inbox/internal/model"
)

var noAttention = json.RawMessage(`{"attention":false}`)

// mentionedPR is a foreign PR with one comment that mentions the viewer.
func mentionedPR(n int) model.PR {
	pr := openPR(n, "other")
	pr.Comments = []model.Comment{{
		ID: "C_1", Kind: model.CommentIssue, Author: "other", Body: "@" + testViewer.Login + " can you take a look?",
		CreatedAt: baseTime,
	}}
	return pr
}

func (r *running) item(t *testing.T, key model.PRKey) api.InboxItem {
	t.Helper()
	// Without the viewer nothing is attributed to them; the daemon fetches it after the API is up.
	eventually(t, "viewer known", func() bool { return r.d.currentViewer().Login != "" })
	list := request[api.ListResponse](t, r.nc, api.SubjectList, api.ListRequest{})
	if list.Error != "" {
		t.Fatalf("list error: %s", list.Error)
	}
	for _, it := range list.Items {
		if it.PR.Key == key {
			return it
		}
	}
	t.Fatalf("list lacks %s", key)
	return api.InboxItem{}
}

func eventClassification(t *testing.T, ev streamEvent) model.Classification {
	t.Helper()
	var data model.EventData
	if err := json.Unmarshal(ev.ce.Data, &data); err != nil {
		t.Fatalf("decode event data: %v", err)
	}
	return data.Classification
}

func TestCommentLevelAnnotationsDriveAttention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pr := mentionedPR(1)
	seed(t, dir, true, pr)
	r := startDaemon(t, testConfig(dir), newFake(pr))
	mentioned := []string{derive.ReasonMentioned}

	if it := r.item(t, pr.Key); !it.NeedsMe || !reflect.DeepEqual(it.Reasons, mentioned) {
		t.Fatalf("initial item: needsMe = %v, reasons = %q, want %q", it.NeedsMe, it.Reasons, mentioned)
	}

	suppress := model.Annotation{PR: pr.Key, CommentID: "C_1", Key: "jev.intent", Value: noAttention}
	r.annotate(t, suppress)
	it := r.item(t, pr.Key)
	if it.NeedsMe || len(it.Reasons) != 0 {
		t.Errorf("list after suppression: needsMe = %v, reasons = %q, want none", it.NeedsMe, it.Reasons)
	}
	if len(it.Annotations) != 0 {
		t.Errorf("list annotations = %+v, want only PR-level ones", it.Annotations)
	}
	get := request[api.GetResponse](t, r.nc, api.SubjectGet, api.GetRequest{PR: pr.Key})
	if get.Error != "" || get.Item == nil || get.Item.NeedsMe || len(get.Item.Annotations) != 1 {
		t.Errorf("get = %+v, want no attention and the comment-level annotation", get)
	}

	del := suppress
	del.Value = json.RawMessage(`null`)
	r.annotate(t, del)
	if it := r.item(t, pr.Key); !it.NeedsMe || !reflect.DeepEqual(it.Reasons, mentioned) {
		t.Errorf("list after delete: needsMe = %v, reasons = %q, want %q", it.NeedsMe, it.Reasons, mentioned)
	}

	evs := r.settledEvents(t, 2)
	if got := eventTypes(evs); !reflect.DeepEqual(got, []string{"devinbox.pr.annotated", "devinbox.pr.annotated"}) {
		t.Fatalf("event types = %q", got)
	}
	if cls := eventClassification(t, evs[0]); cls.NeedsMe || len(cls.Reasons) != 0 {
		t.Errorf("suppressing event: needsMe = %v, reasons = %q, want none", cls.NeedsMe, cls.Reasons)
	}
	if cls := eventClassification(t, evs[1]); !cls.NeedsMe || !reflect.DeepEqual(cls.Reasons, mentioned) {
		t.Errorf("deleting event: needsMe = %v, reasons = %q, want %q", cls.NeedsMe, cls.Reasons, mentioned)
	}
}

func TestIngestUsesCommentLevelAnnotations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	suppressed, plain := mentionedPR(1), mentionedPR(2)
	seed(t, dir, true, suppressed, plain)
	seedAnnotation(t, dir, model.Annotation{PR: suppressed.Key, CommentID: "C_1", Key: "jev.intent", Value: noAttention})

	gh := newFake()
	for i, pr := range []model.PR{suppressed, plain} {
		pr.Checks = model.ChecksFailure
		pr.UpdatedAt = baseTime.Add(time.Minute)
		gh.prs[pr.Key] = pr
		gh.notify(github.Notification{ThreadID: fmt.Sprintf("t%d", i), Reason: "ci_activity", PR: pr.Key, UpdatedAt: baseTime})
	}
	r := startDaemon(t, testConfig(dir), gh)

	got := map[model.PRKey]model.Classification{}
	eventually(t, "a checks_failed event per PR", func() bool {
		clear(got)
		for _, ev := range r.settledEvents(t, 1) {
			if ev.ce.Type != "devinbox.pr.checks_failed" {
				continue
			}
			var data model.EventData
			if err := json.Unmarshal(ev.ce.Data, &data); err != nil {
				t.Fatalf("decode event data: %v", err)
			}
			got[data.PR.Key] = data.Classification
		}
		return len(got) == 2
	})
	if cls := got[suppressed.Key]; cls.NeedsMe || len(cls.Reasons) != 0 {
		t.Errorf("suppressed PR event: needsMe = %v, reasons = %q, want none", cls.NeedsMe, cls.Reasons)
	}
	if cls := got[plain.Key]; !cls.NeedsMe || !reflect.DeepEqual(cls.Reasons, []string{derive.ReasonMentioned}) {
		t.Errorf("plain PR event: needsMe = %v, reasons = %q, want %q", cls.NeedsMe, cls.Reasons, derive.ReasonMentioned)
	}
}

// teamRequestedPR is a foreign PR that asks the viewer's team for a review. The offsets are far from
// derive.TeamReviewMaxAge, so a real clock cannot flip them.
func teamRequestedPR(n int, idle time.Duration) model.PR {
	pr := openPR(n, "other")
	pr.ReviewRequests = []model.ReviewRequest{{Team: testViewer.Teams[0]}}
	pr.UpdatedAt = time.Now().Add(-idle)
	return pr
}

func TestTeamReviewUsesTheClock(t *testing.T) {
	t.Parallel()
	waiting := []string{derive.ReasonTeamReviewRequested}
	score := derive.DefaultWeights.TeamReview + derive.DefaultWeights.NeedsMe

	t.Run("list, get and annotate", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fresh, stale := teamRequestedPR(1, time.Hour), teamRequestedPR(2, 2*derive.TeamReviewMaxAge)
		seed(t, dir, true, fresh, stale)
		r := startDaemon(t, testConfig(dir), newFake(fresh, stale))

		if it := r.item(t, fresh.Key); !it.NeedsMe || !reflect.DeepEqual(it.Reasons, waiting) || it.Score != score {
			t.Errorf("list, recent PR: needsMe = %v, reasons = %q, score = %d, want %q and %d", it.NeedsMe, it.Reasons, it.Score, waiting, score)
		}
		if it := r.item(t, stale.Key); it.NeedsMe || len(it.Reasons) != 0 {
			t.Errorf("list, idle PR: needsMe = %v, reasons = %q, want none", it.NeedsMe, it.Reasons)
		}
		get := request[api.GetResponse](t, r.nc, api.SubjectGet, api.GetRequest{PR: fresh.Key})
		if get.Error != "" || get.Item == nil || !reflect.DeepEqual(get.Item.Reasons, waiting) || get.Item.Score != score {
			t.Errorf("get = %+v, want reasons %q and score %d", get, waiting, score)
		}

		r.annotate(t, model.Annotation{PR: fresh.Key, Key: "linear.urgency", Value: urgency})
		evs := r.settledEvents(t, 1)
		if got := eventClassification(t, evs[0]); !got.NeedsMe || !reflect.DeepEqual(got.Reasons, waiting) {
			t.Errorf("annotated event: needsMe = %v, reasons = %q, want %q", got.NeedsMe, got.Reasons, waiting)
		}
		if got, want := eventScore(t, evs[0]), score+derive.DefaultWeights.Urgent; got != want {
			t.Errorf("annotated event score = %d, want %d", got, want)
		}
	})

	t.Run("ingest", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seed(t, dir, true)
		pr := teamRequestedPR(1, time.Hour)
		gh := newFake(pr)
		gh.notify(github.Notification{ThreadID: "t", Reason: "review_requested", PR: pr.Key, UpdatedAt: baseTime})

		r := startDaemon(t, testConfig(dir), gh)
		evs := r.settledEvents(t, 2)
		if got := eventTypes(evs); !reflect.DeepEqual(got, []string{"devinbox.pr.discovered", "devinbox.pr.review_requested"}) {
			t.Fatalf("event types = %q", got)
		}
		for _, ev := range evs {
			if got := eventClassification(t, ev); !got.NeedsMe || !reflect.DeepEqual(got.Reasons, waiting) {
				t.Errorf("%s: needsMe = %v, reasons = %q, want %q", ev.ce.Type, got.NeedsMe, got.Reasons, waiting)
			}
			if got := eventScore(t, ev); got != score {
				t.Errorf("%s score = %d, want %d", ev.ce.Type, got, score)
			}
		}
	})
}

func TestHiddenUntilNewActivity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pr := openPR(1, "other")
	pr.FetchedAt = baseTime
	seed(t, dir, true, pr)
	gh := newFake(pr)
	r := startDaemon(t, testConfig(dir), gh)

	it := r.item(t, pr.Key)
	r.annotate(t, derive.HiddenAnnotation(it.PR, it.Classification))
	if !r.item(t, pr.Key).Hidden {
		t.Fatal("list after hiding: not hidden")
	}
	if cls := eventClassification(t, r.settledEvents(t, 1)[0]); !cls.Hidden {
		t.Error("annotated event: not hidden")
	}

	update := func(thread string, mut func(*model.PR)) {
		gh.mu.Lock()
		cur := gh.prs[pr.Key]
		mut(&cur)
		gh.prs[pr.Key] = cur
		gh.mu.Unlock()
		gh.notify(github.Notification{ThreadID: thread, Reason: "subscribed", PR: pr.Key, UpdatedAt: time.Now()})
	}
	update("t1", func(cur *model.PR) { cur.Checks, cur.ChecksFailing = model.ChecksFailure, 1 })
	eventually(t, "checks_failed ingested", func() bool {
		return slices.Contains(eventTypes(r.settledEvents(t, 1)), "devinbox.pr.checks_failed")
	})
	if !r.item(t, pr.Key).Hidden {
		t.Error("list after failing checks: not hidden")
	}

	update("t2", func(cur *model.PR) {
		cur.Comments = []model.Comment{{ID: "C_1", Kind: model.CommentIssue, Author: "other", Body: "updated", CreatedAt: baseTime.Add(time.Minute)}}
	})
	eventually(t, "comment unhides", func() bool { return !r.item(t, pr.Key).Hidden })
	for _, ev := range r.settledEvents(t, 1) {
		if ev.ce.Type == "devinbox.pr.comment_added" && eventClassification(t, ev).Hidden {
			t.Error("comment_added event: still hidden")
		}
	}
}
