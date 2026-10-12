package tui

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

// scoredItem is o/r#n in sec, titled "PR n", with the given score and age. A PR of others
// does not need the viewer.
func scoredItem(sec model.Section, n, score int, ago time.Duration) api.InboxItem {
	it := item(prKey("o", "r", n), sec, "PR "+strconv.Itoa(n), ago)
	it.Score = score
	it.NeedsMe = false
	return it
}

// allApp is loadedApp with the "all" view of the Others' PRs tab, on the My PRs tab.
func allApp(t *testing.T, list api.ListResponse, w, h int) (*app, *fakeBackend) {
	t.Helper()
	a, fb := loadedApp(t, list, w, h)
	press(t, a, "2", "a", "1")
	return a, fb
}

func listOf(items ...api.InboxItem) api.ListResponse {
	return api.ListResponse{Viewer: model.Viewer{Login: "roehrijn"}, Items: items}
}

// hiddenFixture has foreign PRs with a negative score (one a direct request), foreign PRs
// at zero, and own PRs with a negative score. The "all" view of the others' tab lists 3 of 6
// PRs, mine 2.
func hiddenFixture() api.ListResponse {
	direct := scoredItem(model.SectionReviewRequested, 3, -5, 3*time.Hour)
	direct.Direct = true
	direct.PR.Title = "Direct but negative"
	own := scoredItem(model.SectionNeedsAction, 7, -7, time.Hour)
	own.PR.Title = "Own negative"
	draft := scoredItem(model.SectionDrafts, 8, -3, time.Hour)
	draft.PR.Title = "Own draft negative"
	return listOf(
		scoredItem(model.SectionReviewRequested, 1, 10, time.Hour),
		scoredItem(model.SectionReviewRequested, 2, 0, 2*time.Hour),
		direct,
		scoredItem(model.SectionReviewRequested, 4, -1, 4*time.Hour),
		scoredItem(model.SectionInvolved, 5, 0, 5*time.Hour),
		scoredItem(model.SectionInvolved, 6, -20, 6*time.Hour),
		own,
		draft,
	)
}

func TestGroupItemsHidesNegativeForeignPRs(t *testing.T) {
	direct := scoredItem(model.SectionReviewRequested, 3, -5, time.Hour)
	direct.Direct = true
	secs := groupItems([]api.InboxItem{
		scoredItem(model.SectionReviewRequested, 1, 10, time.Hour),
		scoredItem(model.SectionReviewRequested, 2, 0, time.Hour),
		direct,
		scoredItem(model.SectionInvolved, 4, -1, time.Hour),
		scoredItem(model.SectionNeedsAction, 5, -50, time.Hour),
		scoredItem(model.SectionReadyToMerge, 6, -1, time.Hour),
		scoredItem(model.SectionDrafts, 7, -1, time.Hour),
		scoredItem(model.SectionWaitingForReview, 8, -1, time.Hour),
		scoredItem(model.SectionNone, 9, -1, time.Hour),
	}, true)

	want := map[model.Section][]string{
		model.SectionReviewRequested:  {"o/r#1", "o/r#2"},
		model.SectionNeedsAction:      {"o/r#5"},
		model.SectionReadyToMerge:     {"o/r#6"},
		model.SectionDrafts:           {"o/r#7"},
		model.SectionWaitingForReview: {"o/r#8"},
	}
	got := sectionKeys(secs)
	if len(got) != len(want) {
		t.Fatalf("listed = %v, want %v", got, want)
	}
	for sec, keys := range want {
		if !slices.Equal(got[sec], keys) {
			t.Errorf("%s = %v, want %v", sec, got[sec], keys)
		}
	}
	hidden := map[model.Section]int{}
	for _, s := range secs {
		if s.hidden > 0 {
			hidden[s.id] = s.hidden
		}
	}
	if wantHidden := map[model.Section]int{model.SectionReviewRequested: 1, model.SectionInvolved: 1}; !maps.Equal(hidden, wantHidden) {
		t.Errorf("hidden = %v, want %v", hidden, wantHidden)
	}
}

func TestHiddenCountsAndHeaders(t *testing.T) {
	for _, w := range []int{120, 60, 40} {
		a, _ := allApp(t, hiddenFixture(), w, 30)

		out := screen(a)
		assertFits(t, out, w, 30)
		assertContains(t, out, "My PRs 2", "Others' PRs 3", "Own negative", "Own draft negative")
		if strings.Contains(out, "hidden") {
			t.Errorf("width %d: My PRs mentions hidden PRs:\n%s", w, out)
		}

		press(t, a, "2")
		out = screen(a)
		assertFits(t, out, w, 30)
		assertOrder(t, out, "Others' PRs 3", "▾ Review requested 2 · 2 hidden", "PR 1", "PR 2", "▾ Involved 1 · 1 hidden", "PR 5")
		for _, title := range []string{"Direct but negative", "PR 4", "PR 6"} {
			if strings.Contains(out, title) {
				t.Errorf("width %d: hidden PR %q is listed:\n%s", w, title, out)
			}
		}
	}
}

func TestHiddenPRsStayInTheStatusTotal(t *testing.T) {
	a, _ := allApp(t, hiddenFixture(), 120, 30)
	if got := a.total(); got != 8 {
		t.Errorf("total = %d, want all 8 PRs", got)
	}
	assertContains(t, screen(a), "8 PRs")
	press(t, a, "2")
	assertContains(t, screen(a), "8 PRs")
}

func TestHiddenPRsAreNotSelectable(t *testing.T) {
	a, _ := allApp(t, hiddenFixture(), 120, 30)
	press(t, a, "2")
	var seen []string
	for range 10 {
		seen = append(seen, selectedKey(t, a))
		press(t, a, "j")
	}
	for _, k := range seen {
		switch k {
		case "o/r#1", "o/r#2", "o/r#5":
		default:
			t.Errorf("cursor visited %s", k)
		}
	}
	if got := len(a.entries()); got != 3 {
		t.Errorf("entries = %d, want the 3 listed PRs", got)
	}
	press(t, a, "G")
	if got := selectedKey(t, a); got != "o/r#5" {
		t.Errorf("last entry = %s, want o/r#5", got)
	}
}

func TestAllHiddenSection(t *testing.T) {
	list := listOf(
		scoredItem(model.SectionReviewRequested, 1, 5, time.Hour),
		scoredItem(model.SectionInvolved, 2, -1, time.Hour),
		scoredItem(model.SectionInvolved, 3, -2, 2*time.Hour),
	)
	for _, w := range []int{120, 40} {
		a, _ := allApp(t, list, w, 20)
		press(t, a, "2")
		out := screen(a)
		assertFits(t, out, w, 20)
		assertOrder(t, out, "Others' PRs 1", "▾ Review requested 1", "PR 1", "Involved 0 · 2 hidden")
		if strings.Contains(out, "▾ Involved") || strings.Contains(out, "PR 2") || strings.Contains(out, "PR 3") {
			t.Errorf("width %d: all-hidden section lists rows:\n%s", w, out)
		}
		press(t, a, "j", "j", "G")
		if got := selectedKey(t, a); got != "o/r#1" {
			t.Errorf("width %d: selected %s, want the only listed PR", w, got)
		}
	}
}

func TestAllHiddenTab(t *testing.T) {
	list := listOf(
		scoredItem(model.SectionReviewRequested, 1, -5, time.Hour),
		scoredItem(model.SectionInvolved, 2, -1, time.Hour),
		scoredItem(model.SectionNeedsAction, 3, 0, time.Hour),
	)
	a, _ := allApp(t, list, 100, 20)
	press(t, a, "2")
	out := screen(a)
	assertFits(t, out, 100, 20)
	assertContains(t, out, "Others' PRs 0", "Review requested 0 · 1 hidden", "Involved 0 · 1 hidden", tabDefs[tabOthers].empty, "3 PRs")
	if _, ok := a.selected(); ok {
		t.Error("selection on a tab without listed PRs")
	}
	press(t, a, "j", "k", "G", "g", "space", "enter", "y", "d", "d")
	assertFits(t, screen(a), 100, 20)
}

func TestSelectionLeavesNewlyHiddenPR(t *testing.T) {
	rr := func(n, score int) api.InboxItem {
		return scoredItem(model.SectionReviewRequested, n, score, time.Duration(n)*time.Hour)
	}
	a, fb := allApp(t, listOf(rr(1, 40), rr(2, 30), rr(3, 20), rr(4, 10)), 140, 30)
	for _, it := range fb.list.Items {
		fb.details[it.PR.Key] = it
	}
	press(t, a, "2", "j", "j")
	drain(t, a, update(t, a, keyPress("d")))
	if got := selectedKey(t, a); got != "o/r#3" {
		t.Fatalf("setup: selected %s", got)
	}

	// The selection turns negative and moves to the PR below it.
	fb.list = listOf(rr(1, 40), rr(2, 30), rr(3, -1), rr(4, 10))
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "o/r#4" {
		t.Errorf("after hiding the selection: selected %s, want the next PR o/r#4", got)
	}
	if k, ok := a.detailKey(); !ok || k != prKey("o", "r", 4) {
		t.Errorf("detail shows %v, %v; want o/r#4", k, ok)
	}
	out := screen(a)
	assertContains(t, out, "Review requested 3 · 1 hidden", "PR 4")
	if strings.Contains(out, "PR 3") {
		t.Errorf("hidden PR still listed:\n%s", out)
	}

	// Nothing is below the last PR, so the PR above it is the nearest.
	fb.list = listOf(rr(1, 40), rr(2, 30), rr(3, -1), rr(4, -1))
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Errorf("after hiding the last PR: selected %s, want o/r#2", got)
	}

	// A hidden PR comes back once its score is no longer negative.
	fb.list = listOf(rr(1, 40), rr(2, 30), rr(3, 0), rr(4, -1))
	drain(t, a, update(t, a, keyPress("r")))
	assertContains(t, screen(a), "Review requested 3 · 1 hidden")
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Errorf("selection moved to %s when a PR reappeared", got)
	}

	// Everything turns negative.
	fb.list = listOf(rr(1, -1), rr(2, -1), rr(3, -1), rr(4, -1))
	drain(t, a, update(t, a, keyPress("r")))
	if _, ok := a.selected(); ok {
		t.Errorf("selected %s with every PR hidden", selectedKey(t, a))
	}
	assertContains(t, screen(a), "Others' PRs 0", "Review requested 0 · 4 hidden")
}

func TestSelectionMovesToNearestWhenSeveralPRsHide(t *testing.T) {
	rr := func(n, score int) api.InboxItem {
		return scoredItem(model.SectionReviewRequested, n, score, time.Duration(n)*time.Hour)
	}
	a, fb := allApp(t, listOf(rr(1, 50), rr(2, 40), rr(3, 30), rr(4, 20), rr(5, 10)), 100, 30)
	press(t, a, "2", "j", "j")
	if got := selectedKey(t, a); got != "o/r#3" {
		t.Fatalf("setup: selected %s", got)
	}

	// The PR above and the selection hide. The cursor index now points at o/r#5, but o/r#4
	// is the nearest PR left.
	fb.list = listOf(rr(1, 50), rr(2, -1), rr(3, -1), rr(4, 20), rr(5, 10))
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "o/r#4" {
		t.Errorf("selected %s, want the nearest PR o/r#4", got)
	}
}

func TestSelectionCrossesSectionWhenSectionHides(t *testing.T) {
	list := listOf(
		scoredItem(model.SectionReviewRequested, 1, 5, time.Hour),
		scoredItem(model.SectionInvolved, 2, 5, time.Hour),
	)
	a, fb := allApp(t, list, 100, 30)
	press(t, a, "2")
	if got := selectedKey(t, a); got != "o/r#1" {
		t.Fatalf("setup: selected %s", got)
	}
	fb.list = listOf(
		scoredItem(model.SectionReviewRequested, 1, -5, time.Hour),
		scoredItem(model.SectionInvolved, 2, 5, time.Hour),
	)
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Errorf("selected %s, want o/r#2 from the next section", got)
	}
}
