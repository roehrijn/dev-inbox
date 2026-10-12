package tui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	issue1 = "https://linear.app/acme/issue/ABC-1/fix-login"
	issue2 = "https://linear.app/acme/issue/ABC-2/add-sso"
	issue3 = "https://linear.app/acme/issue/ABC-3/drop-v1"
)

func linksAnn(k model.PRKey, annKey string, v any) model.Annotation {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return model.Annotation{PR: k, Key: annKey, Value: data}
}

type linksValue struct {
	Label string           `json:"label,omitempty"`
	Links []map[string]any `json:"links"`
}

func linkOf(id, title, url, detail string) map[string]any {
	l := map[string]any{"id": id, "title": title, "detail": detail}
	if url != "" {
		l["url"] = url
	}
	return l
}

// linkedItem is o/r#1, needing action, with the given links in a linear.issues annotation.
func linkedItem(links ...map[string]any) api.InboxItem {
	it := scoredItem(model.SectionNeedsAction, 1, 0, time.Hour)
	if links != nil {
		it.Annotations = []model.Annotation{linksAnn(it.PR.Key, "linear.issues", linksValue{Label: "Linear issues", Links: links})}
	}
	return it
}

var threeLinks = []map[string]any{
	linkOf("ABC-1", "Fix login", issue1, "In Progress"),
	linkOf("ABC-2", "Add SSO", issue2, "Todo"),
	linkOf("ABC-3", "Drop v1", issue3, ""),
}

// recordOpens makes a record the URLs it opens instead of starting a browser.
func recordOpens(a *app) *[]string {
	var opened []string
	a.open = func(u string) error { opened = append(opened, u); return nil }
	return &opened
}

func TestParseLinks(t *testing.T) {
	tests := []struct {
		name  string
		value string
		ok    bool
		label string
		ids   []string
	}{
		{"labeled", `{"label":"Linear issues","links":[{"id":"ABC-1","url":"` + issue1 + `"}]}`, true, "Linear issues", []string{"ABC-1"}},
		{"default label", `{"links":[{"id":"ABC-1","url":"` + issue1 + `"}]}`, true, "Links", []string{"ABC-1"}},
		{"blank label", `{"label":"  ","links":[]}`, true, "Links", nil},
		{"invalid links skipped", `{"links":[{"id":"no-url"},{"id":"ftp","url":"ftp://x/y"},{"id":"rel","url":"/issue/1"},` +
			`{"id":"num","url":42},"str",{"id":"ok","url":" http://x.test/1 "}]}`, true, "Links", []string{"ok"}},
		{"links not an array", `{"links":"ABC-1"}`, false, "", nil},
		{"links null", `{"links":null}`, false, "", nil},
		{"no links key", `{"badge":"urgent"}`, false, "", nil},
		{"string", `"ABC-1"`, false, "", nil},
		{"null", `null`, false, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, ok := parseLinks(model.Annotation{Key: "k", Value: json.RawMessage(tt.value)})
			var ids []string
			for _, l := range g.links {
				ids = append(ids, l.ID)
			}
			if ok != tt.ok || g.label != tt.label || !slices.Equal(ids, tt.ids) {
				t.Errorf("parseLinks(%s) = %q %v, %v; want %q %v, %v", tt.value, g.label, ids, ok, tt.label, tt.ids, tt.ok)
			}
			if ok && len(g.links) > 0 && strings.TrimSpace(g.links[0].URL) != g.links[0].URL {
				t.Errorf("URL %q not trimmed", g.links[0].URL)
			}
		})
	}
}

func TestDetailLinksSections(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := scoreTermsItem(model.ScoreTerm{Term: "urgent", Points: 30})
	k := it.PR.Key
	it.Annotations = []model.Annotation{
		linksAnn(k, "z.refs", linksValue{Links: []map[string]any{linkOf("", "", "https://example.test/doc", "")}}),
		linksAnn(k, "linear.issues", linksValue{Label: "Linear issues", Links: []map[string]any{
			linkOf("ABC-2", "Add SSO", issue2, ""),
			linkOf("ABC-9", "Without URL", "", "Backlog"),
			linkOf("ABC-1", "Fix login", issue1, "In Progress · due 2026-10-14 · project Checkout v2"),
		}}),
		linksAnn(k, "empty.links", linksValue{Label: "Nothing here", Links: []map[string]any{linkOf("X-1", "No URL", "", "")}}),
		badgeAnn(k, "linear.urgency", `{"badge":"urgent"}`),
		{PR: k, CommentID: "gone", Key: "c.links", Value: json.RawMessage(`{"links":[{"id":"C-1","url":"https://x.test/c"}]}`)},
	}
	out := ansi.Strip(a.renderPR(it, true, nil, 120))
	assertOrder(t, out,
		"Score 30",
		"Linear issues",
		"  ABC-2  Add SSO",
		"  ABC-1  Fix login  In Progress · due 2026-10-14 · project Checkout v2",
		"Links",
		"  https://example.test/doc",
		"Annotations",
		"linear.urgency",
		"c.links",
	)
	for _, s := range []string{"ABC-9", "Without URL", "Nothing here", "X-1", "linear.issues", "z.refs", "empty.links"} {
		if strings.Contains(out, s) {
			t.Errorf("detail shows %q:\n%s", s, out)
		}
	}

	for _, w := range []int{40, 20} {
		var b strings.Builder
		a.writeLinks(&b, it.Annotations, w)
		for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
			if lw := ansi.StringWidth(l); lw > w {
				t.Errorf("w=%d: line is %d cells wide: %q", w, lw, ansi.Strip(l))
			}
		}
	}
}

func TestDetailLinksOnlyNoAnnotationsSection(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	out := ansi.Strip(a.renderPR(linkedItem(threeLinks...), true, nil, 100))
	assertContains(t, out, "Linear issues", "ABC-3  Drop v1")
	if strings.Contains(out, "Annotations") {
		t.Errorf("annotations section for a links annotation alone:\n%s", out)
	}
}

func TestPRLinksDedupeByURL(t *testing.T) {
	k := prKey("o", "r", 1)
	anns := []model.Annotation{
		linksAnn(k, "z.second", linksValue{Links: []map[string]any{
			linkOf("ABC-1", "dupe of a.first", issue1, ""),
			linkOf("ABC-3", "Drop v1", issue3, ""),
		}}),
		linksAnn(k, "a.first", linksValue{Links: []map[string]any{
			linkOf("ABC-2", "Add SSO", issue2, ""),
			linkOf("ABC-1", "Fix login", issue1, ""),
			linkOf("ABC-2", "dupe within", issue2, ""),
		}}),
	}
	var got []string
	for _, l := range prLinks(anns) {
		got = append(got, l.ID+" "+l.Title)
	}
	if want := []string{"ABC-2 Add SSO", "ABC-1 Fix login", "ABC-3 Drop v1"}; !slices.Equal(got, want) {
		t.Errorf("prLinks = %q, want %q", got, want)
	}

	// The same issue in two annotations is one link and opens directly.
	it := linkedItem(linkOf("ABC-1", "Fix login", issue1, ""))
	it.Annotations = append(it.Annotations, linksAnn(k, "other.issues", linksValue{Links: []map[string]any{linkOf("ABC-1", "", issue1, "")}}))
	a, _ := loadedApp(t, listOf(it), 120, 30)
	opened := recordOpens(a)
	drain(t, a, update(t, a, keyPress("O")))
	if a.picker != nil || !slices.Equal(*opened, []string{issue1}) {
		t.Errorf("picker open %v, opened %v; want %s opened directly", a.picker != nil, *opened, issue1)
	}
}

func TestOpenLinkKey(t *testing.T) {
	for _, k := range []string{"O"} {
		t.Run(k, func(t *testing.T) {
			t.Run("none", func(t *testing.T) {
				a, _ := loadedApp(t, listOf(linkedItem()), 120, 30)
				opened := recordOpens(a)
				drain(t, a, update(t, a, keyPress(k)))
				if len(*opened) != 0 || a.picker != nil {
					t.Errorf("opened %v, picker %v; want nothing", *opened, a.picker)
				}
				assertContains(t, statusLine(a), "no linked issues")
				if a.flashErr {
					t.Error("no links flashed as an error")
				}
			})

			t.Run("one", func(t *testing.T) {
				a, _ := loadedApp(t, listOf(linkedItem(threeLinks[0])), 120, 30)
				opened := recordOpens(a)
				drain(t, a, update(t, a, keyPress(k)))
				if a.picker != nil || !slices.Equal(*opened, []string{issue1}) {
					t.Errorf("picker open %v, opened %v; want %s opened directly", a.picker != nil, *opened, issue1)
				}
				assertContains(t, statusLine(a), "opened "+issue1)
			})

			t.Run("many", func(t *testing.T) {
				a, _ := loadedApp(t, listOf(linkedItem(threeLinks...)), 120, 30)
				opened := recordOpens(a)
				drain(t, a, update(t, a, keyPress(k)))
				if len(*opened) != 0 {
					t.Errorf("opened %v before picking", *opened)
				}
				if a.picker == nil {
					t.Fatal("picker not open")
				}
				out := screen(a)
				assertFits(t, out, 120, 30)
				assertOrder(t, out, pickerTitle, "1 ABC-1  Fix login", "2 ABC-2  Add SSO", "3 ABC-3  Drop v1", "esc close")
			})
		})
	}
}

func TestOpenLinkKeyInDetailAndOnHeader(t *testing.T) {
	a, fb := loadedApp(t, listOf(linkedItem(threeLinks[1])), 140, 40)
	fb.details[prKey("o", "r", 1)] = linkedItem(threeLinks[1])
	opened := recordOpens(a)
	drain(t, a, update(t, a, keyPress("d")))
	assertContains(t, screen(a), "Linear issues", "ABC-2  Add SSO")
	drain(t, a, update(t, a, keyPress("O")))
	if !slices.Equal(*opened, []string{issue2}) {
		t.Errorf("opened %v in the detail view, want %s", *opened, issue2)
	}

	press(t, a, "space")
	if got := selectedKey(t, a); got != "header:needs_action" {
		t.Fatalf("setup: selected %s", got)
	}
	drain(t, a, update(t, a, keyPress("O")))
	if len(*opened) != 1 || a.picker != nil {
		t.Errorf("O on a section header opened %v, picker %v", *opened, a.picker)
	}
}

// pickerApp is an app whose selected PR has threeLinks, with the picker open.
func pickerApp(t *testing.T) (*app, *[]string) {
	t.Helper()
	a, _ := loadedApp(t, listOf(linkedItem(threeLinks...), scoredItem(model.SectionNeedsAction, 2, -1, time.Hour)), 120, 30)
	opened := recordOpens(a)
	press(t, a, "O")
	if a.picker == nil {
		t.Fatal("setup: picker not open")
	}
	return a, opened
}

func TestLinkPickerNavigation(t *testing.T) {
	a, opened := pickerApp(t)
	steps := []struct {
		key    string
		cursor int
	}{{"j", 1}, {"down", 2}, {"j", 2}, {"k", 1}, {"up", 0}, {"k", 0}, {"G", 2}, {"g", 0}, {"j", 1}}
	for _, s := range steps {
		press(t, a, s.key)
		if a.picker.cursor != s.cursor {
			t.Fatalf("after %s: cursor %d, want %d", s.key, a.picker.cursor, s.cursor)
		}
	}
	if got := selectedKey(t, a); got != "o/r#1" {
		t.Errorf("picker keys moved the list cursor to %s", got)
	}
	assertContains(t, screen(a), "▌2 ABC-2  Add SSO")

	drain(t, a, update(t, a, keyPress("enter")))
	if a.picker != nil {
		t.Error("enter did not close the picker")
	}
	if !slices.Equal(*opened, []string{issue2}) {
		t.Errorf("enter opened %v, want the highlighted %s", *opened, issue2)
	}
	if strings.Contains(screen(a), pickerTitle) {
		t.Error("picker still drawn after closing")
	}
}

func TestLinkPickerOpenKeys(t *testing.T) {
	for _, k := range []string{"enter", "O", "o"} {
		a, opened := pickerApp(t)
		press(t, a, "j", "j")
		drain(t, a, update(t, a, keyPress(k)))
		if a.picker != nil || !slices.Equal(*opened, []string{issue3}) {
			t.Errorf("%s: picker open %v, opened %v; want %s", k, a.picker != nil, *opened, issue3)
		}
	}
}

func TestLinkPickerNumberKeys(t *testing.T) {
	a, opened := pickerApp(t)
	press(t, a, "9", "0")
	if a.picker == nil || len(*opened) != 0 {
		t.Fatalf("number without a link: picker open %v, opened %v", a.picker != nil, *opened)
	}
	drain(t, a, update(t, a, keyPress("3")))
	if a.picker != nil || !slices.Equal(*opened, []string{issue3}) {
		t.Errorf("3: picker open %v, opened %v; want %s", a.picker != nil, *opened, issue3)
	}
	if a.tab != tabMine {
		t.Errorf("number key switched to tab %d", a.tab)
	}
}

func TestLinkPickerCloses(t *testing.T) {
	for _, k := range []string{"esc", "q"} {
		a, opened := pickerApp(t)
		msgs := drain(t, a, update(t, a, keyPress(k)))
		for _, m := range msgs {
			if _, ok := m.(tea.QuitMsg); ok {
				t.Errorf("%s quit the app while the picker was open", k)
			}
		}
		if a.picker != nil || len(*opened) != 0 {
			t.Errorf("%s: picker open %v, opened %v; want closed without opening", k, a.picker != nil, *opened)
		}
		if a.showDetail || a.help.ShowAll {
			t.Errorf("%s reached the app: detail %v, help %v", k, a.showDetail, a.help.ShowAll)
		}
	}

	a, _ := pickerApp(t)
	if _, ok := runCmd(t, update(t, a, keyPress("ctrl+c"))).(tea.QuitMsg); !ok {
		t.Error("ctrl+c did not quit while the picker was open")
	}
}

func TestLinkPickerCapturesKeys(t *testing.T) {
	a, _ := pickerApp(t)
	// The picker lists three links, so "4" picks nothing instead of jumping to a tab.
	for _, k := range []string{"x", "d", "?", "4", "tab", "space", "r", "y", "a"} {
		if cmd := update(t, a, keyPress(k)); cmd != nil {
			if msg := runImmediate(cmd); msg != nil {
				t.Errorf("%s produced %T while the picker was open", k, msg)
			}
		}
	}
	if a.picker == nil || a.showDetail || a.help.ShowAll || a.tab != tabMine || a.flash != "" {
		t.Errorf("keys reached the app: picker %v, detail %v, help %v, tab %d, flash %q",
			a.picker != nil, a.showDetail, a.help.ShowAll, a.tab, a.flash)
	}
	if a.collapsed[model.SectionNeedsAction] {
		t.Error("space folded a section while the picker was open")
	}
}

func TestLinkPickerLayout(t *testing.T) {
	long := linkOf("ABC-4", strings.Repeat("very long title ", 10), "https://linear.app/acme/issue/ABC-4", "")
	links := append(slices.Clone(threeLinks), long)
	for _, size := range []struct{ w, h int }{{120, 30}, {60, 20}, {30, 12}, {12, 6}} {
		a, _ := loadedApp(t, listOf(linkedItem(links...)), size.w, size.h)
		press(t, a, "O", "G")
		out := screen(a)
		assertFits(t, out, size.w, size.h)
		if size.w >= 30 {
			assertContains(t, out, pickerTitle, "4 ABC-4")
		}
		if size.w == 120 {
			lines := strings.Split(out, "\n")
			// The box is centered and drawn over the list, which shows around it.
			top := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "╭") })
			bottom := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "╰") })
			if top < 0 || bottom < 0 || top+bottom < size.h-3 || top+bottom > size.h-1 {
				t.Errorf("box spans lines %d to %d of %d, want it centered:\n%s", top, bottom, size.h, out)
			}
			left := ansi.StringWidth(lines[top][:strings.Index(lines[top], "╭")])
			if want := (size.w - ansi.StringWidth(a.renderPicker()[:strings.Index(a.renderPicker(), "\n")])) / 2; left != want {
				t.Errorf("box starts at column %d, want %d: %q", left, want, lines[top])
			}
			assertContains(t, out, "dev-inbox", "My PRs")
		}
	}
}

func TestOpenLinkKeyBinding(t *testing.T) {
	k := newKeyMap()
	if !key.Matches(keyPress("O"), k.OpenLink) || key.Matches(keyPress("O"), k.Open) {
		t.Error("O does not match only the open link binding")
	}
	for _, s := range []string{"enter", "shift+enter"} {
		if key.Matches(keyPress(s), k.Open, k.OpenLink) {
			t.Errorf("%s matches an open binding", s)
		}
	}

	a, _ := loadedApp(t, fixtureList(), 140, 40)
	press(t, a, "?")
	assertContains(t, screen(a), "open linked issue")
}
