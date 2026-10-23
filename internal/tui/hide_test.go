package tui

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nats-io/nats.go"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/model"
)

// hiddenItem is scoredItem, hidden until something new happens in it.
func hiddenItem(sec model.Section, n, score int, ago time.Duration) api.InboxItem {
	it := scoredItem(sec, n, score, ago)
	it.Hidden = true
	return it
}

// rrItem is review request o/r#n that needs the viewer, updated n hours ago.
func rrItem(n, score int, hidden bool) api.InboxItem {
	it := scoredItem(model.SectionReviewRequested, n, score, time.Duration(n)*time.Hour)
	it.NeedsMe, it.Hidden = true, hidden
	return it
}

func TestGroupItemsHiddenSections(t *testing.T) {
	items := []api.InboxItem{
		hiddenItem(model.SectionReviewRequested, 1, 10, time.Hour),
		hiddenItem(model.SectionReviewRequested, 2, -20, time.Hour),
		hiddenItem(model.SectionInvolved, 3, 0, 2*time.Hour),
		hiddenItem(model.SectionInvolved, 4, 0, time.Hour),
		hiddenItem(model.SectionNeedsAction, 5, -5, time.Hour),
		hiddenItem(model.SectionDrafts, 6, 30, time.Hour),
		hiddenItem(model.SectionNone, 7, 0, time.Hour),
		scoredItem(model.SectionReviewRequested, 8, -1, time.Hour),
		scoredItem(model.SectionReviewRequested, 9, 5, time.Hour),
	}
	tests := []struct {
		name string
		all  bool
		// rr lists review requested, rrHidden counts its PRs left out by the view or score.
		rr       []string
		rrHidden int
	}{
		{"needs me view", false, nil, 2},
		{"all view", true, []string{"o/r#9"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secs := groupItems(items, tt.all)
			var ids []model.Section
			for _, s := range secs {
				ids = append(ids, s.id)
			}
			if want := append(slices.Clone(model.Sections), sectionHiddenMine, sectionHiddenOthers); !slices.Equal(ids, want) {
				t.Fatalf("sections = %v, want %v", ids, want)
			}

			// Hidden PRs sort like any section, whatever their score, view or attention.
			want := map[model.Section][]string{
				sectionHiddenMine:   {"o/r#6", "o/r#5"},
				sectionHiddenOthers: {"o/r#1", "o/r#4", "o/r#3", "o/r#2"},
			}
			if tt.rr != nil {
				want[model.SectionReviewRequested] = tt.rr
			}
			if got := sectionKeys(secs); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
				t.Errorf("grouped = %v, want %v", got, want)
			}

			counts := map[model.Section]int{}
			for _, s := range secs {
				if s.hidden > 0 {
					counts[s.id] = s.hidden
				}
			}
			if want := map[model.Section]int{model.SectionReviewRequested: tt.rrHidden}; !maps.Equal(counts, want) {
				t.Errorf("hidden counts = %v, want %v", counts, want)
			}
		})
	}
}

func TestHiddenSectionsCollapsedByDefault(t *testing.T) {
	list := listOf(
		scoredItem(model.SectionNeedsAction, 1, 0, time.Hour),
		hiddenItem(model.SectionDrafts, 2, 0, time.Hour),
		hiddenItem(model.SectionWaitingForReview, 3, 0, 2*time.Hour),
		rrItem(4, 0, false),
		hiddenItem(model.SectionInvolved, 5, 0, time.Hour),
	)
	for _, w := range []int{120, 50} {
		a, _ := loadedApp(t, list, w, 30)
		out := screen(a)
		assertFits(t, out, w, 30)
		assertOrder(t, out, "My PRs 1", "▾ Needs action 1", "PR 1", "Waiting for review 0", "▸ Hidden until new activity 2")
		press(t, a, "2")
		out = screen(a)
		assertFits(t, out, w, 30)
		assertOrder(t, out, "Others' PRs 1", "▾ Review requested 1", "PR 4", "Involved 0", "▸ Hidden until new activity 1")
		for _, title := range []string{"PR 2", "PR 3", "PR 5"} {
			if strings.Contains(screen(a), title) {
				t.Errorf("width %d: folded hidden PR %q is listed", w, title)
			}
		}
	}

	a, fb := loadedApp(t, list, 100, 30)
	press(t, a, "j")
	if got := selectedKey(t, a); got != "header:"+string(sectionHiddenMine) {
		t.Fatalf("selected %s, want the folded hidden section", got)
	}
	press(t, a, "space")
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Fatalf("space selected %s, want the first hidden PR", got)
	}
	assertOrder(t, screen(a), "▾ Hidden until new activity 2", "PR 2", "PR 3")

	// The choice sticks across re-lists and tab switches, per tab.
	drain(t, a, update(t, a, keyPress("r")))
	if n, _ := fb.calls(); n != 2 {
		t.Fatalf("list calls = %d, want 2", n)
	}
	press(t, a, "2")
	assertContains(t, screen(a), "▸ Hidden until new activity 1")
	press(t, a, "G", "l")
	if got := selectedKey(t, a); got != "o/r#5" {
		t.Fatalf("l selected %s, want o/r#5", got)
	}
	press(t, a, "h")
	if got := selectedKey(t, a); got != "header:"+string(sectionHiddenOthers) {
		t.Fatalf("h selected %s, want the folded hidden section", got)
	}
	press(t, a, "o", "1")
	assertContains(t, screen(a), "▾ Hidden until new activity 2", "PR 2")
	if a.collapsed[sectionHiddenMine] || a.collapsed[sectionHiddenOthers] {
		t.Errorf("unfolded hidden sections folded again: %v", a.collapsed)
	}
}

func TestHiddenSectionIgnoresViewAndScore(t *testing.T) {
	needsMe := hiddenItem(model.SectionInvolved, 3, 5, time.Hour)
	needsMe.NeedsMe = true
	list := listOf(
		hiddenItem(model.SectionReviewRequested, 1, -30, time.Hour),
		hiddenItem(model.SectionInvolved, 2, 0, time.Hour),
		needsMe,
		scoredItem(model.SectionReviewRequested, 4, -1, time.Hour),
		scoredItem(model.SectionInvolved, 5, 0, time.Hour),
	)
	a, _ := loadedApp(t, list, 120, 40)
	press(t, a, "2", "G", "space")
	for _, view := range []struct {
		name                string
		tabBar, rr, involve string
	}{
		{"needs me", "Others' PRs 0 · needs me", "Review requested 0 · 1 hidden", "Involved 0 · 1 hidden"},
		{"all", "Others' PRs 1 · all", "Review requested 0 · 1 hidden", "▾ Involved 1"},
	} {
		out := screen(a)
		assertFits(t, out, 120, 40)
		assertOrder(t, out, view.tabBar, view.rr, view.involve, "▾ Hidden until new activity 3", "PR 3", "PR 2", "PR 1")
		if got, want := sectionKeys(a.sections)[sectionHiddenOthers], []string{"o/r#3", "o/r#2", "o/r#1"}; !slices.Equal(got, want) {
			t.Errorf("%s view: hidden section lists %v, want %v", view.name, got, want)
		}
		if strings.Contains(out, tabDefs[tabOthers].empty) {
			t.Errorf("%s view: empty message beside listed hidden PRs:\n%s", view.name, out)
		}
		press(t, a, "a")
	}
}

func TestOnlyHiddenPRsOnATab(t *testing.T) {
	list := listOf(
		hiddenItem(model.SectionNeedsAction, 1, 0, time.Hour),
		hiddenItem(model.SectionDrafts, 2, 0, time.Hour),
	)
	a, _ := loadedApp(t, list, 80, 20)
	out := screen(a)
	assertFits(t, out, 80, 20)
	assertContains(t, out, "My PRs 0", "▸ Hidden until new activity 2", "2 PRs")
	if strings.Contains(out, tabDefs[tabMine].empty) {
		t.Errorf("tab with hidden PRs claims to have none:\n%s", out)
	}
	if got := selectedKey(t, a); got != "header:"+string(sectionHiddenMine) {
		t.Errorf("selected %s, want the folded hidden section", got)
	}

	// An empty hidden section is not listed.
	press(t, a, "2")
	out = screen(a)
	assertContains(t, out, tabDefs[tabOthers].empty)
	if strings.Contains(out, "Hidden until new activity") {
		t.Errorf("empty hidden section listed:\n%s", out)
	}
}

func TestTabCountsLeaveOutHiddenSection(t *testing.T) {
	list := listOf(
		scoredItem(model.SectionNeedsAction, 1, 0, time.Hour),
		hiddenItem(model.SectionNeedsAction, 2, 0, time.Hour),
		hiddenItem(model.SectionDrafts, 3, 0, time.Hour),
		rrItem(4, 0, false),
		rrItem(5, 0, true),
	)
	a, _ := loadedApp(t, list, 100, 30)
	check := func(state string) {
		t.Helper()
		if got := strings.TrimSpace(tabBarLine(t, a)); got != "My PRs 1   Others' PRs 1 · needs me" {
			t.Errorf("%s: tab bar %q", state, got)
		}
		if got := a.total(); got != 5 {
			t.Errorf("%s: total = %d, want all 5 PRs", state, got)
		}
		assertContains(t, statusLine(a), "5 PRs")
	}
	check("folded")
	press(t, a, "G", "space", "2", "G", "space")
	assertContains(t, screen(a), "▾ Hidden until new activity 1", "PR 5")
	check("unfolded")
}

func TestHideKeySendsAnnotation(t *testing.T) {
	seen := testNow.Add(-time.Minute)
	pr := func(hidden bool) api.InboxItem {
		it := rrItem(1, 10, hidden)
		it.Direct = true
		it.PR.FetchedAt = seen
		it.PR.HeadSHA = "abc123"
		it.PR.IsDraft = true
		return it
	}
	tests := []struct {
		name   string
		hidden bool
		// keys select o/r#1 on the Others' PRs tab.
		keys []string
		// mark is the expected value; nil expects null, which unhides.
		mark  *derive.HiddenMark
		flash string
		// moveTo is the section the row is in right after the key press.
		moveTo model.Section
	}{
		{
			name:   "hide",
			keys:   []string{"2"},
			mark:   &derive.HiddenMark{Seen: seen, Head: "abc123", Draft: true, Requested: true},
			flash:  "hid o/r#1 until something new",
			moveTo: sectionHiddenOthers,
		},
		{
			name:   "unhide",
			hidden: true,
			keys:   []string{"2", "G", "space"},
			flash:  "unhid o/r#1",
			moveTo: model.SectionReviewRequested,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, fb := loadedApp(t, listOf(pr(tt.hidden), rrItem(2, 0, false)), 120, 30)
			press(t, a, tt.keys...)
			if got := selectedKey(t, a); got != "o/r#1" {
				t.Fatalf("setup: selected %s", got)
			}

			cmd := update(t, a, keyPress("x"))
			if n := len(fb.annotations()); n != 0 {
				t.Fatalf("Update sent %d annotations itself instead of in a command", n)
			}
			if got := sectionKeys(a.sections)[tt.moveTo]; !slices.Contains(got, "o/r#1") {
				t.Errorf("row did not move to %s at once: %v", tt.moveTo, sectionKeys(a.sections))
			}
			drain(t, a, cmd)

			anns := fb.annotations()
			if len(anns) != 1 {
				t.Fatalf("annotations = %+v, want one", anns)
			}
			ann := anns[0]
			if ann.PR != prKey("o", "r", 1) || ann.Key != derive.HiddenKey || ann.CommentID != "" {
				t.Errorf("annotation = %+v, want PR-level %s on o/r#1", ann, derive.HiddenKey)
			}
			if tt.mark == nil {
				if string(ann.Value) != "null" {
					t.Errorf("value = %s, want null", ann.Value)
				}
			} else {
				var m derive.HiddenMark
				if err := json.Unmarshal(ann.Value, &m); err != nil {
					t.Fatalf("value %s: %v", ann.Value, err)
				}
				if !m.Seen.Equal(tt.mark.Seen) || m.Head != tt.mark.Head || m.Draft != tt.mark.Draft || m.Requested != tt.mark.Requested {
					t.Errorf("mark = %+v, want %+v", m, *tt.mark)
				}
			}
			assertContains(t, statusLine(a), tt.flash)
			if a.flashErr {
				t.Error("success flashed as an error")
			}
		})
	}
}

func TestHideKeyFailures(t *testing.T) {
	t.Run("daemon error", func(t *testing.T) {
		a, fb := loadedApp(t, listOf(rrItem(1, 10, false), rrItem(2, 0, false)), 120, 30)
		fb.mu.Lock()
		fb.annErr = errors.New("pr o/r#1 is not tracked")
		fb.mu.Unlock()
		press(t, a, "2")
		before, _ := fb.calls()
		drain(t, a, update(t, a, keyPress("x")))
		assertContains(t, statusLine(a), "hide o/r#1: pr o/r#1 is not tracked")
		if !a.flashErr {
			t.Error("error not flashed as such")
		}
		// A re-list puts the row back where the daemon has it.
		if n, _ := fb.calls(); n != before+1 {
			t.Errorf("list calls = %d, want %d", n, before+1)
		}
		if got := sectionKeys(a.sections)[model.SectionReviewRequested]; !slices.Equal(got, []string{"o/r#1", "o/r#2"}) {
			t.Errorf("review requested = %v after the failure, want both PRs back", got)
		}
	})

	t.Run("disconnected", func(t *testing.T) {
		a, fb := loadedApp(t, listOf(rrItem(1, 10, false)), 120, 30)
		press(t, a, "2")
		update(t, a, connMsg{connected: false})
		if cmd := update(t, a, keyPress("x")); cmd == nil {
			t.Fatal("x while disconnected should flash a message")
		}
		assertContains(t, statusLine(a), "not connected to the daemon")
		if !a.flashErr {
			t.Error("error not flashed as such")
		}
		if anns := fb.annotations(); len(anns) != 0 {
			t.Errorf("annotated while disconnected: %+v", anns)
		}
		if got := selectedKey(t, a); got != "o/r#1" {
			t.Errorf("row moved although nothing was sent: selected %s", got)
		}
	})

	t.Run("folded header", func(t *testing.T) {
		a, fb := loadedApp(t, listOf(rrItem(1, 10, true)), 120, 30)
		press(t, a, "2")
		drain(t, a, update(t, a, keyPress("x")))
		if anns := fb.annotations(); len(anns) != 0 {
			t.Errorf("x on a section header annotated %+v", anns)
		}
		if a.flash != "" {
			t.Errorf("x on a section header flashed %q", a.flash)
		}
	})
}

func TestHideMovesCursorToNearestPR(t *testing.T) {
	a, fb := loadedApp(t, listOf(rrItem(1, 40, false), rrItem(2, 30, false), rrItem(3, 20, false), rrItem(4, 10, false)), 120, 40)
	toggle := func(want string) {
		t.Helper()
		drain(t, a, update(t, a, keyPress("x")))
		if got := selectedKey(t, a); got != want {
			t.Errorf("after x: selected %s, want %s", got, want)
		}
	}

	press(t, a, "2", "j")
	toggle("o/r#3") // the PR below takes over
	press(t, a, "j")
	toggle("o/r#3") // nothing below the last PR, the one above takes over

	// Unhiding in the unfolded hidden section keeps the cursor there.
	press(t, a, "G", "space")
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Fatalf("setup: selected %s", got)
	}
	toggle("o/r#4")

	// Hiding with the hidden section unfolded does not follow the PR into it.
	press(t, a, "g")
	toggle("o/r#2")

	want := map[model.Section][]string{
		model.SectionReviewRequested: {"o/r#2", "o/r#3"},
		sectionHiddenOthers:          {"o/r#1", "o/r#4"},
	}
	if got := sectionKeys(a.sections); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("sections = %v, want %v", got, want)
	}
	var sent []string
	for _, ann := range fb.annotations() {
		sent = append(sent, ann.PR.String()+"="+map[bool]string{true: "unhide", false: "hide"}[string(ann.Value) == "null"])
	}
	if want := []string{"o/r#2=hide", "o/r#4=hide", "o/r#2=unhide", "o/r#1=hide"}; !slices.Equal(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}

	// The daemon's list agrees; the cursor stays.
	fb.mu.Lock()
	fb.list = listOf(rrItem(1, 40, true), rrItem(2, 30, false), rrItem(3, 20, false), rrItem(4, 10, true))
	fb.mu.Unlock()
	drain(t, a, update(t, a, keyPress("r")))
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Errorf("after the re-list: selected %s, want o/r#2", got)
	}
}

func TestSelectionAcrossHiddenSectionOnRelist(t *testing.T) {
	a, fb := loadedApp(t, listOf(rrItem(1, 30, false), rrItem(2, 20, false), rrItem(3, 10, false)), 120, 40)
	relist := func(hidden2 bool) {
		t.Helper()
		fb.mu.Lock()
		fb.list = listOf(rrItem(1, 30, false), rrItem(2, 20, hidden2), rrItem(3, 10, false))
		fb.mu.Unlock()
		drain(t, a, update(t, a, keyPress("r")))
	}
	press(t, a, "2", "j")

	// Hidden elsewhere: the folded section takes it out of reach, the nearest PR takes over.
	relist(true)
	if got := selectedKey(t, a); got != "o/r#3" {
		t.Errorf("selected %s, want o/r#3", got)
	}

	// Something new happens: the PR leaves the unfolded hidden section and the cursor follows it.
	press(t, a, "G", "space")
	if got := selectedKey(t, a); got != "o/r#2" {
		t.Fatalf("setup: selected %s", got)
	}
	relist(false)
	e, _ := a.selected()
	if got := selectedKey(t, a); got != "o/r#2" || a.sections[e.sec].id != model.SectionReviewRequested {
		t.Errorf("selected %s in %s, want o/r#2 in review requested", got, a.sections[e.sec].id)
	}
}

func TestHiddenMarkIsNoBadgeNorListedAnnotation(t *testing.T) {
	it := rrItem(1, 0, true)
	it.PR.HeadSHA = "abc123"
	mark := derive.HiddenAnnotation(it.PR, it.Classification)
	tests := []struct {
		name string
		anns []model.Annotation
		want []string
	}{
		{"only the mark", []model.Annotation{mark}, nil},
		{"with other annotations", []model.Annotation{
			mark,
			badgeAnn(it.PR.Key, "linear.id", `"DEVOPS-42"`),
			badgeAnn(it.PR.Key, "linear.urgency", `{"badge":"urgent"}`),
		}, []string{"Annotations", "linear.id", `"DEVOPS-42"`, "linear.urgency", "urgent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestApp(&fakeBackend{})
			it.Annotations = tt.anns
			out := ansi.Strip(a.renderPR(it, true, nil, 100))
			if tt.want == nil && strings.Contains(out, "Annotations") {
				t.Errorf("annotations listed for the mark alone:\n%s", out)
			}
			assertOrder(t, out, tt.want...)
			for _, s := range []string{derive.HiddenKey, "abc123", `"seen"`} {
				if strings.Contains(out, s) {
					t.Errorf("detail shows %q of the mark:\n%s", s, out)
				}
			}
		})
	}

	a := newTestApp(&fakeBackend{})
	if got := prBadges([]model.Annotation{mark}); len(got) != 0 {
		t.Errorf("mark renders badges %+v", got)
	}
	bare := it
	bare.Annotations = nil
	it.Annotations = []model.Annotation{mark}
	l1 := a.renderRow(it, 0, false, 100)[0]
	l0 := a.renderRow(bare, 0, false, 100)[0]
	if l1 != l0 {
		t.Errorf("mark changes the row:\n%q\nwant\n%q", ansi.Strip(l1), ansi.Strip(l0))
	}
}

func TestHideKeyInHelp(t *testing.T) {
	k := newKeyMap()
	if !key.Matches(keyPress("x"), k.Hide) {
		t.Fatal("x does not match the hide binding")
	}
	isHide := func(b key.Binding) bool { return b.Help().Key == "x" }
	if !slices.ContainsFunc(k.ShortHelp(), isHide) {
		t.Error("hide missing from the short help")
	}
	var group []string
	for _, g := range k.FullHelp() {
		if slices.ContainsFunc(g, isHide) {
			for _, b := range g {
				group = append(group, b.Help().Key)
			}
		}
	}
	if len(group) < 4 || !slices.Equal(group[:4], []string{"o", "O", "y", "x"}) {
		t.Errorf("full help column %v, want hide right after open, open link and copy", group)
	}

	a, _ := loadedApp(t, fixtureList(), 120, 30)
	press(t, a, "?")
	assertContains(t, screen(a), "hide until new")
}

// serveAnnotate answers annotate requests like the daemon, failing for PR number 404, and
// passes every annotation it receives to the returned channel.
func serveAnnotate(t *testing.T, url string) <-chan model.Annotation {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	got := make(chan model.Annotation, 8)
	if _, err := nc.Subscribe(api.SubjectAnnotate, func(m *nats.Msg) {
		var req api.AnnotateRequest
		var resp api.AnnotateResponse
		if err := json.Unmarshal(m.Data, &req); err != nil {
			resp.Error = "bad request: " + err.Error()
		} else if req.Annotation.PR.Number == 404 {
			resp.Error = "pr " + req.Annotation.PR.String() + " is not tracked"
		}
		got <- req.Annotation
		data, err := json.Marshal(resp)
		if err != nil {
			t.Error(err)
			return
		}
		if err := m.Respond(data); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return got
}

func receive(t *testing.T, ch <-chan model.Annotation) model.Annotation {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("no annotate request received")
		return model.Annotation{}
	}
}

func TestClientAnnotate(t *testing.T) {
	s := startNATS(t)
	got := serveAnnotate(t, s.ClientURL())
	c := connectedClient(t, s.ClientURL())

	pr := model.PR{Key: prKey("o", "r", 1), PRStatus: model.PRStatus{HeadSHA: "abc123"}, FetchedAt: testNow}
	tests := []struct {
		name    string
		ann     model.Annotation
		wantErr string
	}{
		{"hide", derive.HiddenAnnotation(pr, model.Classification{Direct: true}), ""},
		{"unhide", model.Annotation{PR: pr.Key, Key: derive.HiddenKey, Value: json.RawMessage("null")}, ""},
		{"daemon error", model.Annotation{PR: prKey("o", "r", 404), Key: derive.HiddenKey, Value: json.RawMessage("null")},
			"pr o/r#404 is not tracked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.Annotate(context.Background(), tt.ann)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Annotate: %v", err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("Annotate error = %v, want %q", err, tt.wantErr)
			}
			ann := receive(t, got)
			if !reflect.DeepEqual(ann, roundTrip(t, tt.ann)) || string(ann.Value) != string(tt.ann.Value) {
				t.Errorf("daemon received %+v (value %s), want %+v (value %s)", ann, ann.Value, tt.ann, tt.ann.Value)
			}
		})
	}
}

func TestAppHidesAgainstEmbeddedNATS(t *testing.T) {
	s := startNATS(t)
	serveFixtures(t, s.ClientURL(), fixtureList())
	got := serveAnnotate(t, s.ClientURL())
	c := connectedClient(t, s.ClientURL())

	a := newTestApp(c)
	update(t, a, tea.WindowSizeMsg{Width: 140, Height: 40})
	drain(t, a, update(t, a, connMsg{connected: true}))
	drain(t, a, update(t, a, keyPress("x")))

	ann := receive(t, got)
	if ann.PR != prKey("roehrijn", "dev-inbox", 3) || ann.Key != derive.HiddenKey {
		t.Errorf("daemon received %+v, want %s on roehrijn/dev-inbox#3", ann, derive.HiddenKey)
	}
	assertContains(t, statusLine(a), "hid roehrijn/dev-inbox#3 until something new")
}
