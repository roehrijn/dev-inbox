package tui

import (
	"errors"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

// assertFits checks that the screen fills exactly h lines of at most w cells.
func assertFits(t *testing.T, out string, w, h int) {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		t.Errorf("screen has %d lines, want %d:\n%s", len(lines), h, out)
	}
	for i, l := range lines {
		if lw := ansi.StringWidth(l); lw > w {
			t.Errorf("line %d is %d cells wide, max %d: %q", i, lw, w, l)
		}
	}
}

func assertOrder(t *testing.T, s string, subs ...string) {
	t.Helper()
	last := -1
	for _, sub := range subs {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Errorf("missing %q", sub)
			continue
		}
		if i < last {
			t.Errorf("%q appears out of order", sub)
		}
		last = i
	}
}

func TestViewWide(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 120, 30)
	out := screen(a)
	assertFits(t, out, 120, 30)
	assertOrder(t, out,
		"dev-inbox",
		"My PRs 2",
		"Others' PRs 3",
		"▾ Needs action 1",
		"✗ Fix the flaky webhook test that keeps failing on main for everyone",
		"urgent",
		"roehrijn/dev-inbox#3 · roehrijn · 2h ago · checks failing, changes requested",
		"Ready to merge 0",
		"▾ Your drafts 1",
		"● ◌ WIP: detail pane",
		"roehrijn/dev-inbox#4 · roehrijn · 3d ago · draft",
		"Waiting for review 0",
		"● connected · 5 PRs · @roehrijn · refreshed ",
		"? help",
	)
	if strings.Contains(out, "DEVOPS-42") {
		t.Error("non-badge annotation rendered in the list")
	}
	if strings.Count(out, "changes requested") != 1 {
		t.Error("review decision repeated although a needs_action reason says the same")
	}
	if strings.Contains(out, "▾ Ready to merge") {
		t.Error("empty section rendered as expandable")
	}
	for _, other := range []string{"Review requested", "Involved", "Direct request, older", "other/repo#99"} {
		if strings.Contains(out, other) {
			t.Errorf("%q of the other tab rendered on My PRs", other)
		}
	}

	press(t, a, "2")
	out = screen(a)
	assertFits(t, out, 120, 30)
	assertOrder(t, out,
		"▾ Review requested 2",
		"★ ✓ Direct request, older",
		"acme-co/frontend#12 · alice · 5h ago · approved",
		"Team request, updated recently",
		"▾ Involved 1",
		"other/repo#99 · alice · 40m ago",
		"● connected · 5 PRs · @roehrijn · refreshed ",
	)
	for _, mine := range []string{"Needs action", "Your drafts", "Fix the flaky webhook"} {
		if strings.Contains(out, mine) {
			t.Errorf("%q of My PRs rendered on Others' PRs", mine)
		}
	}
}

func TestViewNarrow(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 60, 30)
	out := screen(a)
	assertFits(t, out, 60, 30)
	assertContains(t, out,
		"▾ Needs action 1",
		"✗ Fix the flaky webhook test that keeps fai… 20  urgent",
		"roehrijn/dev-inbox#3 · roehrijn · 2h ago  ",
		"checks failing, changes requested",
		"● connected · 5 PRs · @roehrijn",
	)
	press(t, a, "2")
	out = screen(a)
	assertFits(t, out, 60, 30)
	assertContains(t, out, "▾ Review requested 2", "★ ✓ Direct request, older")
}

func scoredList(scores ...int) api.ListResponse {
	var items []api.InboxItem
	for i, score := range scores {
		// Later items are more recent, so only the score can put them first.
		it := item(prKey("o", "r", i+1), model.SectionNeedsAction, "Title "+string(rune('A'+i)), time.Duration(len(scores)-i)*time.Hour)
		it.Score = score
		items = append(items, it)
	}
	return api.ListResponse{Items: items}
}

func TestViewOrdersRowsByScore(t *testing.T) {
	for _, w := range []int{120, 60, 40} {
		a, _ := loadedApp(t, scoredList(0, -15, 30, 10), w, 30)
		out := screen(a)
		assertFits(t, out, w, 30)
		assertOrder(t, out, "Title C", "Title D", "Title A", "Title B")
		if got := selectedKey(t, a); got != "o/r#3" {
			t.Errorf("width %d: selected %s, want the highest score", w, got)
		}
	}
}

func TestRowScore(t *testing.T) {
	urgent := badgeAnn(prKey("o", "r", 1), "linear.urgency", `{"badge":"urgent","level":"crit"}`)
	tests := []struct {
		name   string
		w      int
		score  int
		badges []model.Annotation
		// suffix is what the first line must end with; empty means no digit may appear.
		suffix string
	}{
		{name: "positive wide", w: 120, score: 30, suffix: "30"},
		{name: "positive narrow", w: 40, score: 30, suffix: "30"},
		{name: "positive tiny", w: 20, score: 30, suffix: "30"},
		{name: "negative wide", w: 120, score: -15, suffix: "-15"},
		{name: "negative narrow", w: 40, score: -15, suffix: "-15"},
		{name: "negative tiny", w: 20, score: -15, suffix: "-15"},
		{name: "zero wide", w: 120},
		{name: "zero narrow", w: 40},
		{name: "zero tiny", w: 20},
		{name: "before badges wide", w: 120, score: 20, badges: []model.Annotation{urgent}, suffix: "20  urgent "},
		{name: "before badges narrow", w: 40, score: 20, badges: []model.Annotation{urgent}, suffix: "20  urgent "},
		{name: "badges yield to the score", w: 20, score: 20, badges: []model.Annotation{urgent}, suffix: "20"},
		{name: "zero keeps badges", w: 40, badges: []model.Annotation{urgent}, suffix: "urgent "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestApp(&fakeBackend{})
			it := item(prKey("o", "r", 1), model.SectionNeedsAction, "Fix the flaky webhook test", time.Hour)
			it.Score, it.Annotations = tt.score, tt.badges
			plain := ansi.Strip(a.renderRow(it, 0, false, tt.w)[0])
			if got := ansi.StringWidth(plain); got > tt.w {
				t.Errorf("first line is %d cells wide, max %d: %q", got, tt.w, plain)
			}
			if tt.suffix == "" {
				if strings.ContainsAny(plain, "0123456789") {
					t.Errorf("zero score rendered: %q", plain)
				}
				return
			}
			if !strings.HasSuffix(plain, tt.suffix) {
				t.Errorf("first line %q does not end with %q", plain, tt.suffix)
			}
		})
	}
}

func TestRowScoreSurvivesTinyWidths(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := item(prKey("o", "r", 1), model.SectionNeedsAction, "Fix the flaky webhook test", time.Hour)
	it.Score = 100
	for w := 0; w <= 12; w++ {
		assertRowFits(t, a.renderRow(it, 0, true, w), w)
	}
}

func assertRowFits(t *testing.T, rows []string, w int) {
	t.Helper()
	for _, l := range rows {
		if ansi.StringWidth(l) > w {
			t.Errorf("width %d: row overflows: %q", w, ansi.Strip(strings.Join(rows, " / ")))
			return
		}
	}
}

func TestRowSplitsMetaWhenNarrow(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := item(prKey("o", "r", 1), model.SectionNeedsAction, "Fix the flaky webhook test", time.Hour)
	it.PR.ReviewDecision = "APPROVED"
	it.Reasons = []string{"checks failing"}
	tests := []struct {
		w    int
		want []string
	}{
		{wideRowMinWidth, []string{"o/r#1 · alice · 1h ago · approved · checks failing"}},
		{wideRowMinWidth - 1, []string{"o/r#1 · alice · 1h ago", "approved · checks failing"}},
		{40, []string{"o/r#1 · alice · 1h ago", "approved · checks failing"}},
	}
	for _, tt := range tests {
		rows := a.renderRow(it, 0, false, tt.w)
		if len(rows) != rowHeight(tt.w) {
			t.Fatalf("width %d: %d lines, want %d", tt.w, len(rows), rowHeight(tt.w))
		}
		assertRowFits(t, rows, tt.w)
		for i, want := range tt.want {
			if got := strings.TrimSpace(ansi.Strip(rows[i+1])); got != want {
				t.Errorf("width %d: line %d = %q, want %q", tt.w, i+2, got, want)
			}
		}
	}

	it.Reasons, it.PR.ReviewDecision = nil, ""
	if got := a.renderRow(it, 0, false, 40)[2]; strings.TrimSpace(got) != "" {
		t.Errorf("state line of a PR without state = %q, want blank", ansi.Strip(got))
	}
}

func TestRowDraftMark(t *testing.T) {
	long := "Draft a very long title that cannot fit into the first line of a row at all"
	tests := []struct {
		name  string
		draft bool
		title string
		w     int
	}{
		{"draft", true, "WIP: detail pane", 100},
		{"not a draft", false, "WIP: detail pane", 100},
		{"long draft wide", true, long, 80},
		{"long draft narrow", true, long, 40},
		{"long draft tiny", true, long, 16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestApp(&fakeBackend{})
			it := item(prKey("o", "r", 1), model.SectionDrafts, tt.title, time.Hour)
			it.PR.IsDraft = tt.draft
			it.Score = 20
			rows := a.renderRow(it, 0, false, tt.w)
			l1, l2 := rows[0], rows[1]
			plain := ansi.Strip(l1)
			if got := ansi.StringWidth(plain); got > tt.w {
				t.Errorf("first line is %d cells wide, max %d: %q", got, tt.w, plain)
			}
			// The score at the end survives only if the mark is part of the title's budget.
			if !strings.HasSuffix(plain, "20") {
				t.Errorf("first line %q lost the score", plain)
			}
			if !tt.draft {
				if strings.Contains(plain, "◌") {
					t.Errorf("non-draft row marked: %q", plain)
				}
				return
			}
			if !strings.Contains(plain, "◌ "+tt.title[:3]) {
				t.Errorf("first line %q has no ◌ right before the title", plain)
			}
			if !strings.Contains(l1, a.st.draft.Render("◌")) {
				t.Errorf("mark is not rendered in the draft style: %q", l1)
			}
			if tt.w >= 80 && !strings.Contains(ansi.Strip(l2), "draft") {
				t.Errorf("second line %q lost the draft legend", ansi.Strip(l2))
			}
		})
	}
}

func TestRenderScoreColors(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	if got := a.renderScore(0); got != "" {
		t.Errorf("zero score = %q, want it hidden", got)
	}
	if got, want := a.renderScore(30), a.st.accent.Render("30"); got != want {
		t.Errorf("positive score = %q, want accent %q", got, want)
	}
	if got, want := a.renderScore(-15), a.st.muted.Render("-15"); got != want {
		t.Errorf("negative score = %q, want muted %q", got, want)
	}
	if a.st.accent.Render("1") == a.st.muted.Render("1") {
		t.Fatal("accent and muted styles are indistinguishable")
	}
}

func TestViewTiny(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 20, 5)
	assertFits(t, screen(a), 20, 5)
	press(t, a, "d", "G", "?")
	assertFits(t, screen(a), 20, 5)
	press(t, a, "2", "shift+tab")
	assertFits(t, screen(a), 20, 5)
}

func TestViewScrollsToCursor(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 80, 8)
	press(t, a, "2", "G")
	out := screen(a)
	assertFits(t, out, 80, 8)
	assertContains(t, out, "▾ Involved 1", "Something I commented on", "other/repo#99")
	if strings.Contains(out, "Review requested") {
		t.Errorf("top of the list still visible after G:\n%s", out)
	}
	press(t, a, "g")
	out = screen(a)
	assertContains(t, out, "▾ Review requested 2", "Direct request, older")
}

func TestViewZeroItems(t *testing.T) {
	a, _ := loadedApp(t, api.ListResponse{Viewer: model.Viewer{Login: "roehrijn"}}, 80, 20)
	out := screen(a)
	assertFits(t, out, 80, 20)
	assertOrder(t, out, "My PRs 0", "Others' PRs 0", "Needs action 0", "Ready to merge 0", "Your drafts 0",
		"Waiting for review 0", "You have no open pull requests.", "0 PRs")

	press(t, a, "2")
	out = screen(a)
	assertFits(t, out, 80, 20)
	assertOrder(t, out, "Review requested 0", "Involved 0", "Nothing to review. All caught up.", "0 PRs")
}

func TestViewMissingFields(t *testing.T) {
	list := api.ListResponse{Items: []api.InboxItem{{
		PR:             model.PR{Key: prKey("o", "r", 1)},
		Classification: model.Classification{Section: model.SectionInvolved, NeedsMe: true},
	}}}
	a, _ := loadedApp(t, list, 80, 20)
	press(t, a, "2")
	out := screen(a)
	assertFits(t, out, 80, 20)
	assertContains(t, out, "(no title)", "o/r#1", "1 PR")
	press(t, a, "d")
	assertContains(t, screen(a), "(no title)")
}

func TestViewLoading(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	update(t, a, tea.WindowSizeMsg{Width: 80, Height: 20})
	update(t, a, connMsg{connected: true})
	out := screen(a)
	assertFits(t, out, 80, 20)
	assertContains(t, out, "Loading inbox")
}

func TestViewBeforeWindowSize(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	if got := a.View().Content; got != "" {
		t.Fatalf("view before size = %q", got)
	}
}

func TestViewHelp(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 120, 30)
	press(t, a, "?")
	out := screen(a)
	assertFits(t, out, 120, 30)
	assertContains(t, out, "open in browser", "fold section", "copy URL", "details", "refresh", "quit",
		"next tab", "prev tab", "jump tab")
	press(t, a, "esc")
	if strings.Contains(screen(a), "open in browser") {
		t.Error("esc did not close help")
	}
}

func openDetail(t *testing.T, w, h int) (*app, *fakeBackend) {
	t.Helper()
	a, fb := loadedApp(t, fixtureList(), w, h)
	fb.details[prKey("roehrijn", "dev-inbox", 3)] = fixtureDetail()
	drain(t, a, update(t, a, keyPress("d")))
	return a, fb
}

func TestDetailSplit(t *testing.T) {
	a, _ := openDetail(t, 140, 70)
	out := screen(a)
	assertFits(t, out, 140, 70)
	// The list keeps its rows on the left, the detail pane sits beside it.
	assertContains(t, out, "▾ Needs action 1", "│ Fix the flaky webhook test that keeps failing on main for everyone")
	assertOrder(t, out,
		"roehrijn/dev-inbox#3 · open · by roehrijn · opened 3d ago · updated 2h ago",
		"main ← fix/webhook",
		"+120 −34 · 7 files changed",
		"Needs action · Priority 20 · checks failing, changes requested",
		"Labels    bug, area/sync",
		"Annotations",
		"linear.id",
		`"DEVOPS-42"`,
		"linear.urgency   urgent",
		"ai.orphan",
		`{"score":0.5}`,
		"on comment gone",
		"Checks 1 failed, 1 running, 1 passed",
		"✗ e2e  failure",
		"● lint  in progress",
		"✓ unit  success",
		"Reviews",
		"✗ bob changes requested · 2h ago",
		"awaiting carol, acme-co/eng",
		"Comments 2",
		"bob · 1h ago",
		"Please fix this.",
		"fmt.Println(1)",
		"carol · 10m ago · on internal/x.go:12",
		"nitpick",
		"rename",
	)
	if strings.Contains(out, "bob commented") {
		t.Error("older review of the same author listed")
	}
	if strings.Contains(out, "on comment c2") {
		t.Error("annotation of a shown comment listed with the PR annotations")
	}
}

func TestDetailShowsPriority(t *testing.T) {
	for _, tt := range []struct {
		score int
		want  string
	}{{30, "Priority 30"}, {-15, "Priority -15"}, {0, "Priority 0"}} {
		a, _ := loadedApp(t, scoredList(tt.score), 140, 30)
		drain(t, a, update(t, a, keyPress("d")))
		assertContains(t, screen(a), "Needs action · "+tt.want)
	}
}

func TestDetailStackedOnNarrowTerminal(t *testing.T) {
	a, _ := openDetail(t, 80, 40)
	out := screen(a)
	assertFits(t, out, 80, 40)
	if strings.Contains(out, "│") {
		t.Error("narrow terminal rendered a side-by-side split")
	}
	assertOrder(t, out, "▾ Needs action 1", "Fix the flaky webhook", strings.Repeat("─", 80),
		"main ← fix/webhook")
}

func TestDetailScroll(t *testing.T) {
	a, _ := openDetail(t, 140, 20)
	if strings.Contains(screen(a), "carol · 10m ago") {
		t.Fatal("setup: comments already visible")
	}
	for range 20 {
		press(t, a, "J")
	}
	out := screen(a)
	assertFits(t, out, 140, 20)
	assertContains(t, out, "carol · 10m ago")
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#3" {
		t.Fatalf("scrolling the detail moved the selection to %s", got)
	}
	press(t, a, "pgup", "pgup", "pgup", "K")
	assertContains(t, screen(a), "main ← fix/webhook")
}

func TestDetailLoadingAndError(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 140, 40)
	cmd := update(t, a, keyPress("d"))
	assertContains(t, screen(a), "Fix the flaky webhook", "Loading comments…")

	// fb has no details, so the fetch fails.
	drain(t, a, cmd)
	assertContains(t, screen(a), "Could not load comments: roehrijn/dev-inbox#3 not found")

	fb.details[prKey("roehrijn", "dev-inbox", 3)] = fixtureDetail()
	drain(t, a, update(t, a, keyPress("r")))
	assertContains(t, screen(a), "Comments 2", "Please fix this.")
}

func TestDetailOnCollapsedHeader(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 140, 40)
	press(t, a, "2", "space", "d")
	assertContains(t, screen(a), "2 pull requests folded")
}

func TestDetailToggle(t *testing.T) {
	a, _ := openDetail(t, 140, 40)
	press(t, a, "d")
	if strings.Contains(screen(a), "Checks 1 failed") {
		t.Fatal("d did not close the detail pane")
	}
	press(t, a, "d")
	assertContains(t, screen(a), "Checks 1 failed")
	press(t, a, "esc")
	if a.showDetail {
		t.Fatal("esc did not close the detail pane")
	}
}

func TestDarkBackground(t *testing.T) {
	a, _ := openDetail(t, 140, 40)
	light := a.View().Content
	update(t, a, tea.BackgroundColorMsg{Color: color.Black})
	if !a.dark {
		t.Fatal("black background not detected as dark")
	}
	dark := a.View().Content
	if light == dark {
		t.Error("colors did not change for a dark background")
	}
	if ansi.Strip(light) != ansi.Strip(dark) {
		t.Error("text changed with the background color")
	}
}

func TestStatusShowsRefreshError(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 120, 20)
	update(t, a, listMsg{seq: a.listSeq + 1, err: errors.New("timeout")})
	assertContains(t, screen(a), "refresh failed: timeout", "5 PRs")
}

func scoreTermsItem(terms ...model.ScoreTerm) api.InboxItem {
	it := scoredItem(model.SectionNeedsAction, 1, 0, time.Hour)
	it.ScoreTerms = terms
	for _, t := range terms {
		it.Score += t.Points
	}
	return it
}

func TestDetailScoreSection(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := scoreTermsItem(
		model.ScoreTerm{Term: "urgent", Points: 30},
		model.ScoreTerm{Term: "failing checks", Points: -40},
		model.ScoreTerm{Term: "draft", Points: -5},
		model.ScoreTerm{Term: "needs you", Points: 100},
	)
	it.Annotations = []model.Annotation{badgeAnn(it.PR.Key, "linear.id", `"DEVOPS-42"`)}
	out := ansi.Strip(a.renderPR(it, true, nil, 80))
	assertOrder(t, out, "Priority 85", "Score 85", "urgent", "+30", "failing checks", "-40", "draft", "-5", "needs you", "+100", "Annotations")

	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  ") && (strings.HasSuffix(l, "0") || strings.HasSuffix(l, "5")) && !strings.Contains(l, "linear.id") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("score lines = %q, want 4", lines)
	}
	end := ansi.StringWidth(lines[0])
	for _, l := range lines {
		if got := ansi.StringWidth(l); got != end {
			t.Errorf("points column not aligned: %q is %d cells, want %d", l, got, end)
		}
	}
	for i, want := range []string{"urgent  ", "failing checks", "draft", "needs you"} {
		if !strings.HasPrefix(lines[i], "  "+want) {
			t.Errorf("line %d = %q, want label %q", i, lines[i], want)
		}
	}
	for i, want := range []string{"  +30", "  -40", "   -5", "+100"} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("line %d = %q, want points %q right-aligned", i, lines[i], want)
		}
	}
}

func TestDetailScoreStyles(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	var b strings.Builder
	a.writeScore(&b, scoreTermsItem(model.ScoreTerm{Term: "up", Points: 10}, model.ScoreTerm{Term: "down", Points: -10}), 40)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %q", lines)
	}
	if !strings.HasSuffix(lines[1], a.st.accent.Render("+10")) {
		t.Errorf("positive points not in accent style: %q", lines[1])
	}
	if !strings.HasSuffix(lines[2], a.st.muted.Render("-10")) {
		t.Errorf("negative points not in muted style: %q", lines[2])
	}
}

func TestDetailScoreOmittedWithoutTerms(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := scoreTermsItem()
	it.Score = 20
	out := ansi.Strip(a.renderPR(it, true, nil, 80))
	if strings.Contains(out, "Score") {
		t.Errorf("score section shown without terms:\n%s", out)
	}
	assertContains(t, out, "Priority 20")
}

func TestDetailScoreTruncatesToWidth(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := scoreTermsItem(
		model.ScoreTerm{Term: "team acme-co/web-platform-core", Points: 20},
		model.ScoreTerm{Term: "changes since your review", Points: -140},
		model.ScoreTerm{Term: "x", Points: 5},
	)
	for _, w := range []int{60, 20, 12, 8, 4, 2} {
		var b strings.Builder
		a.writeScore(&b, it, w)
		for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n")[1:] {
			if lw := ansi.StringWidth(l); lw > w {
				t.Errorf("w=%d: line is %d cells wide: %q", w, lw, ansi.Strip(l))
			}
		}
	}
	var b strings.Builder
	a.writeScore(&b, it, 20)
	out := ansi.Strip(b.String())
	assertContains(t, out, "team acme-c…   +20", "-140")
	if strings.Contains(out, "web-platform-core") {
		t.Errorf("long label not truncated:\n%s", out)
	}
}
