package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
)

var (
	t0   = time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.UTC)
	keyA = model.PRKey{Owner: "acme-co", Repo: "frontend", Number: 1}
	keyB = model.PRKey{Owner: "acme-co", Repo: "frontend", Number: 2}
	keyC = model.PRKey{Owner: "acme-co", Repo: "acme-enterprise", Number: 3}
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newPR(key model.PRKey, state model.PRState, updated time.Time, comments ...model.Comment) model.PR {
	return model.PR{
		Key: key,
		PRStatus: model.PRStatus{
			State: state, IsDraft: true, HeadSHA: "abc123", UpdatedAt: updated, Checks: model.ChecksFailure,
			Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED", InMergeQueue: true,
		},
		NodeID:         "PR_" + key.String(),
		Title:          "fix: " + key.String(),
		URL:            "https://github.com/" + key.Owner + "/" + key.Repo + "/pull/1",
		Author:         "alice",
		BaseRef:        "main",
		HeadRef:        "alice/fix",
		Labels:         []string{"bug", "backport"},
		Assignees:      []string{"bob"},
		Additions:      10,
		Deletions:      2,
		ChangedFiles:   3,
		CreatedAt:      t0.Add(-time.Hour),
		LastPushAt:     t0.Add(-time.Minute),
		ReviewRequests: []model.ReviewRequest{{Login: "carol"}, {Team: "acme-co/core"}},
		Reviews:        []model.Review{{ID: "R1", Author: "carol", State: "APPROVED", SubmittedAt: t0}},
		CheckRuns:      []model.CheckRun{{Name: "build", Status: "COMPLETED", Conclusion: "FAILURE", URL: "https://ci/1"}},
		Comments:       comments,
		FetchedAt:      updated.Add(time.Second),
	}
}

func newComment(id string, created time.Time) model.Comment {
	return model.Comment{
		ID: id, Kind: model.CommentInline, Author: "bob", Body: "body of " + id,
		URL: "https://github.com/c/" + id, Path: "main.go", Line: 7, ReplyTo: "parent", CreatedAt: created,
	}
}

func newEvent(id, typ string, key model.PRKey, at time.Time) model.Event {
	return model.Event{ID: id, Type: typ, PR: key, Time: at, Data: json.RawMessage(`{"id":"` + id + `"}`)}
}

func newAnnotation(key model.PRKey, commentID, annKey, value string) model.Annotation {
	return model.Annotation{
		PR: key, CommentID: commentID, Key: annKey, Value: json.RawMessage(value),
		Source: "test", UpdatedAt: t0.Add(5 * time.Minute),
	}
}

func mustApply(t *testing.T, s *Store, pr model.PR, events ...model.Event) {
	t.Helper()
	if err := s.ApplySnapshot(t.Context(), pr, events); err != nil {
		t.Fatalf("ApplySnapshot(%s): %v", pr.Key, err)
	}
}

func mustSetAnnotation(t *testing.T, s *Store, a model.Annotation, ev model.Event) {
	t.Helper()
	if err := s.SetAnnotation(t.Context(), a, ev); err != nil {
		t.Fatalf("SetAnnotation(%s %s): %v", a.PR, a.Key, err)
	}
}

func pendingIDs(t *testing.T, s *Store) []string {
	t.Helper()
	events, err := s.PendingEvents(t.Context(), 1000)
	if err != nil {
		t.Fatalf("PendingEvents: %v", err)
	}
	ids := []string{}
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	return ids
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func prNumbers(prs []model.PR) []int {
	nums := []int{}
	for _, pr := range prs {
		nums = append(nums, pr.Key.Number)
	}
	return nums
}

func commentIDs(comments []model.Comment) []string {
	ids := []string{}
	for _, c := range comments {
		ids = append(ids, c.ID)
	}
	return ids
}

func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s mismatch\n got: %+v\nwant: %+v", what, got, want)
	}
}

func TestOpenConfiguresConnection(t *testing.T) {
	s := openStore(t)
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pragma string
		want   any
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", int64(1)},
		{"busy_timeout", int64(5000)},
		{"user_version", int64(len(migrations))},
	} {
		var got any
		if err := s.db.QueryRowContext(t.Context(), "PRAGMA "+tc.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", tc.pragma, err)
		}
		if got != tc.want {
			t.Errorf("PRAGMA %s = %v, want %v", tc.pragma, got, tc.want)
		}
	}
}

func TestOpenCreatesPrivateFileAtAwkwardPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state dir #1 ?x%41", "inbox.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("database not created at the requested path: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("database mode = %v, want no group or other access", mode)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()

	// IDs and creation times are deliberately out of order; equal times fall back to the ID.
	pr := newPR(keyA, model.PRStateOpen, t0,
		newComment("c3", t0.Add(3*time.Minute)),
		newComment("c1", t0.Add(time.Minute)),
		newComment("c2", t0.Add(2*time.Minute)),
		newComment("b-tie", t0.Add(4*time.Minute)),
		newComment("a-tie", t0.Add(4*time.Minute)),
	)
	mustApply(t, s, pr)

	want := pr
	want.Comments = []model.Comment{
		newComment("c1", t0.Add(time.Minute)),
		newComment("c2", t0.Add(2*time.Minute)),
		newComment("c3", t0.Add(3*time.Minute)),
		newComment("a-tie", t0.Add(4*time.Minute)),
		newComment("b-tie", t0.Add(4*time.Minute)),
	}

	got, ok, err := s.GetPR(ctx, keyA)
	if err != nil || !ok {
		t.Fatalf("GetPR = ok %v, err %v", ok, err)
	}
	equal(t, "GetPR", got, want)

	listed, err := s.ListPRs(ctx, false)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	equal(t, "ListPRs", listed, []model.PR{want})

	t.Run("key lookup ignores case", func(t *testing.T) {
		got, ok, err := s.GetPR(ctx, model.PRKey{Owner: "ACME-CO", Repo: "FRontend", Number: 1})
		if err != nil || !ok {
			t.Fatalf("GetPR = ok %v, err %v", ok, err)
		}
		equal(t, "GetPR", got, want)
	})

	t.Run("unknown PR", func(t *testing.T) {
		got, ok, err := s.GetPR(ctx, model.PRKey{Owner: "acme-co", Repo: "frontend", Number: 99})
		if err != nil || ok {
			t.Fatalf("GetPR = ok %v, err %v, want not found without error", ok, err)
		}
		equal(t, "GetPR", got, model.PR{})
	})

	t.Run("reapplying is idempotent", func(t *testing.T) {
		mustApply(t, s, pr)
		got, _, _ := s.GetPR(ctx, keyA)
		equal(t, "GetPR", got, want)
		if n := countRows(t, s, "prs"); n != 1 {
			t.Errorf("prs rows = %d, want 1", n)
		}
	})

	t.Run("snapshot is stored without comments", func(t *testing.T) {
		var snapshot string
		if err := s.db.QueryRowContext(ctx, "SELECT snapshot FROM prs").Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(snapshot), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["comments"]; ok {
			t.Errorf("snapshot contains comments: %s", snapshot)
		}
	})

	t.Run("caller's PR is not modified", func(t *testing.T) {
		if len(pr.Comments) != 5 {
			t.Errorf("ApplySnapshot modified the caller's comments: %v", commentIDs(pr.Comments))
		}
	})
}

func TestApplySnapshotUpdatesPR(t *testing.T) {
	s := openStore(t)
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0))

	merged := newPR(keyA, model.PRStateMerged, t0.Add(time.Hour))
	merged.Title = "new title"
	mustApply(t, s, merged)

	got, _, err := s.GetPR(t.Context(), keyA)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "GetPR", got, merged)

	open, err := s.ListPRs(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("merged PR listed as open: %v", prNumbers(open))
	}
}

func TestCommentsKeptWhenMissingFromNewerSnapshot(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()

	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0,
		newComment("c1", t0.Add(time.Minute)),
		newComment("c2", t0.Add(2*time.Minute)),
	))

	edited := newComment("c2", t0.Add(2*time.Minute))
	edited.Body = "edited"
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0.Add(time.Hour),
		edited,
		newComment("c3", t0.Add(3*time.Minute)),
	))

	got, _, err := s.GetPR(ctx, keyA)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "comment IDs", commentIDs(got.Comments), []string{"c1", "c2", "c3"})
	equal(t, "edited comment", got.Comments[1], edited)

	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0.Add(2*time.Hour)))
	got, _, _ = s.GetPR(ctx, keyA)
	equal(t, "comment IDs after a snapshot without comments", commentIDs(got.Comments), []string{"c1", "c2", "c3"})

	listed, err := s.ListPRs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "ListPRs comment IDs", commentIDs(listed[0].Comments), []string{"c1", "c2", "c3"})
}

func TestComments(t *testing.T) {
	t.Run("without ID are rejected and nothing is written", func(t *testing.T) {
		s := openStore(t)
		err := s.ApplySnapshot(t.Context(), newPR(keyA, model.PRStateOpen, t0, newComment("", t0)),
			[]model.Event{newEvent("e1", model.EventDiscovered, keyA, t0)})
		if err == nil {
			t.Fatal("ApplySnapshot succeeded")
		}
		if n := countRows(t, s, "prs") + countRows(t, s, "comments") + countRows(t, s, "events"); n != 0 {
			t.Errorf("%d rows written", n)
		}
	})

	t.Run("belong to their own PR", func(t *testing.T) {
		s := openStore(t)
		mustApply(t, s, newPR(keyA, model.PRStateOpen, t0, newComment("a1", t0)))
		mustApply(t, s, newPR(keyB, model.PRStateOpen, t0, newComment("b1", t0), newComment("b2", t0.Add(time.Second))))

		a, _, _ := s.GetPR(t.Context(), keyA)
		b, _, _ := s.GetPR(t.Context(), keyB)
		equal(t, "PR A comments", commentIDs(a.Comments), []string{"a1"})
		equal(t, "PR B comments", commentIDs(b.Comments), []string{"b1", "b2"})
	})
}

func TestListPRs(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()

	empty, err := s.ListPRs(ctx, false)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("ListPRs on empty store = %v, %v, want empty non-nil slice", empty, err)
	}

	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0.Add(2*time.Hour), newComment("a1", t0)))
	mustApply(t, s, newPR(keyB, model.PRStateMerged, t0.Add(3*time.Hour), newComment("b1", t0)))
	mustApply(t, s, newPR(keyC, model.PRStateOpen, t0.Add(time.Hour)))
	mustApply(t, s, newPR(model.PRKey{Owner: "acme-co", Repo: "frontend", Number: 4}, model.PRStateClosed, t0.Add(4*time.Hour)))

	for _, tc := range []struct {
		name     string
		openOnly bool
		want     []int
	}{
		{"all, most recently updated first", false, []int{4, 2, 1, 3}},
		{"open only", true, []int{1, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prs, err := s.ListPRs(ctx, tc.openOnly)
			if err != nil {
				t.Fatal(err)
			}
			equal(t, "PR numbers", prNumbers(prs), tc.want)
			for _, pr := range prs {
				switch pr.Key {
				case keyA:
					equal(t, "comments of A", commentIDs(pr.Comments), []string{"a1"})
				case keyB:
					equal(t, "comments of B", commentIDs(pr.Comments), []string{"b1"})
				default:
					if pr.Comments != nil {
						t.Errorf("%s has comments %v", pr.Key, commentIDs(pr.Comments))
					}
				}
			}
		})
	}
}

func TestOutbox(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()

	// IDs are not sorted so that ordering by insertion is distinguishable from ordering by ID.
	evB := newEvent("ev-b", model.EventUpdated, keyA, t0.Add(time.Hour))
	evA := newEvent("ev-a", model.EventDiscovered, keyA, t0)
	evC := newEvent("ev-c", model.EventAnnotated, keyA, t0.Add(10*time.Hour))
	evD := newEvent("ev-d", model.EventDiscovered, keyB, t0.Add(10*time.Hour))
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0), evB, evA)
	mustSetAnnotation(t, s, newAnnotation(keyA, "", "k", `1`), evC)
	mustApply(t, s, newPR(keyB, model.PRStateOpen, t0), evD)

	pending, err := s.PendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "pending events", pending, []model.Event{evB, evA, evC, evD})

	t.Run("limit", func(t *testing.T) {
		got, err := s.PendingEvents(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, "limited events", got, []model.Event{evB, evA})
		if got, _ := s.PendingEvents(ctx, 0); len(got) != 0 {
			t.Errorf("PendingEvents(0) = %d events, want none", len(got))
		}
	})

	if err := s.MarkPublished(ctx, nil); err != nil {
		t.Fatalf("MarkPublished(nil): %v", err)
	}
	if err := s.MarkPublished(ctx, []string{"ev-a", "ev-d", "unknown"}); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	equal(t, "pending after MarkPublished", pendingIDs(t, s), []string{"ev-b", "ev-c"})

	if err := s.MarkPublished(ctx, []string{"ev-a"}); err != nil {
		t.Fatalf("MarkPublished twice: %v", err)
	}
	equal(t, "pending after repeated MarkPublished", pendingIDs(t, s), []string{"ev-b", "ev-c"})

	remaining := func() []string {
		rows, err := s.db.QueryContext(ctx, "SELECT id FROM events ORDER BY seq")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}

	// ev-a (published, t0), ev-b (pending, t0+1h), ev-c (pending, t0+10h), ev-d (published, t0+10h)
	prune := func(before time.Time) int64 {
		n, err := s.PruneEvents(ctx, before)
		if err != nil {
			t.Fatalf("PruneEvents: %v", err)
		}
		return n
	}

	if n := prune(t0); n != 0 {
		t.Errorf("PruneEvents(before == event time) = %d, want 0", n)
	}
	if n := prune(t0.Add(5 * time.Hour)); n != 1 {
		t.Errorf("PruneEvents = %d, want 1 (only ev-a is published and older)", n)
	}
	equal(t, "events after first prune", remaining(), []string{"ev-b", "ev-c", "ev-d"})

	if err := s.MarkPublished(ctx, []string{"ev-b", "ev-c"}); err != nil {
		t.Fatal(err)
	}
	if n := prune(t0.Add(20 * time.Hour)); n != 3 {
		t.Errorf("PruneEvents = %d, want 3", n)
	}
	equal(t, "events after second prune", remaining(), []string{})

	evE := newEvent("ev-e", model.EventUpdated, keyA, t0)
	evF := newEvent("ev-f", model.EventUpdated, keyA, t0)
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0), evF, evE)
	equal(t, "pending after pruning everything", pendingIDs(t, s), []string{"ev-f", "ev-e"})
}

func TestEventTimestampAndMissingData(t *testing.T) {
	s := openStore(t)

	withTime := newEvent("with-time", model.EventUpdated, keyA, time.Date(2026, 10, 1, 12, 0, 0, 1, time.UTC))
	noData := model.Event{ID: "no-data", Type: model.EventUpdated, PR: keyA, Time: t0}
	zeroTime := newEvent("zero-time", model.EventUpdated, keyA, time.Time{})
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0), withTime, noData, zeroTime)

	got, err := s.PendingEvents(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "events", got, []model.Event{withTime, noData, zeroTime})
}

func TestAnnotationUpsert(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0))

	first := newAnnotation(keyA, "", "linear.urgency", `{"badge":"P1","level":"crit"}`)
	mustSetAnnotation(t, s, first, newEvent("e1", model.EventAnnotated, keyA, t0))
	got, err := s.Annotations(ctx, keyA)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "annotations", got, []model.Annotation{first})

	second := first
	second.Value = json.RawMessage(`{"badge":"P3"}`)
	second.Source = "other"
	second.UpdatedAt = t0.Add(time.Hour)
	mustSetAnnotation(t, s, second, newEvent("e2", model.EventAnnotated, keyA, t0))
	got, _ = s.Annotations(ctx, keyA)
	equal(t, "annotations after update", got, []model.Annotation{second})

	// The same key at PR level and on a comment are two annotations.
	onComment := newAnnotation(keyA, "c1", "linear.urgency", `"note"`)
	mustSetAnnotation(t, s, onComment, newEvent("e3", model.EventAnnotated, keyA, t0))
	got, _ = s.Annotations(ctx, keyA)
	equal(t, "annotations with comment level", got, []model.Annotation{second, onComment})

	equal(t, "outbox", pendingIDs(t, s), []string{"e1", "e2", "e3"})
}

func TestAnnotationDelete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value json.RawMessage
	}{
		{"JSON null", json.RawMessage(`null`)},
		{"null with whitespace", json.RawMessage(" null\n")},
		{"nil", nil},
		{"empty", json.RawMessage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			ctx := t.Context()
			mustApply(t, s, newPR(keyA, model.PRStateOpen, t0))
			mustSetAnnotation(t, s, newAnnotation(keyA, "", "a", `1`), newEvent("e1", model.EventAnnotated, keyA, t0))
			mustSetAnnotation(t, s, newAnnotation(keyA, "", "b", `2`), newEvent("e2", model.EventAnnotated, keyA, t0))
			mustSetAnnotation(t, s, newAnnotation(keyA, "c1", "a", `3`), newEvent("e3", model.EventAnnotated, keyA, t0))

			del := newAnnotation(keyA, "", "a", "")
			del.Value = tc.value
			mustSetAnnotation(t, s, del, newEvent("e4", model.EventAnnotated, keyA, t0))

			got, err := s.Annotations(ctx, keyA)
			if err != nil {
				t.Fatal(err)
			}
			remaining := []string{}
			for _, a := range got {
				remaining = append(remaining, a.Key+"@"+a.CommentID)
			}
			equal(t, "remaining annotations", remaining, []string{"b@", "a@c1"})
			equal(t, "outbox", pendingIDs(t, s), []string{"e1", "e2", "e3", "e4"})

			// Deleting something that is not there is not an error.
			mustSetAnnotation(t, s, del, newEvent("e5", model.EventAnnotated, keyA, t0))
		})
	}
}

func TestAnnotationRejections(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0))

	t.Run("invalid JSON", func(t *testing.T) {
		err := s.SetAnnotation(ctx, newAnnotation(keyA, "", "k", `{not json`), newEvent("e1", model.EventAnnotated, keyA, t0))
		if err == nil {
			t.Fatal("SetAnnotation succeeded")
		}
	})

	t.Run("unknown PR", func(t *testing.T) {
		for _, value := range []string{`1`, `null`} {
			err := s.SetAnnotation(ctx, newAnnotation(keyB, "", "k", value), newEvent("e2", model.EventAnnotated, keyB, t0))
			if !errors.Is(err, ErrPRNotFound) {
				t.Errorf("SetAnnotation(%s) = %v, want ErrPRNotFound", value, err)
			}
		}
	})

	if n := countRows(t, s, "annotations") + countRows(t, s, "events"); n != 0 {
		t.Errorf("%d rows written by rejected calls", n)
	}
}

func TestAnnotationLevels(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0))
	mustApply(t, s, newPR(keyB, model.PRStateOpen, t0))
	mustApply(t, s, newPR(keyC, model.PRStateOpen, t0))

	set := func(id string, a model.Annotation) {
		t.Helper()
		mustSetAnnotation(t, s, a, newEvent(id, model.EventAnnotated, a.PR, t0))
	}
	set("e1", newAnnotation(keyA, "", "b", `{"badge":"B"}`))
	set("e2", newAnnotation(model.PRKey{Owner: "ACME-CO", Repo: "FRontend", Number: 1}, "", "a", `"a"`))
	// Another spelling of the PR key addresses the same annotation, so this updates it in place.
	set("e3", newAnnotation(model.PRKey{Owner: "Acme-Co", Repo: "frontend", Number: 1}, "", "a", `"a2"`))
	set("e4", newAnnotation(keyA, "c1", "a", `"on c1"`))
	set("e5", newAnnotation(keyA, "c0", "z", `"on c0"`))
	set("e6", newAnnotation(keyB, "", "d", `4`))

	prA := newAnnotation(keyA, "", "a", `"a2"`)
	prB := newAnnotation(keyA, "", "b", `{"badge":"B"}`)
	prD := newAnnotation(keyB, "", "d", `4`)

	t.Run("AnnotationsByPR returns both levels grouped by PR", func(t *testing.T) {
		got, err := s.AnnotationsByPR(ctx)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, "AnnotationsByPR", got, map[model.PRKey][]model.Annotation{
			keyA: {
				prA, prB,
				newAnnotation(keyA, "c0", "z", `"on c0"`),
				newAnnotation(keyA, "c1", "a", `"on c1"`),
			},
			keyB: {prD},
		})
	})

	t.Run("Annotations returns both levels", func(t *testing.T) {
		got, err := s.Annotations(ctx, model.PRKey{Owner: "acme-CO", Repo: "frontend", Number: 1})
		if err != nil {
			t.Fatal(err)
		}
		equal(t, "Annotations", got, []model.Annotation{
			prA, prB,
			newAnnotation(keyA, "c0", "z", `"on c0"`),
			newAnnotation(keyA, "c1", "a", `"on c1"`),
		})
	})

	t.Run("PR without annotations", func(t *testing.T) {
		got, err := s.Annotations(ctx, keyC)
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("Annotations = %v, %v, want empty non-nil slice", got, err)
		}
	})
}

func TestMeta(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()

	if v, ok, err := s.Meta(ctx, "notifications.last_modified"); err != nil || ok || v != "" {
		t.Fatalf("Meta on unset key = %q, %v, %v", v, ok, err)
	}
	for _, value := range []string{"Wed, 01 Oct 2026 12:00:00 GMT", "newer", ""} {
		if err := s.SetMeta(ctx, "notifications.last_modified", value); err != nil {
			t.Fatalf("SetMeta: %v", err)
		}
		if v, ok, err := s.Meta(ctx, "notifications.last_modified"); err != nil || !ok || v != value {
			t.Errorf("Meta = %q, %v, %v, want %q", v, ok, err, value)
		}
	}
	if _, ok, _ := s.Meta(ctx, "other"); ok {
		t.Error("Meta found a key that was never set")
	}
}

func TestApplySnapshotRollsBackOnDuplicateEventID(t *testing.T) {
	t.Run("new PR", func(t *testing.T) {
		s := openStore(t)
		ctx := t.Context()
		err := s.ApplySnapshot(ctx, newPR(keyA, model.PRStateOpen, t0, newComment("c1", t0)), []model.Event{
			newEvent("dup", model.EventDiscovered, keyA, t0),
			newEvent("dup", model.EventUpdated, keyA, t0),
		})
		if err == nil {
			t.Fatal("ApplySnapshot succeeded")
		}
		if _, ok, _ := s.GetPR(ctx, keyA); ok {
			t.Error("PR persisted despite the failed event insert")
		}
		for _, table := range []string{"prs", "comments", "events"} {
			if n := countRows(t, s, table); n != 0 {
				t.Errorf("%s has %d rows after rollback", table, n)
			}
		}
	})

	t.Run("existing PR keeps its previous state", func(t *testing.T) {
		s := openStore(t)
		ctx := t.Context()
		v1 := newPR(keyA, model.PRStateOpen, t0, newComment("c1", t0))
		mustApply(t, s, v1, newEvent("e1", model.EventDiscovered, keyA, t0))

		v2 := newPR(keyA, model.PRStateMerged, t0.Add(time.Hour), newComment("c1", t0), newComment("c2", t0.Add(time.Minute)))
		v2.Title = "changed"
		edited := newComment("c1", t0)
		edited.Body = "edited"
		v2.Comments[0] = edited
		err := s.ApplySnapshot(ctx, v2, []model.Event{
			newEvent("e2", model.EventMerged, keyA, t0),
			newEvent("e1", model.EventUpdated, keyA, t0),
		})
		if err == nil {
			t.Fatal("ApplySnapshot succeeded")
		}

		got, _, _ := s.GetPR(ctx, keyA)
		equal(t, "PR after rollback", got, v1)
		equal(t, "outbox after rollback", pendingIDs(t, s), []string{"e1"})
	})
}

func TestSetAnnotationRollsBackOnDuplicateEventID(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	mustApply(t, s, newPR(keyA, model.PRStateOpen, t0))
	v1 := newAnnotation(keyA, "", "k", `"v1"`)
	mustSetAnnotation(t, s, v1, newEvent("e1", model.EventAnnotated, keyA, t0))

	t.Run("insert", func(t *testing.T) {
		err := s.SetAnnotation(ctx, newAnnotation(keyA, "", "new", `1`), newEvent("e1", model.EventAnnotated, keyA, t0))
		if err == nil {
			t.Fatal("SetAnnotation succeeded")
		}
	})
	t.Run("update", func(t *testing.T) {
		err := s.SetAnnotation(ctx, newAnnotation(keyA, "", "k", `"v2"`), newEvent("e1", model.EventAnnotated, keyA, t0))
		if err == nil {
			t.Fatal("SetAnnotation succeeded")
		}
	})
	t.Run("delete", func(t *testing.T) {
		err := s.SetAnnotation(ctx, newAnnotation(keyA, "", "k", `null`), newEvent("e1", model.EventAnnotated, keyA, t0))
		if err == nil {
			t.Fatal("SetAnnotation succeeded")
		}
	})

	got, err := s.Annotations(ctx, keyA)
	if err != nil {
		t.Fatal(err)
	}
	v1.PR = keyA
	equal(t, "annotations after rollbacks", got, []model.Annotation{v1})
	equal(t, "outbox after rollbacks", pendingIDs(t, s), []string{"e1"})
}

func TestReopenKeepsDataAndSkipsMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	ctx := t.Context()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pr := newPR(keyA, model.PRStateOpen, t0, newComment("c1", t0))
	ann := newAnnotation(keyA, "", "k", `{"badge":"x"}`)
	mustApply(t, s, pr, newEvent("e1", model.EventDiscovered, keyA, t0), newEvent("e2", model.EventUpdated, keyA, t0))
	mustSetAnnotation(t, s, ann, newEvent("e3", model.EventAnnotated, keyA, t0))
	if err := s.SetMeta(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPublished(ctx, []string{"e1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The initial migration uses plain CREATE TABLE, so running it again would fail.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()

	got, ok, err := s.GetPR(ctx, keyA)
	if err != nil || !ok {
		t.Fatalf("GetPR after reopen = ok %v, err %v", ok, err)
	}
	equal(t, "PR after reopen", got, pr)
	anns, _ := s.Annotations(ctx, keyA)
	ann.PR = keyA
	equal(t, "annotations after reopen", anns, []model.Annotation{ann})
	if v, ok, _ := s.Meta(ctx, "k"); !ok || v != "v" {
		t.Errorf("Meta after reopen = %q, %v", v, ok)
	}
	equal(t, "outbox after reopen", pendingIDs(t, s), []string{"e2", "e3"})

	mustApply(t, s, newPR(keyB, model.PRStateOpen, t0), newEvent("e4", model.EventDiscovered, keyB, t0))
	equal(t, "outbox after write on reopened store", pendingIDs(t, s), []string{"e2", "e3", "e4"})
}

func TestMigrate(t *testing.T) {
	open := func(t *testing.T) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migrate.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	version := func(t *testing.T, db *sql.DB) int {
		t.Helper()
		var v int
		if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	hasTable := func(t *testing.T, db *sql.DB, name string) bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	ctx := context.Background()
	first, second := "CREATE TABLE a (x);", "CREATE TABLE b (y); CREATE INDEX b_y ON b (y);"

	t.Run("applies only what is new", func(t *testing.T) {
		db := open(t)
		if err := migrate(ctx, db, []string{first}); err != nil {
			t.Fatal(err)
		}
		if v := version(t, db); v != 1 {
			t.Fatalf("version = %d, want 1", v)
		}
		// Re-running the first migration would fail on the existing table.
		if err := migrate(ctx, db, []string{first, second}); err != nil {
			t.Fatal(err)
		}
		if v := version(t, db); v != 2 || !hasTable(t, db, "b") {
			t.Errorf("version = %d, table b = %v, want 2 and true", v, hasTable(t, db, "b"))
		}
		if err := migrate(ctx, db, []string{first, second}); err != nil {
			t.Fatalf("migrate on an up-to-date database: %v", err)
		}
	})

	t.Run("rejects a newer schema", func(t *testing.T) {
		db := open(t)
		if err := migrate(ctx, db, []string{first, second}); err != nil {
			t.Fatal(err)
		}
		if err := migrate(ctx, db, []string{first}); err == nil {
			t.Fatal("migrate succeeded on a database with a newer schema")
		}
	})

	t.Run("failure leaves no partial schema", func(t *testing.T) {
		db := open(t)
		if err := migrate(ctx, db, []string{first, "NOT SQL"}); err == nil {
			t.Fatal("migrate succeeded")
		}
		if v := version(t, db); v != 0 || hasTable(t, db, "a") {
			t.Errorf("version = %d, table a = %v, want 0 and false", v, hasTable(t, db, "a"))
		}
	})
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded on a database with a newer schema")
	}
}

func TestConcurrentUse(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()

	const workers, rounds = 8, 15
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds*4)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := model.PRKey{Owner: "acme-co", Repo: "frontend", Number: 100 + w}
			for i := range rounds {
				id := fmt.Sprintf("w%d-%d", w, i)
				pr := newPR(key, model.PRStateOpen, t0.Add(time.Duration(i)*time.Minute), newComment(id, t0.Add(time.Duration(i)*time.Second)))
				errs <- s.ApplySnapshot(ctx, pr, []model.Event{newEvent(id+"-snap", model.EventUpdated, key, t0)})
				errs <- s.SetAnnotation(ctx, newAnnotation(key, "", "k", fmt.Sprint(i)), newEvent(id+"-ann", model.EventAnnotated, key, t0))
				_, err := s.ListPRs(ctx, true)
				errs <- err
				_, err = s.PendingEvents(ctx, 10)
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation failed: %v", err)
		}
	}

	prs, err := s.ListPRs(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != workers {
		t.Errorf("%d PRs stored, want %d", len(prs), workers)
	}
	for _, pr := range prs {
		if len(pr.Comments) != rounds {
			t.Errorf("%s has %d comments, want %d", pr.Key, len(pr.Comments), rounds)
		}
	}
	if n := countRows(t, s, "events"); n != workers*rounds*2 {
		t.Errorf("%d events stored, want %d", n, workers*rounds*2)
	}
}
