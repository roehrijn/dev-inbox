package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

func conflictItem(sec model.Section, mergeable string, reasons ...string) api.InboxItem {
	it := item(prKey("o", "r", 1), sec, "Rebase me", time.Hour)
	it.PR.Mergeable = mergeable
	it.Reasons = reasons
	return it
}

// flat collapses line breaks, so a marker that wraps still counts.
func flat(s string) string { return strings.Join(strings.Fields(ansi.Strip(s)), " ") }

func TestRowConflictMarker(t *testing.T) {
	tests := []struct {
		name string
		it   api.InboxItem
		want int
	}{
		{"review requested", conflictItem(model.SectionReviewRequested, "CONFLICTING"), 1},
		{"involved", conflictItem(model.SectionInvolved, "CONFLICTING"), 1},
		{"ready to merge", conflictItem(model.SectionReadyToMerge, "CONFLICTING"), 1},
		{"drafts", conflictItem(model.SectionDrafts, "CONFLICTING"), 1},
		{"waiting for review", conflictItem(model.SectionWaitingForReview, "CONFLICTING"), 1},
		{"needs action without the reason", conflictItem(model.SectionNeedsAction, "CONFLICTING", "checks failing"), 1},
		{"needs action with the reason", conflictItem(model.SectionNeedsAction, "CONFLICTING", "checks failing", "merge conflict"), 1},
		{"reason alone", conflictItem(model.SectionNeedsAction, "CONFLICTING", "merge conflict"), 1},
		{"mergeable", conflictItem(model.SectionReviewRequested, "MERGEABLE"), 0},
		{"unknown", conflictItem(model.SectionReviewRequested, "UNKNOWN"), 0},
		{"not computed", conflictItem(model.SectionInvolved, ""), 0},
	}
	for _, tt := range tests {
		for _, w := range []int{120, 80} {
			t.Run(fmt.Sprintf("%s/w%d", tt.name, w), func(t *testing.T) {
				a := newTestApp(&fakeBackend{})
				l1, l2 := a.renderRow(tt.it, false, w)
				if got := strings.Count(ansi.Strip(l2), mergeConflict); got != tt.want {
					t.Errorf("width %d: %d markers on line 2, want %d: %q", w, got, tt.want, ansi.Strip(l2))
				}
				if strings.Contains(ansi.Strip(l1), mergeConflict) {
					t.Errorf("width %d: marker on line 1: %q", w, ansi.Strip(l1))
				}
			})
		}
	}
}

func TestRowConflictMarkerFollowsMetadata(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := conflictItem(model.SectionReviewRequested, "CONFLICTING")
	it.PR.IsDraft = true
	_, l2 := a.renderRow(it, false, 120)
	if got, want := ansi.Strip(l2), "o/r#1 · alice · 1h ago · draft · merge conflict"; !strings.HasSuffix(got, want) {
		t.Errorf("line 2 = %q, want suffix %q", got, want)
	}
	if !strings.HasSuffix(l2, a.st.warning.Render(mergeConflict)) {
		t.Errorf("marker is not warning colored: %q", l2)
	}

	it = conflictItem(model.SectionNeedsAction, "CONFLICTING", "checks failing", "merge conflict")
	_, l2 = a.renderRow(it, false, 120)
	if got, want := ansi.Strip(l2), "o/r#1 · alice · 1h ago · checks failing, merge conflict"; !strings.HasSuffix(got, want) {
		t.Errorf("line 2 = %q, want suffix %q", got, want)
	}
}

func TestRowConflictMarkerSurvivesNarrowWidths(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := conflictItem(model.SectionInvolved, "CONFLICTING")
	for w := 0; w <= 60; w++ {
		l1, l2 := a.renderRow(it, true, w)
		if ansi.StringWidth(l1) > w || ansi.StringWidth(l2) > w {
			t.Errorf("width %d: row overflows: %q / %q", w, ansi.Strip(l1), ansi.Strip(l2))
		}
	}
}

func TestConflictMarkerColor(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	got := a.conflictMarker(conflictItem(model.SectionInvolved, "CONFLICTING"))
	if want := a.st.warning.Render(mergeConflict); got != want {
		t.Errorf("marker = %q, want warning %q", got, want)
	}
	for name, st := range map[string]string{
		"accent": a.st.accent.Render(mergeConflict),
		"danger": a.st.danger.Render(mergeConflict),
		"muted":  a.st.muted.Render(mergeConflict),
	} {
		if got == st {
			t.Errorf("marker is indistinguishable from %s", name)
		}
	}
	if got := a.conflictMarker(conflictItem(model.SectionInvolved, "MERGEABLE")); got != "" {
		t.Errorf("marker for a mergeable PR = %q", got)
	}
}

func TestViewShowsConflictsInBothTabs(t *testing.T) {
	rr := item(prKey("acme-co", "frontend", 11), model.SectionReviewRequested, "Foreign request", time.Hour)
	rr.PR.Mergeable = "CONFLICTING"
	inv := item(prKey("other", "repo", 99), model.SectionInvolved, "Foreign involvement", 2*time.Hour)
	inv.PR.Mergeable = "CONFLICTING"
	clean := item(prKey("other", "repo", 100), model.SectionInvolved, "Clean involvement", 3*time.Hour)
	clean.PR.Mergeable = "MERGEABLE"
	own := item(prKey("roehrijn", "dev-inbox", 3), model.SectionNeedsAction, "Own conflict", time.Hour)
	own.PR.Mergeable = "CONFLICTING"
	own.Reasons = []string{"merge conflict"}
	ownClean := item(prKey("roehrijn", "dev-inbox", 4), model.SectionReadyToMerge, "Own ready", time.Hour)
	ownClean.PR.Mergeable = "UNKNOWN"
	list := api.ListResponse{
		Viewer: model.Viewer{Login: "roehrijn"},
		Items:  []api.InboxItem{rr, inv, clean, own, ownClean},
	}

	for _, w := range []int{120, 70} {
		a, _ := loadedApp(t, list, w, 30)
		out := screen(a)
		assertFits(t, out, w, 30)
		if got := strings.Count(out, mergeConflict); got != 1 {
			t.Errorf("width %d: My PRs shows %d markers, want 1 (no duplicate for the reason):\n%s", w, got, out)
		}

		press(t, a, "2")
		out = screen(a)
		assertFits(t, out, w, 30)
		if got := strings.Count(out, mergeConflict); got != 2 {
			t.Errorf("width %d: Others' PRs shows %d markers, want 2:\n%s", w, got, out)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "other/repo#100") && strings.Contains(line, mergeConflict) {
				t.Errorf("width %d: mergeable PR marked: %q", w, line)
			}
		}
	}
}

func TestDetailConflictMarker(t *testing.T) {
	tests := []struct {
		name string
		it   api.InboxItem
		// status is the status line the marker belongs to; empty when no marker is expected.
		status string
	}{
		{"review requested", conflictItem(model.SectionReviewRequested, "CONFLICTING"), "Review requested · Priority 0 · merge conflict"},
		{"involved", conflictItem(model.SectionInvolved, "CONFLICTING"), "Involved · Priority 0 · merge conflict"},
		{"after the decision", withDecision(conflictItem(model.SectionReviewRequested, "CONFLICTING"), "APPROVED"), "Review requested · Priority 0 · approved · merge conflict"},
		{"own with the reason", conflictItem(model.SectionNeedsAction, "CONFLICTING", "merge conflict"), "Needs action · Priority 0 · merge conflict"},
		{"mergeable", conflictItem(model.SectionReviewRequested, "MERGEABLE"), ""},
		{"unknown", conflictItem(model.SectionInvolved, "UNKNOWN"), ""},
	}
	for _, tt := range tests {
		for _, w := range []int{120, 60} {
			t.Run(fmt.Sprintf("%s/w%d", tt.name, w), func(t *testing.T) {
				a := newTestApp(&fakeBackend{})
				out := flat(a.renderPR(tt.it, true, nil, w))
				if tt.status == "" {
					if strings.Contains(out, mergeConflict) {
						t.Errorf("width %d: unexpected marker:\n%s", w, out)
					}
					return
				}
				if !strings.Contains(out, tt.status) {
					t.Errorf("width %d: missing status %q in:\n%s", w, tt.status, out)
				}
				if got := strings.Count(out, mergeConflict); got != 1 {
					t.Errorf("width %d: %d markers, want 1:\n%s", w, got, out)
				}
			})
		}
	}
}

func withDecision(it api.InboxItem, d string) api.InboxItem {
	it.PR.ReviewDecision = d
	return it
}

func TestDetailPaneShowsConflictOfForeignPR(t *testing.T) {
	for _, w := range []int{140, 80} {
		it := conflictItem(model.SectionReviewRequested, "CONFLICTING")
		a, fb := loadedApp(t, api.ListResponse{Items: []api.InboxItem{it}}, w, 40)
		fb.details[it.PR.Key] = it
		press(t, a, "2")
		drain(t, a, update(t, a, keyPress("d")))
		out := screen(a)
		assertFits(t, out, w, 40)
		assertContains(t, out, "Review requested · Priority 0 · merge conflict")
		// Once in the list row, once in the detail status.
		if got := strings.Count(out, mergeConflict); got != 2 {
			t.Errorf("width %d: %d markers, want 2:\n%s", w, got, out)
		}
	}
}
