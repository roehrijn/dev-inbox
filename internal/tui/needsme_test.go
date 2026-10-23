package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

// attentionItem is o/r#n in sec. It needs the viewer exactly when it has reasons.
func attentionItem(sec model.Section, n, score int, title string, reasons ...string) api.InboxItem {
	it := scoredItem(sec, n, score, 0)
	it.PR.Title = title
	it.Reasons = reasons
	it.NeedsMe = len(reasons) > 0
	return it
}

// needsMeFixture has, on the Others' PRs tab, three PRs that need the viewer (one with a
// negative score) and three that do not (one with a negative score), and one PR of mine.
//
//	needs me view: Review requested #1 #3 (1 hidden), Involved #4 (2 hidden)
//	all view:      Review requested #1 #2 #3,         Involved #4 #5 (1 hidden)
func needsMeFixture() api.ListResponse {
	return listOf(
		attentionItem(model.SectionReviewRequested, 1, 40, "Direct request", "review requested"),
		attentionItem(model.SectionReviewRequested, 2, 5, "Team request"),
		attentionItem(model.SectionReviewRequested, 3, -30, "Needs me but negative", "review requested"),
		attentionItem(model.SectionInvolved, 4, 50, "Author replied", "changes since your review", "mentioned you"),
		attentionItem(model.SectionInvolved, 5, 0, "Just involved"),
		attentionItem(model.SectionInvolved, 6, -10, "Involved and negative"),
		attentionItem(model.SectionNeedsAction, 7, -5, "My PR"),
	)
}

func listedKeys(a *app) []string {
	var keys []string
	for _, e := range a.entriesOf(tabOthers) {
		keys = append(keys, a.sections[e.sec].items[e.row].PR.Key.String())
	}
	return keys
}

func TestOthersTabDefaultsToNeedsMeView(t *testing.T) {
	for _, w := range []int{120, 60} {
		a, _ := loadedApp(t, needsMeFixture(), w, 30)
		if a.showAll {
			t.Fatal("the all view is active on start")
		}
		press(t, a, "2")
		out := screen(a)
		assertFits(t, out, w, 30)
		assertOrder(t, out, "Others' PRs 3 · needs me", "▾ Review requested 2 · 1 hidden", "Direct request",
			"Needs me but negative", "▾ Involved 1 · 2 hidden", "Author replied")
		for _, title := range []string{"Team request", "Just involved", "Involved and negative"} {
			if strings.Contains(out, title) {
				t.Errorf("width %d: %q is listed in the needs me view:\n%s", w, title, out)
			}
		}
		if got, want := listedKeys(a), []string{"o/r#1", "o/r#3", "o/r#4"}; !slices.Equal(got, want) {
			t.Errorf("width %d: listed %v, want %v", w, got, want)
		}
		assertContains(t, out, "7 PRs")
	}
}

func TestToggleKeyShowsAllPRs(t *testing.T) {
	for _, w := range []int{120, 60} {
		a, _ := loadedApp(t, needsMeFixture(), w, 30)
		press(t, a, "2", "a")
		if !a.showAll {
			t.Fatalf("width %d: a did not switch to the all view", w)
		}
		out := screen(a)
		assertFits(t, out, w, 30)
		assertOrder(t, out, "Others' PRs 5 · all", "▾ Review requested 3", "Direct request", "Team request",
			"Needs me but negative", "▾ Involved 2 · 1 hidden", "Author replied", "Just involved")
		if strings.Contains(out, "Involved and negative") || strings.Contains(out, "Review requested 3 ·") {
			t.Errorf("width %d: unexpected hidden-ness in the all view:\n%s", w, out)
		}
		if got, want := listedKeys(a), []string{"o/r#1", "o/r#2", "o/r#3", "o/r#4", "o/r#5"}; !slices.Equal(got, want) {
			t.Errorf("width %d: listed %v, want %v", w, got, want)
		}

		press(t, a, "a")
		if a.showAll {
			t.Fatalf("width %d: a did not switch back to the needs me view", w)
		}
		assertContains(t, screen(a), "Others' PRs 3 · needs me", "▾ Review requested 2 · 1 hidden")
	}
}

func TestToggleKeyIsInactiveOnMyPRs(t *testing.T) {
	a, _ := loadedApp(t, needsMeFixture(), 120, 30)
	before := screen(a)
	press(t, a, "a")
	if a.showAll {
		t.Error("a switched the view on the My PRs tab")
	}
	if got := screen(a); got != before {
		t.Errorf("a changed the My PRs tab:\n%s\nwant\n%s", got, before)
	}

	press(t, a, "2", "a", "1", "a")
	if !a.showAll {
		t.Error("a on the My PRs tab switched the view back")
	}
	assertContains(t, screen(a), "My PRs 1", "Others' PRs 5 · all")
}

func TestNeedsMePRIsNeverHidden(t *testing.T) {
	a, _ := loadedApp(t, needsMeFixture(), 120, 30)
	press(t, a, "2")
	assertContains(t, screen(a), "Needs me but negative")
	press(t, a, "a")
	assertContains(t, screen(a), "Needs me but negative")
}

func TestNonNeedsMePRHidesOnlyWhenNegativeInAllView(t *testing.T) {
	a, _ := loadedApp(t, needsMeFixture(), 120, 30)
	press(t, a, "2")
	for _, title := range []string{"Team request", "Just involved", "Involved and negative"} {
		if strings.Contains(screen(a), title) {
			t.Errorf("%q is listed in the needs me view", title)
		}
	}
	press(t, a, "a")
	out := screen(a)
	assertContains(t, out, "Team request", "Just involved")
	if strings.Contains(out, "Involved and negative") {
		t.Errorf("negative PR without attention is listed in the all view:\n%s", out)
	}
}

func TestHiddenFromList(t *testing.T) {
	tests := []struct {
		name     string
		sec      model.Section
		score    int
		needsMe  bool
		all      bool
		wantHide bool
	}{
		{"needs me, needs me view", model.SectionInvolved, 10, true, false, false},
		{"needs me and negative, needs me view", model.SectionInvolved, -50, true, false, false},
		{"needs me and negative, all view", model.SectionReviewRequested, -50, true, true, false},
		{"plain, needs me view", model.SectionInvolved, 10, false, false, true},
		{"plain and negative, needs me view", model.SectionReviewRequested, -1, false, false, true},
		{"plain, all view", model.SectionInvolved, 0, false, true, false},
		{"plain and negative, all view", model.SectionInvolved, -1, false, true, true},
		{"own and negative, needs me view", model.SectionNeedsAction, -50, false, false, false},
		{"own and negative, all view", model.SectionDrafts, -50, false, true, false},
		{"own, needs me view", model.SectionWaitingForReview, 0, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := scoredItem(tt.sec, 1, tt.score, 0)
			it.NeedsMe = tt.needsMe
			if got := hiddenFromList(it, tt.all); got != tt.wantHide {
				t.Errorf("hiddenFromList = %v, want %v", got, tt.wantHide)
			}
		})
	}
}

func TestMyPRsTabIgnoresTheView(t *testing.T) {
	a, _ := loadedApp(t, needsMeFixture(), 120, 30)
	assertContains(t, screen(a), "My PRs 1", "My PR")
	press(t, a, "2", "a", "1")
	assertContains(t, screen(a), "My PRs 1", "My PR")
	if strings.Contains(screen(a), "hidden") {
		t.Errorf("My PRs mentions hidden PRs:\n%s", screen(a))
	}
}

func TestToggleKeepsSelection(t *testing.T) {
	a, _ := loadedApp(t, needsMeFixture(), 120, 30)
	press(t, a, "2", "a", "j")
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Fatalf("setup: selected %s", got)
	}

	// The selected PR does not need me and leaves; the next one takes over.
	press(t, a, "a")
	if got := selectedKey(t, a); got != "o/r#3" {
		t.Errorf("after hiding the selection: selected %s, want o/r#3", got)
	}
	press(t, a, "a")
	if got := selectedKey(t, a); got != "o/r#3" {
		t.Errorf("after showing all: selected %s, want o/r#3", got)
	}

	// Nothing below the last PR, so the PR above it takes over.
	press(t, a, "G")
	if got := selectedKey(t, a); got != "o/r#5" {
		t.Fatalf("setup: selected %s", got)
	}
	press(t, a, "a")
	if got := selectedKey(t, a); got != "o/r#4" {
		t.Errorf("after hiding the last PR: selected %s, want o/r#4", got)
	}

	// A PR that needs me stays selected in both directions.
	for range 2 {
		press(t, a, "a")
		if got := selectedKey(t, a); got != "o/r#4" {
			t.Errorf("selected %s, want o/r#4 in either view", got)
		}
	}
}

func TestToggleKeepsFoldedHeaderSelected(t *testing.T) {
	a, _ := loadedApp(t, needsMeFixture(), 120, 30)
	press(t, a, "2", "G", "h")
	if got := selectedKey(t, a); got != "header:involved" {
		t.Fatalf("setup: selected %s", got)
	}
	press(t, a, "a", "a")
	if got := selectedKey(t, a); got != "header:involved" {
		t.Errorf("selected %s after toggling, want the folded header", got)
	}
	if !a.collapsed[model.SectionInvolved] {
		t.Error("toggling unfolded the section")
	}
}

func TestViewSurvivesRelist(t *testing.T) {
	a, fb := loadedApp(t, needsMeFixture(), 120, 30)
	press(t, a, "2", "a", "j")
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Fatalf("setup: selected %s", got)
	}

	list := needsMeFixture()
	list.Items[1].Score = 7
	fb.list = list
	drain(t, a, update(t, a, keyPress("r")))
	if !a.showAll {
		t.Error("a re-list reset the view")
	}
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Errorf("selected %s after the re-list, want o/r#2", got)
	}

	// The PR starts to need me: it stays selected when the view narrows.
	list = needsMeFixture()
	list.Items[1].Reasons, list.Items[1].NeedsMe = []string{"reply to your comment"}, true
	fb.list = list
	drain(t, a, update(t, a, keyPress("r")))
	press(t, a, "a")
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Errorf("selected %s in the needs me view, want o/r#2", got)
	}
	assertContains(t, screen(a), "Others' PRs 4 · needs me", "reply to your comment")
}

func TestForeignRowsRenderReasons(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := attentionItem(model.SectionReviewRequested, 1, 0, "Look again", "review requested", "mentioned you")
	for w, want := range map[int]string{
		120: "o/r#1 · alice · just now · review requested, mentioned you",
		70:  "review requested, mentioned you",
	} {
		rows := a.renderRow(it, 0, false, w)
		assertRowFits(t, rows, w)
		if strings.Contains(ansi.Strip(rows[0]), "mentioned you") {
			t.Errorf("width %d: reasons on line 1: %q", w, ansi.Strip(rows[0]))
		}
		last := rows[len(rows)-1]
		if got := ansi.Strip(last); !strings.HasSuffix(got, want) {
			t.Errorf("width %d: last line = %q, want suffix %q", w, got, want)
		}
		if want := a.st.accent.Render("review requested, mentioned you"); !strings.Contains(last, want) {
			t.Errorf("width %d: reasons are not accent colored: %q", w, last)
		}
	}

	// The reasons come after the review decision and before the conflict marker, which the
	// reasons do not stand in for.
	it.PR.ReviewDecision = "APPROVED"
	it.PR.Mergeable = "CONFLICTING"
	l2 := a.renderRow(it, 0, false, 140)[1]
	if got, want := ansi.Strip(l2), "o/r#1 · alice · just now · approved · review requested, mentioned you · merge conflict"; !strings.HasSuffix(got, want) {
		t.Errorf("line 2 = %q, want suffix %q", got, want)
	}
	if got := strings.Count(ansi.Strip(l2), mergeConflict); got != 1 {
		t.Errorf("%d merge conflict markers, want 1: %q", got, ansi.Strip(l2))
	}
}

func TestForeignReasonsOnTheSecondLineOfTheRow(t *testing.T) {
	a, _ := loadedApp(t, needsMeFixture(), 100, 30)
	press(t, a, "2")
	lines := strings.Split(screen(a), "\n")
	for _, c := range []struct{ title, reasons string }{
		{"Direct request", "review requested"},
		{"Author replied", "changes since your review, mentioned you"},
	} {
		i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, c.title) })
		if i < 0 || i+1 >= len(lines) || !strings.Contains(lines[i+1], c.reasons) {
			t.Errorf("reasons %q are not on the line below %q:\n%s", c.reasons, c.title, strings.Join(lines, "\n"))
		}
	}
}
