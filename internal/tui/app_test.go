package tui

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

func sectionKeys(secs []section) map[model.Section][]string {
	out := map[model.Section][]string{}
	for _, s := range secs {
		for _, it := range s.items {
			out[s.id] = append(out[s.id], it.PR.Key.String())
		}
	}
	return out
}

func TestGroupItems(t *testing.T) {
	k := func(n int) model.PRKey { return prKey("o", "r", n) }
	direct := func(it api.InboxItem) api.InboxItem { it.Direct, it.Score = true, 10; return it }
	items := []api.InboxItem{
		item(k(1), model.SectionReviewRequested, "team, old", 5*time.Hour),
		item(k(2), model.SectionReviewRequested, "team, new", time.Hour),
		direct(item(k(3), model.SectionReviewRequested, "direct, oldest", 48*time.Hour)),
		direct(item(k(4), model.SectionReviewRequested, "direct, newer", 2*time.Hour)),
		item(k(5), model.SectionInvolved, "involved", time.Minute),
		item(k(6), model.SectionNone, "closed", time.Minute),
		item(k(7), "bogus", "unknown section", time.Minute),
		item(k(8), model.SectionNeedsAction, "needs action", time.Minute),
	}
	secs := groupItems(items)

	var ids []model.Section
	for _, s := range secs {
		ids = append(ids, s.id)
	}
	if !slices.Equal(ids, model.Sections) {
		t.Fatalf("sections = %v, want %v", ids, model.Sections)
	}
	got := sectionKeys(secs)
	want := map[model.Section][]string{
		model.SectionReviewRequested: {"o/r#4", "o/r#3", "o/r#2", "o/r#1"},
		model.SectionNeedsAction:     {"o/r#8"},
		model.SectionInvolved:        {"o/r#5"},
	}
	if len(got) != len(want) {
		t.Fatalf("grouped = %v, want %v", got, want)
	}
	for sec, keys := range want {
		if !slices.Equal(got[sec], keys) {
			t.Errorf("%s = %v, want %v", sec, got[sec], keys)
		}
	}
}

func TestGroupItemsOrdersByScoreThenUpdatedAt(t *testing.T) {
	k := func(n int) model.PRKey { return prKey("o", "r", n) }
	scored := func(n, score int, ago time.Duration) api.InboxItem {
		it := item(k(n), model.SectionNeedsAction, "pr", ago)
		it.Score = score
		return it
	}
	flagged := scored(3, 0, 48*time.Hour)
	flagged.Direct = true

	tests := []struct {
		name  string
		items []api.InboxItem
		want  []string
	}{
		{
			name:  "higher score beats recency",
			items: []api.InboxItem{scored(1, 0, time.Minute), scored(2, 30, 72*time.Hour), scored(3, 10, time.Hour)},
			want:  []string{"o/r#2", "o/r#3", "o/r#1"},
		},
		{
			name:  "negative scores sort last",
			items: []api.InboxItem{scored(1, -15, time.Minute), scored(2, 0, 72*time.Hour), scored(3, -30, time.Hour)},
			want:  []string{"o/r#2", "o/r#1", "o/r#3"},
		},
		{
			name:  "equal scores fall back to the latest update",
			items: []api.InboxItem{scored(1, 20, 5*time.Hour), scored(2, 20, time.Hour), scored(3, 20, 48*time.Hour)},
			want:  []string{"o/r#2", "o/r#1", "o/r#3"},
		},
		{
			name:  "equal scores and times fall back to the key",
			items: []api.InboxItem{scored(2, 5, time.Hour), scored(10, 5, time.Hour), scored(1, 5, time.Hour)},
			want:  []string{"o/r#1", "o/r#10", "o/r#2"},
		},
		{
			name:  "the direct flag alone does not move an item",
			items: []api.InboxItem{scored(1, 0, 5*time.Hour), flagged, scored(2, 0, time.Hour)},
			want:  []string{"o/r#2", "o/r#1", "o/r#3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sectionKeys(groupItems(tt.items))[model.SectionNeedsAction]
			if !slices.Equal(got, tt.want) {
				t.Errorf("order = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseBadge(t *testing.T) {
	tests := []struct {
		value     string
		ok        bool
		text, lvl string
	}{
		{`{"badge":"urgent","level":"crit"}`, true, "urgent", "crit"},
		{`{"badge":"P2","level":"warn"}`, true, "P2", "warn"},
		{`{"badge":"fyi"}`, true, "fyi", "info"},
		{`{"badge":"odd","level":"loud"}`, true, "odd", "info"},
		{`{"badge":"","level":"crit"}`, false, "", ""},
		{`{"badge":42}`, false, "", ""},
		{`{"level":"crit"}`, false, "", ""},
		{`"urgent"`, false, "", ""},
		{`["urgent"]`, false, "", ""},
		{`null`, false, "", ""},
		{``, false, "", ""},
	}
	for _, tt := range tests {
		b, ok := parseBadge(model.Annotation{Key: "k", Value: json.RawMessage(tt.value)})
		if ok != tt.ok || b.text != tt.text || b.level != tt.lvl {
			t.Errorf("parseBadge(%s) = %+v, %v; want text %q level %q, %v", tt.value, b, ok, tt.text, tt.lvl, tt.ok)
		}
	}
}

func TestPRBadgesSkipCommentAnnotationsAndSortByKey(t *testing.T) {
	k := prKey("o", "r", 1)
	anns := []model.Annotation{
		badgeAnn(k, "z.last", `{"badge":"z"}`),
		badgeAnn(k, "a.first", `{"badge":"a","level":"warn"}`),
		badgeAnn(k, "m.raw", `{"other":true}`),
		{PR: k, CommentID: "c1", Key: "b.comment", Value: json.RawMessage(`{"badge":"c"}`)},
	}
	got := prBadges(anns)
	want := []badge{{key: "a.first", text: "a", level: "warn"}, {key: "z.last", text: "z", level: "info"}}
	if !slices.Equal(got, want) {
		t.Fatalf("prBadges = %+v, want %+v", got, want)
	}
}

func TestNavigationAcrossSections(t *testing.T) {
	tests := []struct {
		name  string
		tab   string
		order []string
	}{
		{"my PRs", "1", []string{
			"roehrijn/dev-inbox#3", // needs action; ready to merge is empty and skipped
			"roehrijn/dev-inbox#4", // drafts; waiting for review is empty and skipped
		}},
		{"others' PRs", "2", []string{
			"acme-co/frontend#12", "acme-co/frontend#11", // review requested: higher score first
			"other/repo#99", // involved
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := loadedApp(t, fixtureList(), 100, 40)
			press(t, a, tt.tab)
			order := tt.order
			last := len(order) - 1
			for i, want := range order {
				if got := selectedKey(t, a); got != want {
					t.Fatalf("step %d: selected %s, want %s", i, got, want)
				}
				press(t, a, "j")
			}
			if got := selectedKey(t, a); got != order[last] {
				t.Fatalf("j past the end moved to %s", got)
			}
			press(t, a, "k", "up")
			if got := selectedKey(t, a); got != order[max(last-2, 0)] {
				t.Fatalf("after k, up: %s, want %s", got, order[max(last-2, 0)])
			}
			press(t, a, "g")
			if got := selectedKey(t, a); got != order[0] {
				t.Fatalf("after g: %s", got)
			}
			press(t, a, "k")
			if got := selectedKey(t, a); got != order[0] {
				t.Fatalf("k before the start moved to %s", got)
			}
			press(t, a, "G")
			if got := selectedKey(t, a); got != order[last] {
				t.Fatalf("after G: %s", got)
			}
			press(t, a, "down")
			if got := selectedKey(t, a); got != order[last] {
				t.Fatalf("down past the end moved to %s", got)
			}
		})
	}
}

func TestCollapseAndExpand(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 100, 40)
	press(t, a, "space")
	if got := selectedKey(t, a); got != "header:needs_action" {
		t.Fatalf("after collapsing: %s", got)
	}
	if !a.collapsed[model.SectionNeedsAction] {
		t.Fatal("section not collapsed")
	}
	press(t, a, "j")
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#4" {
		t.Fatalf("j from collapsed header: %s", got)
	}
	press(t, a, "k", "l")
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#3" {
		t.Fatalf("l on header should expand to the first row, got %s", got)
	}

	press(t, a, "h") // collapse needs action from its row
	if got := selectedKey(t, a); got != "header:needs_action" {
		t.Fatalf("h on row: %s", got)
	}
	press(t, a, "h") // no-op on a header
	if got := selectedKey(t, a); got != "header:needs_action" {
		t.Fatalf("h on header: %s", got)
	}
	press(t, a, "enter")
	if a.collapsed[model.SectionNeedsAction] || selectedKey(t, a) != "roehrijn/dev-inbox#3" {
		t.Fatalf("enter on header should expand, got %s", selectedKey(t, a))
	}
	press(t, a, "l") // no-op on a row
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#3" {
		t.Fatalf("l on row: %s", got)
	}

	press(t, a, "space")
	out := screen(a)
	assertContains(t, out, "▸ Needs action 1")
	if strings.Contains(out, "Fix the flaky webhook") {
		t.Errorf("collapsed section still shows its rows:\n%s", out)
	}
}

func TestSelectionFollowsPRAcrossRelist(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 100, 40)
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#3" {
		t.Fatalf("setup: %s", got)
	}

	// A new PR with a higher score is inserted above the selection.
	list := fixtureList()
	newer := item(prKey("x", "y", 1), model.SectionNeedsAction, "new", time.Minute)
	newer.Score = 50
	list.Items = append(list.Items, newer)
	fb.list = list
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#3" {
		t.Fatalf("selection moved to %s after re-list", got)
	}

	// The selected PR disappears; the cursor stays in place.
	list.Items = slices.DeleteFunc(list.Items, func(it api.InboxItem) bool { return it.PR.Key.Number == 3 })
	fb.list = list
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "roehrijn/dev-inbox#4" {
		t.Fatalf("after removal selected %s, want the next PR", got)
	}

	// Everything disappears.
	fb.list = api.ListResponse{Viewer: list.Viewer}
	drain(t, a, update(t, a, keyPress("r")))
	if _, ok := a.selected(); ok {
		t.Fatal("selection without items")
	}
	press(t, a, "j", "G", "space", "enter", "y", "2", "j", "G", "space", "enter", "y")
}

func TestSelectionFollowsPRWhenItsScoreReorders(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 100, 40)
	press(t, a, "2", "j") // acme-co/frontend#11, second in review requested
	if got := selectedKey(t, a); got != "acme-co/frontend#11" {
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
	if got := sectionKeys(a.sections)[model.SectionReviewRequested][0]; got != "acme-co/frontend#11" {
		t.Fatalf("setup: first review request is %s, want the rescored PR", got)
	}
	if got := selectedKey(t, a); got != "acme-co/frontend#11" {
		t.Fatalf("selection moved to %s after the PR changed position", got)
	}
	if got := a.cur().cursor; got != 0 {
		t.Fatalf("cursor = %d, want 0 for the PR now at the top", got)
	}
}

func TestCollapsedHeaderSelectionSurvivesRelist(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 100, 40)
	press(t, a, "space")
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "header:needs_action" {
		t.Fatalf("selected %s", got)
	}
	if fb.listCalls != 2 {
		t.Fatalf("list calls = %d, want 2", fb.listCalls)
	}
}

func TestEventsAreDebouncedIntoOneRelist(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 100, 40)
	k := prKey("acme-co", "frontend", 12)

	first := update(t, a, eventMsg{key: &k})
	if first == nil {
		t.Fatal("first event did not schedule a re-list")
	}
	for range 5 {
		if cmd := update(t, a, eventMsg{key: &k}); cmd != nil {
			t.Fatal("events during the debounce window must not schedule another re-list")
		}
	}
	if cmd := update(t, a, eventMsg{}); cmd != nil {
		t.Fatal("undecodable event during the debounce window scheduled a re-list")
	}

	msgs := drain(t, a, first)
	if len(msgs) != 2 {
		t.Fatalf("messages = %#v, want relistMsg and listMsg", msgs)
	}
	if _, ok := msgs[0].(relistMsg); !ok {
		t.Fatalf("first message %T, want relistMsg", msgs[0])
	}
	if n, _ := fb.calls(); n != 2 {
		t.Fatalf("list calls = %d, want initial + 1", n)
	}

	// After the window closed the next event schedules a new re-list.
	drain(t, a, update(t, a, eventMsg{key: &k}))
	if n, _ := fb.calls(); n != 3 {
		t.Fatalf("list calls = %d, want 3", n)
	}
}

func TestEventRefreshesOpenDetail(t *testing.T) {
	list := fixtureList()
	a, fb := loadedApp(t, list, 140, 40)
	sel := prKey("acme-co", "frontend", 12)
	other := prKey("acme-co", "frontend", 11)
	for _, it := range list.Items {
		fb.details[it.PR.Key] = it
	}

	press(t, a, "2")
	drain(t, a, update(t, a, keyPress("d")))
	if _, gets := fb.calls(); !slices.Equal(gets, []model.PRKey{sel}) {
		t.Fatalf("gets after opening detail = %v", gets)
	}
	// Visit the other PR so it is cached, then come back.
	drain(t, a, update(t, a, keyPress("j")))
	drain(t, a, update(t, a, keyPress("k")))
	if _, gets := fb.calls(); len(gets) != 2 {
		t.Fatalf("cached detail fetched again: %v", gets)
	}

	drain(t, a, update(t, a, eventMsg{key: &other}))
	if _, gets := fb.calls(); len(gets) != 2 {
		t.Fatalf("event for another PR refreshed the open detail: %v", gets)
	}
	if a.details[other] != nil {
		t.Fatal("event did not invalidate the cached detail of its PR")
	}

	drain(t, a, update(t, a, eventMsg{key: &sel}))
	if _, gets := fb.calls(); len(gets) != 3 || gets[2] != sel {
		t.Fatalf("event for the open PR did not refresh it: %v", gets)
	}
	if a.details[sel] == nil {
		t.Fatal("open detail dropped from cache")
	}
}

func TestDetailNotFetchedWhileClosed(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 100, 40)
	k := prKey("roehrijn", "dev-inbox", 3)
	drain(t, a, update(t, a, eventMsg{key: &k}))
	if _, gets := fb.calls(); len(gets) != 0 {
		t.Fatalf("detail fetched while the pane is closed: %v", gets)
	}
}

func TestListErrorBeforeFirstLoad(t *testing.T) {
	fb := &fakeBackend{listErr: errors.New("boom: store locked")}
	a := newTestApp(fb)
	update(t, a, tea.WindowSizeMsg{Width: 80, Height: 20})

	cmd := update(t, a, connMsg{connected: true})
	out := runCmd(t, cmd)
	retry := update(t, a, out)
	if retry == nil {
		t.Fatal("failed list did not schedule a retry")
	}
	assertContains(t, screen(a), "Could not load the inbox", "boom: store locked")

	fb.mu.Lock()
	fb.listErr = nil
	fb.list = fixtureList()
	fb.mu.Unlock()
	drain(t, a, retry)
	if !a.loaded || a.listErr != nil {
		t.Fatalf("retry did not load the inbox: loaded=%v err=%v", a.loaded, a.listErr)
	}
	assertContains(t, screen(a), "Needs action 1")
}

func TestListErrorAfterLoadKeepsData(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 120, 30)
	fb.mu.Lock()
	fb.listErr = errors.New("decode failed")
	fb.mu.Unlock()
	update(t, a, runCmd(t, update(t, a, keyPress("r"))))
	out := screen(a)
	assertContains(t, out, "refresh failed: decode failed", "Fix the flaky webhook")
}

func TestDisconnected(t *testing.T) {
	fb := &fakeBackend{list: fixtureList()}
	a := newTestApp(fb)
	update(t, a, tea.WindowSizeMsg{Width: 100, Height: 20})
	assertContains(t, screen(a), "Daemon not reachable at "+api.DefaultNATSURL, "dev-inbox serve")

	if cmd := update(t, a, keyPress("r")); cmd == nil {
		t.Fatal("r while disconnected should flash a message")
	}
	if fb.listCalls != 0 {
		t.Fatal("listed while disconnected")
	}
	assertContains(t, screen(a), "not connected to the daemon")

	drain(t, a, update(t, a, connMsg{connected: true}))
	update(t, a, connMsg{connected: false})
	out := screen(a)
	assertContains(t, out, "Fix the flaky webhook", "daemon not reachable at "+api.DefaultNATSURL)
}

func TestReconnectRelists(t *testing.T) {
	a, fb := loadedApp(t, fixtureList(), 100, 40)
	update(t, a, connMsg{connected: false})
	drain(t, a, update(t, a, connMsg{connected: true}))
	if fb.listCalls != 2 {
		t.Fatalf("list calls = %d, want 2", fb.listCalls)
	}
}

func TestStaleListResponseIgnored(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 100, 40)
	update(t, a, listMsg{seq: a.appliedSeq + 2, resp: fixtureList(), at: testNow})
	update(t, a, listMsg{seq: a.appliedSeq - 1, resp: api.ListResponse{}, at: testNow})
	if a.total() != 5 {
		t.Fatalf("older response overwrote newer one: total %d", a.total())
	}
}

func TestOpenAndCopy(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 120, 40)
	var opened []string
	a.open = func(u string) error { opened = append(opened, u); return nil }

	drain(t, a, update(t, a, keyPress("enter")))
	drain(t, a, update(t, a, keyPress("o")))
	want := "https://github.com/roehrijn/dev-inbox/pull/3"
	if !slices.Equal(opened, []string{want, want}) {
		t.Fatalf("opened %v", opened)
	}

	a.open = func(string) error { return errors.New("no browser") }
	drain(t, a, update(t, a, keyPress("o")))
	assertContains(t, screen(a), "open browser: no browser")

	if cmd := update(t, a, keyPress("y")); cmd == nil {
		t.Fatal("y returned no command")
	}
	assertContains(t, screen(a), "copied "+want)
}

func TestPRURLFallback(t *testing.T) {
	pr := model.PR{Key: prKey("o", "r", 7)}
	if got := prURL(pr); got != "https://github.com/o/r/pull/7" {
		t.Fatalf("prURL = %s", got)
	}
}

func TestOpenURLRejectsNonHTTP(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "--help", "javascript:alert(1)"} {
		if err := openURL(u); err == nil {
			t.Errorf("openURL(%q) succeeded", u)
		}
	}
}

func TestQuit(t *testing.T) {
	a, _ := loadedApp(t, fixtureList(), 100, 40)
	for _, k := range []string{"q", "ctrl+c"} {
		cmd := update(t, a, keyPress(k))
		if _, ok := runCmd(t, cmd).(tea.QuitMsg); !ok {
			t.Errorf("%s did not quit", k)
		}
	}
}

func TestRelAge(t *testing.T) {
	tests := []struct {
		ago  time.Duration
		want string
	}{
		{10 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
		{70 * 24 * time.Hour, "2mo ago"},
		{800 * 24 * time.Hour, "2y ago"},
	}
	for _, tt := range tests {
		if got := relAge(testNow, testNow.Add(-tt.ago)); got != tt.want {
			t.Errorf("relAge(%s) = %q, want %q", tt.ago, got, tt.want)
		}
	}
	if got := relAge(testNow, time.Time{}); got != "" {
		t.Errorf("relAge(zero) = %q", got)
	}
}

// runCmd runs a command expected to finish immediately and returns its message.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil command")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				if m := runImmediate(c); m != nil {
					return m
				}
			}
		}
		return nil
	}
	return msg
}

// runImmediate returns the message of c if it completes within a short wait.
func runImmediate(c tea.Cmd) tea.Msg {
	done := make(chan tea.Msg, 1)
	go func() { done <- c() }()
	select {
	case m := <-done:
		return m
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}
