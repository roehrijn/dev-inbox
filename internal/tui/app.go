package tui

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	debounceDelay = 300 * time.Millisecond
	retryDelay    = 2 * time.Second
	flashDuration = 3 * time.Second
)

var sectionTitles = map[model.Section]string{
	model.SectionReviewRequested:  "Review requested",
	model.SectionNeedsAction:      "Needs action",
	model.SectionReadyToMerge:     "Ready to merge",
	model.SectionDrafts:           "Your drafts",
	model.SectionWaitingForReview: "Waiting for review",
	model.SectionInvolved:         "Involved",
}

type (
	connMsg struct{ connected bool }
	// eventMsg reports a published event; key is nil when the event could not be decoded.
	eventMsg      struct{ key *model.PRKey }
	relistMsg     struct{}
	tickMsg       struct{}
	clearFlashMsg struct{ seq int }
	flashMsg      struct {
		text string
		err  bool
	}
	listMsg struct {
		seq  int
		resp api.ListResponse
		err  error
		at   time.Time
	}
	detailMsg struct {
		key  model.PRKey
		item *api.InboxItem
		err  error
	}
)

type section struct {
	id    model.Section
	items []api.InboxItem
	// hidden counts the PRs of the section left out of items, see hiddenFromList.
	hidden int
}

// entry is a cursor position: a PR row, or the header of a collapsed section (row -1).
// sec indexes app.sections.
type entry struct {
	sec, row int
}

// detailSig identifies what the detail viewport currently shows, to skip needless re-renders.
type detailSig struct {
	entry
	key   model.PRKey
	width int
	dark  bool
	rev   int
}

type rendererKey struct {
	width int
	dark  bool
}

type markdownKey struct {
	body string
	rendererKey
}

type app struct {
	ctx      context.Context
	be       backend
	natsURL  string
	now      func() time.Time
	open     func(url string) error
	debounce time.Duration
	retry    time.Duration

	width, height int
	dark          bool
	st            styles
	keys          keyMap
	help          help.Model

	connected     bool
	loaded        bool
	listErr       error
	refreshedAt   time.Time
	listSeq       int
	appliedSeq    int
	relistPending bool

	viewer    model.Viewer
	sections  []section
	collapsed map[model.Section]bool
	tab       int
	tabs      [len(tabDefs)]tabState

	showDetail  bool
	viewport    viewport.Model
	details     map[model.PRKey]*api.InboxItem
	detailErrs  map[model.PRKey]error
	inflight    map[model.PRKey]bool
	detailStale bool
	detailSig   detailSig
	renderers   map[rendererKey]*glamour.TermRenderer
	markdown    map[markdownKey]string
	// rev changes whenever data shown in the detail pane may have changed.
	rev int

	flash    string
	flashErr bool
	flashSeq int
}

func newApp(ctx context.Context, be backend, natsURL string) *app {
	a := &app{
		ctx:        ctx,
		be:         be,
		natsURL:    natsURL,
		now:        time.Now,
		open:       openURL,
		debounce:   debounceDelay,
		retry:      retryDelay,
		keys:       newKeyMap(),
		help:       help.New(),
		collapsed:  map[model.Section]bool{},
		viewport:   viewport.New(),
		details:    map[model.PRKey]*api.InboxItem{},
		detailErrs: map[model.PRKey]error{},
		inflight:   map[model.PRKey]bool{},
		renderers:  map[rendererKey]*glamour.TermRenderer{},
		markdown:   map[markdownKey]string{},
	}
	a.viewport.FillHeight = true
	for _, id := range model.Sections {
		a.sections = append(a.sections, section{id: id})
	}
	a.setDark(true)
	return a
}

func (a *app) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, minuteTick())
}

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.help.SetWidth(msg.Width)
	case tea.BackgroundColorMsg:
		a.setDark(msg.IsDark())
	case tea.KeyPressMsg:
		cmds = append(cmds, a.handleKey(msg))
	case connMsg:
		a.connected = msg.connected
		if msg.connected {
			// Events may have been missed while disconnected.
			a.forgetDetails(nil)
			cmds = append(cmds, a.relist())
		}
	case eventMsg:
		cmds = append(cmds, a.handleEvent(msg.key))
	case relistMsg:
		a.relistPending = false
		cmds = append(cmds, a.relist())
	case listMsg:
		cmds = append(cmds, a.handleList(msg))
	case detailMsg:
		delete(a.inflight, msg.key)
		if msg.err != nil {
			a.detailErrs[msg.key] = msg.err
		} else {
			a.details[msg.key] = msg.item
			delete(a.detailErrs, msg.key)
		}
		a.rev++
	case flashMsg:
		cmds = append(cmds, a.setFlash(msg.text, msg.err))
	case clearFlashMsg:
		if msg.seq == a.flashSeq {
			a.flash = ""
		}
	case tickMsg:
		a.rev++
		cmds = append(cmds, minuteTick())
	}
	a.move(0)
	a.ensureVisible()
	cmds = append(cmds, a.syncDetail())
	return a, tea.Batch(cmds...)
}

func (a *app) setDark(dark bool) {
	a.dark = dark
	a.st = newStyles(dark)
	a.help.Styles = a.st.help
}

func minuteTick() tea.Cmd {
	return tea.Tick(time.Minute, func(time.Time) tea.Msg { return tickMsg{} })
}

func (a *app) setFlash(text string, isErr bool) tea.Cmd {
	a.flash, a.flashErr = text, isErr
	a.flashSeq++
	seq := a.flashSeq
	return tea.Tick(flashDuration, func(time.Time) tea.Msg { return clearFlashMsg{seq: seq} })
}

func (a *app) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	k := a.keys
	sel, hasSel := a.selected()
	switch {
	case key.Matches(msg, k.Quit):
		return tea.Quit
	case key.Matches(msg, k.Help):
		a.help.ShowAll = !a.help.ShowAll
	case key.Matches(msg, k.Back):
		if a.help.ShowAll {
			a.help.ShowAll = false
		} else {
			a.showDetail = false
		}
	case key.Matches(msg, k.Up):
		a.move(-1)
	case key.Matches(msg, k.Down):
		a.move(1)
	case key.Matches(msg, k.Top):
		a.cur().cursor = 0
	case key.Matches(msg, k.Bottom):
		a.cur().cursor = max(len(a.entries())-1, 0)
	case key.Matches(msg, k.NextTab):
		a.selectTab(a.tab + 1)
	case key.Matches(msg, k.PrevTab):
		a.selectTab(a.tab - 1)
	case key.Matches(msg, k.JumpTab):
		a.selectTab(int(msg.String()[0] - '1'))
	case key.Matches(msg, k.Toggle):
		if hasSel {
			a.setCollapsed(sel.sec, sel.row >= 0)
		}
	case key.Matches(msg, k.Collapse):
		if hasSel && sel.row >= 0 {
			a.setCollapsed(sel.sec, true)
		}
	case key.Matches(msg, k.Expand):
		if hasSel && sel.row < 0 {
			a.setCollapsed(sel.sec, false)
		}
	case key.Matches(msg, k.Open):
		if hasSel && sel.row < 0 {
			a.setCollapsed(sel.sec, false)
		} else if it := a.selectedItem(); it != nil {
			return a.openCmd(prURL(it.PR))
		}
	case key.Matches(msg, k.Copy):
		if it := a.selectedItem(); it != nil {
			u := prURL(it.PR)
			return tea.Batch(tea.SetClipboard(u), a.setFlash("copied "+u, false))
		}
	case key.Matches(msg, k.Detail):
		a.showDetail = !a.showDetail
	case key.Matches(msg, k.ScrollDown):
		a.viewport.ScrollDown(3)
	case key.Matches(msg, k.ScrollUp):
		a.viewport.ScrollUp(3)
	case key.Matches(msg, k.PageDown):
		a.viewport.HalfPageDown()
	case key.Matches(msg, k.PageUp):
		a.viewport.HalfPageUp()
	case key.Matches(msg, k.Refresh):
		if !a.connected {
			return a.setFlash("not connected to the daemon", true)
		}
		clear(a.detailErrs)
		a.detailStale = true
		return a.relist()
	}
	return nil
}

func (a *app) openCmd(u string) tea.Cmd {
	open := a.open
	return func() tea.Msg {
		if err := open(u); err != nil {
			return flashMsg{text: "open browser: " + err.Error(), err: true}
		}
		return flashMsg{text: "opened " + u}
	}
}

func prURL(pr model.PR) string {
	if pr.URL != "" {
		return pr.URL
	}
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.Key.Owner, pr.Key.Repo, pr.Key.Number)
}

func openURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("refusing to open %q", raw)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", raw)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw)
	default:
		cmd = exec.Command("xdg-open", raw)
	}
	return cmd.Run()
}

// relist requests the inbox and, if marked stale, the PR shown in the detail pane.
func (a *app) relist() tea.Cmd {
	if !a.connected {
		return nil
	}
	a.listSeq++
	seq, be, ctx := a.listSeq, a.be, a.ctx
	cmds := []tea.Cmd{func() tea.Msg {
		resp, err := be.List(ctx)
		return listMsg{seq: seq, resp: resp, err: err, at: time.Now()}
	}}
	if a.detailStale {
		a.detailStale = false
		if k, ok := a.detailKey(); ok {
			cmds = append(cmds, a.fetchDetail(k))
		}
	}
	return tea.Batch(cmds...)
}

func (a *app) scheduleRelist(d time.Duration) tea.Cmd {
	if a.relistPending {
		return nil
	}
	a.relistPending = true
	return tea.Tick(d, func(time.Time) tea.Msg { return relistMsg{} })
}

func (a *app) fetchDetail(k model.PRKey) tea.Cmd {
	a.inflight[k] = true
	be, ctx := a.be, a.ctx
	return func() tea.Msg {
		resp, err := be.Get(ctx, k)
		return detailMsg{key: k, item: resp.Item, err: err}
	}
}

// handleEvent coalesces bursts of events into one re-list after the debounce delay.
func (a *app) handleEvent(k *model.PRKey) tea.Cmd {
	a.forgetDetails(k)
	return a.scheduleRelist(a.debounce)
}

// forgetDetails drops cached details of k, or of all PRs when k is nil. The PR in the
// detail pane is kept on screen and marked stale so the next re-list refreshes it.
func (a *app) forgetDetails(k *model.PRKey) {
	cur, hasCur := a.detailKey()
	match := func(dk model.PRKey) bool {
		return (k == nil || sameKey(dk, *k)) && (!hasCur || dk != cur)
	}
	maps.DeleteFunc(a.details, func(dk model.PRKey, _ *api.InboxItem) bool { return match(dk) })
	maps.DeleteFunc(a.detailErrs, func(dk model.PRKey, _ error) bool { return match(dk) })
	if hasCur && (k == nil || sameKey(cur, *k)) {
		a.detailStale = true
	}
}

func sameKey(x, y model.PRKey) bool {
	return x.Number == y.Number && strings.EqualFold(x.Owner, y.Owner) && strings.EqualFold(x.Repo, y.Repo)
}

func (a *app) handleList(msg listMsg) tea.Cmd {
	if msg.seq < a.appliedSeq {
		return nil
	}
	a.appliedSeq = msg.seq
	if msg.err != nil {
		a.listErr = msg.err
		return a.scheduleRelist(a.retry)
	}
	a.listErr = nil
	a.loaded = true
	a.refreshedAt = msg.at
	a.applyList(msg.resp)
	return nil
}

func (a *app) applyList(resp api.ListResponse) {
	var anchors [len(tabDefs)]anchor
	for ti := range anchors {
		anchors[ti] = a.anchorOf(ti)
	}

	a.viewer = resp.Viewer
	a.sections = groupItems(resp.Items)
	present := map[model.PRKey]bool{}
	for _, it := range resp.Items {
		present[it.PR.Key] = true
	}
	for k := range a.details {
		if !present[k] {
			delete(a.details, k)
			delete(a.detailErrs, k)
		}
	}
	a.rev++

	for ti, an := range anchors {
		a.restore(ti, an)
	}
}

// hiddenFromList reports whether it is kept out of the list: PRs of others with a
// negative score. Own PRs are always listed.
func hiddenFromList(it api.InboxItem) bool {
	return it.Score < 0 && sectionTab[it.Section] == tabOthers
}

// groupItems sorts items into model.Sections order; inside a section the highest score
// comes first, then the most recently updated. Items without a known section are dropped
// and those matching hiddenFromList are only counted in section.hidden.
func groupItems(items []api.InboxItem) []section {
	secs := make([]section, len(model.Sections))
	idx := map[model.Section]int{}
	for i, id := range model.Sections {
		secs[i].id = id
		idx[id] = i
	}
	for _, it := range items {
		i, ok := idx[it.Section]
		switch {
		case !ok:
		case hiddenFromList(it):
			secs[i].hidden++
		default:
			secs[i].items = append(secs[i].items, it)
		}
	}
	for _, s := range secs {
		slices.SortStableFunc(s.items, func(x, y api.InboxItem) int {
			if c := cmp.Compare(y.Score, x.Score); c != 0 {
				return c
			}
			if c := y.PR.UpdatedAt.Compare(x.PR.UpdatedAt); c != 0 {
				return c
			}
			return cmp.Compare(x.PR.Key.String(), y.PR.Key.String())
		})
	}
	return secs
}

// total counts all PRs, hidden ones included.
func (a *app) total() int {
	n := 0
	for _, s := range a.sections {
		n += len(s.items) + s.hidden
	}
	return n
}

// entries lists the cursor positions of the active tab: rows of expanded sections and
// headers of collapsed ones. Empty sections are skipped.
func (a *app) entries() []entry { return a.entriesOf(a.tab) }

func (a *app) entriesOf(ti int) []entry {
	var es []entry
	for _, si := range a.tabSections(ti) {
		s := a.sections[si]
		switch {
		case len(s.items) == 0:
		case a.collapsed[s.id]:
			es = append(es, entry{sec: si, row: -1})
		default:
			for ri := range s.items {
				es = append(es, entry{sec: si, row: ri})
			}
		}
	}
	return es
}

func (a *app) selected() (entry, bool) { return a.selectedIn(a.tab) }

func (a *app) selectedIn(ti int) (entry, bool) {
	es := a.entriesOf(ti)
	if len(es) == 0 {
		return entry{}, false
	}
	return es[min(max(a.tabs[ti].cursor, 0), len(es)-1)], true
}

func (a *app) selectedItem() *api.InboxItem {
	e, ok := a.selected()
	if !ok || e.row < 0 {
		return nil
	}
	return &a.sections[e.sec].items[e.row]
}

// detailKey returns the PR shown in the detail pane.
func (a *app) detailKey() (model.PRKey, bool) {
	if !a.showDetail {
		return model.PRKey{}, false
	}
	if it := a.selectedItem(); it != nil {
		return it.PR.Key, true
	}
	return model.PRKey{}, false
}

func (a *app) move(delta int) {
	a.cur().cursor += delta
	a.clamp(a.tab)
}

// setCollapsed folds section si and puts the cursor on its header, or unfolds it and
// puts the cursor on its first row.
func (a *app) setCollapsed(si int, collapsed bool) {
	a.collapsed[a.sections[si].id] = collapsed
	for i, e := range a.entries() {
		if e.sec == si {
			a.cur().cursor = i
			return
		}
	}
}
