package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

func TestSectionTabMapping(t *testing.T) {
	want := map[int][]model.Section{
		tabMine: {
			model.SectionNeedsAction, model.SectionReadyToMerge, model.SectionDrafts, model.SectionWaitingForReview,
		},
		tabOthers: {model.SectionReviewRequested, model.SectionInvolved},
	}
	for _, sec := range displaySections {
		if _, ok := sectionTab[sec]; !ok {
			t.Errorf("section %s has no tab", sec)
		}
	}
	if len(sectionTab) != len(displaySections) {
		t.Errorf("%d sections mapped, %d displayed", len(sectionTab), len(displaySections))
	}
	for ti, sec := range hiddenSections {
		if sectionTab[sec] != ti {
			t.Errorf("hidden section %s is on tab %d, want %d", sec, sectionTab[sec], ti)
		}
	}

	a, _ := loadedApp(t, fixtureList(), 100, 40)
	for ti, def := range tabDefs {
		var got []model.Section
		for _, si := range a.tabSections(ti) {
			got = append(got, a.sections[si].id)
		}
		if len(got) != len(want[ti]) {
			t.Errorf("%s lists %v, want %v", def.title, got, want[ti])
			continue
		}
		for _, sec := range want[ti] {
			if !slices.Contains(got, sec) {
				t.Errorf("%s lists %v, missing %s", def.title, got, sec)
			}
		}
		// Sections keep the model's display order inside a tab.
		order := func(sec model.Section) int { return slices.Index(model.Sections, sec) }
		if !slices.IsSortedFunc(got, func(x, y model.Section) int { return order(x) - order(y) }) {
			t.Errorf("%s lists %v out of model.Sections order", def.title, got)
		}
	}
}

func TestTabSwitchingKeys(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 100, 40)
	if a.tab != tabMine {
		t.Fatalf("started on tab %d, want My PRs", a.tab)
	}
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#3" {
		t.Fatalf("My PRs selects %s", got)
	}

	steps := []struct {
		key  string
		tab  int
		name string
	}{
		{"tab", tabOthers, "tab goes to the next tab"},
		{"tab", tabMine, "tab wraps to the first tab"},
		{"shift+tab", tabOthers, "shift+tab wraps to the last tab"},
		{"shift+tab", tabMine, "shift+tab goes to the previous tab"},
		{"2", tabOthers, "2 jumps to Others' PRs"},
		{"2", tabOthers, "2 on Others' PRs stays"},
		{"1", tabMine, "1 jumps to My PRs"},
		{"1", tabMine, "1 on My PRs stays"},
	}
	for _, s := range steps {
		press(t, a, s.key)
		if a.tab != s.tab {
			t.Fatalf("%s: tab = %d, want %d", s.name, a.tab, s.tab)
		}
	}

	press(t, a, "2")
	if got := selectedKey(t, a); got != "acme-co/frontend#12" {
		t.Fatalf("Others' PRs selects %s", got)
	}
}

func TestTabKeysDoNotMoveCursorOrSelectDigits(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 100, 40)
	press(t, a, "3", "0", "9")
	if a.tab != tabMine || selectedKey(t, a) != "roehrijn/dev-inbox#3" {
		t.Fatalf("other digits changed state: tab %d, selected %s", a.tab, selectedKey(t, a))
	}
}

func TestTabsKeepCursorAndCollapseState(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 100, 40)

	press(t, a, "j") // My PRs: #4
	press(t, a, "2", "j", "space")
	if got := selectedKey(t, a); got != "header:review_requested" {
		t.Fatalf("Others' PRs after folding: %s", got)
	}
	press(t, a, "1")
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#4" {
		t.Fatalf("My PRs lost its cursor: %s", got)
	}
	if a.collapsed[model.SectionNeedsAction] || a.collapsed[model.SectionDrafts] {
		t.Fatal("folding on Others' PRs folded sections of My PRs")
	}
	press(t, a, "k", "h") // fold needs action
	if got := selectedKey(t, a); got != "header:needs_action" {
		t.Fatalf("My PRs after folding: %s", got)
	}

	press(t, a, "tab")
	if got := selectedKey(t, a); got != "header:review_requested" {
		t.Fatalf("Others' PRs lost its folded header: %s", got)
	}
	if a.collapsed[model.SectionInvolved] {
		t.Fatal("folding on My PRs folded sections of Others' PRs")
	}
	assertContains(t, screen(a), "▸ Review requested 2", "▾ Involved 1")
	press(t, a, "tab")
	assertContains(t, screen(a), "▸ Needs action 1", "▾ Your drafts 1")

	// Both tabs keep their selection when the list refreshes, also the inactive one.
	list := fixtureList()
	newer := item(prKey("x", "y", 1), model.SectionReviewRequested, "new", time.Minute)
	newer.Direct, newer.Score = true, 50
	above := item(prKey("x", "y", 2), model.SectionNeedsAction, "above", time.Minute)
	above.Score = 99
	list.Items = append(list.Items, newer, above)
	fb.list = list
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "header:needs_action" {
		t.Fatalf("My PRs after re-list: %s", got)
	}
	press(t, a, "2")
	if got := selectedKey(t, a); got != "header:review_requested" {
		t.Fatalf("Others' PRs after re-list: %s", got)
	}
	if !a.collapsed[model.SectionReviewRequested] || !a.collapsed[model.SectionNeedsAction] {
		t.Fatal("collapse state lost on re-list")
	}
}

func TestInactiveTabSelectionFollowsPRAcrossRelist(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 100, 40)
	press(t, a, "2", "j") // #11, second in review requested
	press(t, a, "1", "j") // #4
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#4" {
		t.Fatalf("setup: %s", got)
	}

	list := fixtureList()
	for i := range list.Items {
		if list.Items[i].PR.Key.Number == 11 {
			list.Items[i].Score = 30
		}
	}
	fb.list = list
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#4" {
		t.Fatalf("active tab moved to %s", got)
	}
	press(t, a, "2")
	if got := selectedKey(t, a); got != "acme-co/frontend#11" {
		t.Fatalf("inactive tab selected %s after its PR was rescored, want acme-co/frontend#11", got)
	}

	// The selected PR of the inactive tab disappears; its cursor stays in place.
	press(t, a, "1")
	list.Items = slices.DeleteFunc(list.Items, func(it api.InboxItem) bool { return it.PR.Key.Number == 11 })
	fb.list = list
	drain(t, a, update(t, a, keyPress("r")))
	press(t, a, "2")
	if got := selectedKey(t, a); got != "acme-co/frontend#12" {
		t.Fatalf("after removal selected %s", got)
	}
}

func TestTabsKeepScrollPosition(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 80, 8)
	press(t, a, "2", "G")
	assertContains(t, screen(a), "▾ Involved 1")
	if a.cur().offset == 0 {
		t.Fatal("setup: Others' PRs did not scroll")
	}

	press(t, a, "1")
	if a.cur().offset != 0 {
		t.Fatalf("My PRs inherited the scroll offset %d", a.cur().offset)
	}
	assertContains(t, screen(a), "▾ Needs action 1")

	press(t, a, "2")
	out := screen(a)
	assertFits(t, out, 80, 8)
	assertContains(t, out, "▾ Involved 1", "other/repo#99")
	if strings.Contains(out, "▾ Review requested") {
		t.Errorf("Others' PRs lost its scroll position:\n%s", out)
	}
}

func TestDetailFollowsActiveTab(t *testing.T) {
	list := fixtureList()
	a, fb := loadedApp(t, list, 140, 50)
	for _, it := range list.Items {
		fb.details[it.PR.Key] = it
	}
	mine, others := prKey("roehrijn", "dev-inbox", 3), prKey("acme-co", "frontend", 12)

	drain(t, a, update(t, a, keyPress("d")))
	assertContains(t, screen(a), "roehrijn/dev-inbox#3 · open · by roehrijn")
	if _, gets := fb.calls(); !slices.Equal(gets, []model.PRKey{mine}) {
		t.Fatalf("gets = %v", gets)
	}

	drain(t, a, update(t, a, keyPress("2")))
	out := screen(a)
	assertContains(t, out, "acme-co/frontend#12 · open · by alice")
	if strings.Contains(out, "roehrijn/dev-inbox#3 · open · by roehrijn") {
		t.Error("detail still shows the PR of the other tab")
	}
	if k, ok := a.detailKey(); !ok || k != others {
		t.Fatalf("detail key = %v, %v; want %v", k, ok, others)
	}
	if _, gets := fb.calls(); !slices.Equal(gets, []model.PRKey{mine, others}) {
		t.Fatalf("gets = %v", gets)
	}

	// Going back shows the cached PR without another fetch.
	drain(t, a, update(t, a, keyPress("shift+tab")))
	assertContains(t, screen(a), "roehrijn/dev-inbox#3 · open · by roehrijn")
	if _, gets := fb.calls(); len(gets) != 2 {
		t.Fatalf("cached detail fetched again: %v", gets)
	}

	// A folded header on the active tab shows the section, not a PR of the other tab.
	press(t, a, "2", "space")
	assertContains(t, screen(a), "Review requested", "2 pull requests folded")
}

func TestDetailOnEmptyTab(t *testing.T) {
	list := api.ListResponse{Items: []api.InboxItem{
		item(prKey("o", "r", 1), model.SectionNeedsAction, "mine", time.Hour),
	}}
	a, fb := loadedApp(t, list, 140, 30)
	fb.details[prKey("o", "r", 1)] = list.Items[0]
	drain(t, a, update(t, a, keyPress("d")))
	assertContains(t, screen(a), "o/r#1 · open")

	drain(t, a, update(t, a, keyPress("2")))
	out := screen(a)
	assertContains(t, out, "No pull request selected.")
	if strings.Contains(out, "o/r#1 · open") {
		t.Error("detail pane shows a PR of the other tab")
	}
	if _, ok := a.detailKey(); ok {
		t.Fatal("detail key on an empty tab")
	}
}

func TestDetailTogglesOnDNotTab(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 140, 40)
	press(t, a, "tab")
	if a.showDetail {
		t.Fatal("tab opened the detail pane")
	}
	press(t, a, "d")
	if !a.showDetail {
		t.Fatal("d did not open the detail pane")
	}
	press(t, a, "tab")
	if !a.showDetail || a.tab != tabMine {
		t.Fatalf("tab changed the detail pane (open: %v) or did not switch tabs (tab %d)", a.showDetail, a.tab)
	}
	press(t, a, "shift+tab", "1", "2")
	if !a.showDetail {
		t.Fatal("tab keys closed the detail pane")
	}
	press(t, a, "d")
	if a.showDetail {
		t.Fatal("d did not close the detail pane")
	}
}

func tabBarLine(t *testing.T, a *app) string {
	t.Helper()
	return strings.Split(screen(a), "\n")[1]
}

func TestTabBar(t *testing.T) {
	tests := []struct {
		width int
		want  string
	}{
		{120, " My PRs 2   Others' PRs 3 · needs me"},
		{36, " My PRs 2   Others' PRs 3 · needs me"},
		{35, " Mine 2   Others 3 · needs me"},
		{29, " Mine 2   Others 3 · needs me"},
		{18, " Mine 2   Others 3"},
		{10, " Mine 2   "},
	}
	for _, tt := range tests {
		a, _ := loadedApp(t, fixtureList(), tt.width, 20)
		out := screen(a)
		assertFits(t, out, tt.width, 20)
		if got := strings.TrimRight(tabBarLine(t, a), " "); got != strings.TrimRight(tt.want, " ") {
			t.Errorf("width %d: tab bar %q, want %q", tt.width, got, tt.want)
		}
	}
}

func TestTabBarCountsFollowTheList(t *testing.T) {
	a, fb := loadedApp(t, scoredList(0, 5, 10), 80, 20)
	if got := strings.TrimSpace(tabBarLine(t, a)); got != "My PRs 3   Others' PRs 0 · needs me" {
		t.Fatalf("tab bar %q", got)
	}

	fb.list = fixtureList()
	drain(t, a, update(t, a, keyPress("r")))
	if got := strings.TrimSpace(tabBarLine(t, a)); got != "My PRs 2   Others' PRs 3 · needs me" {
		t.Fatalf("tab bar after re-list %q", got)
	}
	if !strings.Contains(screen(a), "5 PRs") {
		t.Error("status bar total is not the overall count")
	}
	press(t, a, "2")
	if !strings.Contains(screen(a), "5 PRs") {
		t.Error("status bar total changed with the tab")
	}
}

func TestTabBarWithoutCountsBeforeFirstLoad(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	update(t, a, tea.WindowSizeMsg{Width: 80, Height: 20})
	assertFits(t, screen(a), 80, 20)
	if got := strings.TrimSpace(tabBarLine(t, a)); got != "My PRs   Others' PRs · needs me" {
		t.Fatalf("tab bar before the list loaded: %q", got)
	}
}

func TestTabBarHighlightsActiveTab(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 80, 20)
	if a.st.tabActive.Render("x") == a.st.muted.Render("x") {
		t.Fatal("active and inactive tab styles are indistinguishable")
	}
	active := func(label string) bool { return strings.Contains(a.View().Content, a.st.tabActive.Render(label)) }
	inactive := func(label string) bool { return strings.Contains(a.View().Content, a.st.muted.Render(label)) }

	if !active("My PRs 2") || !inactive("Others' PRs 3 · needs me") {
		t.Error("My PRs is not the highlighted tab")
	}
	press(t, a, "2")
	if !active("Others' PRs 3 · needs me") || !inactive("My PRs 2") {
		t.Error("Others' PRs is not the highlighted tab after switching")
	}
}

func TestEmptyTabMessage(t *testing.T) {
	list := api.ListResponse{Items: []api.InboxItem{
		item(prKey("o", "r", 1), model.SectionInvolved, "someone else's", time.Hour),
	}}
	a, _ := loadedApp(t, list, 80, 20)

	out := screen(a)
	assertFits(t, out, 80, 20)
	assertContains(t, out, tabDefs[tabMine].empty, "My PRs 0", "Others' PRs 1", "1 PR")
	if !strings.Contains(a.View().Content, a.st.muted.Render(tabDefs[tabMine].empty)) {
		t.Error("empty message is not dimmed")
	}
	if _, ok := a.selected(); ok {
		t.Fatal("selection on an empty tab")
	}
	press(t, a, "j", "k", "g", "G", "space", "h", "l", "enter", "y", "d")

	press(t, a, "2")
	out = screen(a)
	assertContains(t, out, "▾ Involved 1", "someone else's")
	if strings.Contains(out, tabDefs[tabMine].empty) || strings.Contains(out, tabDefs[tabOthers].empty) {
		t.Errorf("empty message shown on a tab with PRs:\n%s", out)
	}

	press(t, a, "1")
	assertContains(t, screen(a), tabDefs[tabMine].empty)
}
