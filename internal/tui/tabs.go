package tui

import "github.com/roehrijn/dev-inbox/internal/model"

const (
	tabMine = iota
	tabOthers
)

type tabDef struct {
	title string
	// short replaces title when the tab bar does not fit.
	short string
	// empty is shown in the list when the tab has no PRs.
	empty string
}

var tabDefs = [...]tabDef{
	tabMine:   {title: "My PRs", short: "Mine", empty: "You have no open pull requests."},
	tabOthers: {title: "Others' PRs", short: "Others", empty: "Nothing to review. All caught up."},
}

// The hidden sections are TUI-local: each tab lists the PRs the viewer hid until something new
// happens in them (model.Classification.Hidden) in its own section, after all others.
const (
	sectionHiddenMine   model.Section = "hidden_mine"
	sectionHiddenOthers model.Section = "hidden_others"
)

// hiddenSections holds the hidden section of each tab.
var hiddenSections = [len(tabDefs)]model.Section{tabMine: sectionHiddenMine, tabOthers: sectionHiddenOthers}

// sectionTab fixes the tab each section is listed on.
var sectionTab = map[model.Section]int{
	model.SectionNeedsAction:      tabMine,
	model.SectionReadyToMerge:     tabMine,
	model.SectionDrafts:           tabMine,
	model.SectionWaitingForReview: tabMine,
	sectionHiddenMine:             tabMine,
	model.SectionReviewRequested:  tabOthers,
	model.SectionInvolved:         tabOthers,
	sectionHiddenOthers:           tabOthers,
}

// tabState is the view state a tab keeps while another tab is active.
type tabState struct {
	cursor, offset int
}

func (a *app) cur() *tabState { return &a.tabs[a.tab] }

// viewName names the view of the Others' PRs tab, see hiddenFromList.
func (a *app) viewName() string {
	if a.showAll {
		return "all"
	}
	return "needs me"
}

// selectTab activates tab ti, wrapping around at both ends.
func (a *app) selectTab(ti int) {
	n := len(tabDefs)
	a.tab = (ti%n + n) % n
}

// tabSections returns the indices into a.sections of the sections listed on tab ti. The
// hidden section is listed only while it holds PRs.
func (a *app) tabSections(ti int) []int {
	var idx []int
	for i, s := range a.sections {
		if sectionTab[s.id] == ti && (s.id != hiddenSections[ti] || len(s.items) > 0) {
			idx = append(idx, i)
		}
	}
	return idx
}

// tabCount counts the PRs listed on tab ti, except those in its hidden section.
func (a *app) tabCount(ti int) int {
	n := 0
	for _, si := range a.tabSections(ti) {
		if s := a.sections[si]; s.id != hiddenSections[ti] {
			n += len(s.items)
		}
	}
	return n
}

// anchor is what a tab's cursor points at: a PR, or the header of a collapsed section.
type anchor struct {
	// keys lists the PRs the cursor may go back to, in order of preference: the selected PR,
	// the PRs below it, then the PRs above it, nearest first.
	keys   []model.PRKey
	header model.Section
}

func (a *app) anchorOf(ti int) anchor {
	es := a.entriesOf(ti)
	if len(es) == 0 {
		return anchor{}
	}
	c := min(max(a.tabs[ti].cursor, 0), len(es)-1)
	if es[c].row < 0 {
		return anchor{header: a.sections[es[c].sec].id}
	}
	key := func(e entry) model.PRKey { return a.sections[e.sec].items[e.row].PR.Key }
	keys := []model.PRKey{key(es[c])}
	for _, e := range es[c+1:] {
		if e.row >= 0 {
			keys = append(keys, key(e))
		}
	}
	for i := c - 1; i >= 0; i-- {
		if es[i].row >= 0 {
			keys = append(keys, key(es[i]))
		}
	}
	return anchor{keys: keys}
}

// restore puts the cursor of tab ti back on an. If the PR is gone or hidden it moves to the
// nearest PR left, and keeps the position if none is.
func (a *app) restore(ti int, an anchor) {
	rows := map[model.PRKey]int{}
	for i, e := range a.entriesOf(ti) {
		s := a.sections[e.sec]
		switch {
		case e.row >= 0:
			rows[s.items[e.row].PR.Key] = i
		case an.header != model.SectionNone && s.id == an.header:
			a.tabs[ti].cursor = i
			return
		}
	}
	for _, k := range an.keys {
		if i, ok := rows[k]; ok {
			a.tabs[ti].cursor = i
			return
		}
	}
	a.clamp(ti)
}

func (a *app) clamp(ti int) {
	t := &a.tabs[ti]
	t.cursor = min(max(t.cursor, 0), max(len(a.entriesOf(ti))-1, 0))
}
