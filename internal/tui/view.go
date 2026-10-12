package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	rowHeight = 2
	// rowIndent is the width of the gutter, direct flag and checks icon in front of a title.
	rowIndent = 6
	// splitMinWidth is the narrowest terminal that shows the detail pane beside the list.
	splitMinWidth = 110
	minListWidth  = 50
)

type layout struct {
	listW, listH     int
	detailW, detailH int
	split            bool
}

func (a *app) bodyHeight() int {
	h := a.height - 3 // header, tab bar and status bar
	if help := a.helpView(); help != "" {
		h -= lipgloss.Height(help)
	}
	return max(h, 0)
}

// helpView is the full help, cut to the lines left between tab bar and status bar.
func (a *app) helpView() string {
	if !a.help.ShowAll {
		return ""
	}
	lines := strings.Split(a.help.View(a.keys), "\n")
	return strings.Join(lines[:min(len(lines), max(a.height-3, 0))], "\n")
}

func (a *app) layout() layout {
	h := a.bodyHeight()
	l := layout{listW: a.width, listH: h}
	if !a.showDetail {
		return l
	}
	if a.width >= splitMinWidth {
		l.split = true
		l.listW = max(minListWidth, a.width*2/5)
		l.detailW, l.detailH = a.width-l.listW, h
		return l
	}
	l.listH = max(h*2/5, min(h, rowHeight+1))
	l.detailW, l.detailH = a.width, h-l.listH
	return l
}

// detailSize is the size of the viewport inside the detail pane's border and padding.
func (l layout) detailSize() (w, h int) {
	if l.split {
		return max(l.detailW-2, 0), l.detailH
	}
	return max(l.detailW-1, 0), max(l.detailH-1, 0)
}

type span struct{ top, bottom int }

// spans returns the list lines occupied by each entry, in the order of listLines. The
// first row of a section includes its header so scrolling up reveals the header.
func (a *app) spans() (spans []span, total int) {
	line := 0
	for _, si := range a.tabSections(a.tab) {
		s := a.sections[si]
		header := line
		line++
		switch {
		case len(s.items) == 0:
		case a.collapsed[s.id]:
			spans = append(spans, span{header, header})
		default:
			for ri := range s.items {
				top := line
				if ri == 0 {
					top = header
				}
				spans = append(spans, span{top, line + rowHeight - 1})
				line += rowHeight
			}
		}
	}
	return spans, line
}

func (a *app) ensureVisible() {
	h := a.layout().listH
	spans, total := a.spans()
	t := a.cur()
	if t.cursor >= 0 && t.cursor < len(spans) && h > 0 {
		s := spans[t.cursor]
		if s.bottom >= t.offset+h {
			t.offset = s.bottom - h + 1
		}
		if s.top < t.offset {
			t.offset = s.top
		}
	}
	t.offset = max(min(t.offset, total-h), 0)
}

func (a *app) View() tea.View {
	v := tea.NewView(a.render())
	v.AltScreen = true
	v.WindowTitle = "dev-inbox"
	return v
}

func (a *app) render() string {
	if a.width <= 0 || a.height <= 0 {
		return ""
	}
	parts := []string{a.renderHeader(), a.renderTabs()}
	if a.bodyHeight() > 0 {
		parts = append(parts, a.renderBody())
	}
	if help := a.helpView(); help != "" {
		parts = append(parts, help)
	}
	parts = append(parts, a.renderStatus())
	return strings.Join(parts, "\n")
}

func (a *app) renderHeader() string {
	s := " " + a.st.accentBold.Render("dev-inbox") + a.st.muted.Render("  pull requests")
	return ansi.Truncate(s, a.width, "")
}

// renderTabs renders the tab bar with the PR count of each tab; the counts wait for the
// first list. Short titles stand in when the full ones do not fit.
func (a *app) renderTabs() string {
	bar := func(short bool) string {
		labels := make([]string, len(tabDefs))
		for i, t := range tabDefs {
			label := t.title
			if short {
				label = t.short
			}
			if a.loaded {
				label += " " + strconv.Itoa(a.tabCount(i))
			}
			if i == tabOthers {
				label += " · " + a.viewName()
			}
			st := a.st.muted
			if i == a.tab {
				st = a.st.tabActive
			}
			labels[i] = st.Render(label)
		}
		return " " + strings.Join(labels, "   ")
	}
	if full := bar(false); ansi.StringWidth(full) <= a.width {
		return full
	}
	return ansi.Truncate(bar(true), a.width, "")
}

func (a *app) renderBody() string {
	if !a.loaded {
		return a.renderMessage(a.bodyHeight())
	}
	l := a.layout()
	list := a.renderList(l.listW, l.listH)
	if !a.showDetail || l.detailH <= 0 {
		return list
	}
	pane := lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderForeground(a.st.p.divider).PaddingLeft(1)
	if l.split {
		pane = pane.BorderLeft(true)
		return lipgloss.JoinHorizontal(lipgloss.Top, list, pane.Render(a.viewport.View()))
	}
	pane = pane.BorderTop(true)
	return lipgloss.JoinVertical(lipgloss.Left, list, pane.Render(a.viewport.View()))
}

func (a *app) renderMessage(h int) string {
	var title, body string
	switch {
	case !a.connected:
		title = "Daemon not reachable at " + a.natsURL
		body = "Start it with `dev-inbox serve`. Reconnecting in the background…"
	case a.listErr != nil:
		title = "Could not load the inbox"
		body = a.listErr.Error() + "\nRetrying…"
	default:
		title = "Loading inbox…"
	}
	w := max(a.width-4, 1)
	block := a.st.bold.Width(w).Align(lipgloss.Center).Render(title)
	if body != "" {
		block += "\n\n" + a.st.muted.Width(w).Align(lipgloss.Center).Render(body)
	}
	return lipgloss.Place(a.width, h, lipgloss.Center, lipgloss.Center, block)
}

func (a *app) renderList(w, h int) string {
	lines := a.listLines(w)
	start := max(min(a.cur().offset, len(lines)-h), 0)
	end := min(len(lines), start+h)
	out := make([]string, 0, h)
	for _, l := range lines[start:end] {
		out = append(out, fit(l, w))
	}
	for len(out) < h {
		out = append(out, strings.Repeat(" ", w))
	}
	return strings.Join(out, "\n")
}

// fit truncates or pads s to exactly w cells.
func fit(s string, w int) string {
	s = ansi.Truncate(s, w, "")
	return s + strings.Repeat(" ", max(w-ansi.StringWidth(s), 0))
}

func (a *app) listLines(w int) []string {
	sel, hasSel := a.selected()
	var lines []string
	for _, si := range a.tabSections(a.tab) {
		s := a.sections[si]
		collapsed := a.collapsed[s.id] && len(s.items) > 0
		lines = append(lines, a.renderSectionHeader(s, collapsed, hasSel && sel.sec == si && sel.row < 0, w))
		if collapsed {
			continue
		}
		for ri, it := range s.items {
			l1, l2 := a.renderRow(it, hasSel && sel.sec == si && sel.row == ri, w)
			lines = append(lines, l1, l2)
		}
	}
	// Not tabCount: PRs hidden until new activity are left out of it but still listed.
	if len(a.entries()) == 0 {
		lines = append(lines, "", "  "+a.st.muted.Render(tabDefs[a.tab].empty))
	}
	return lines
}

func (a *app) gutter(selected bool) string {
	if selected {
		return a.st.accent.Render("▌")
	}
	return " "
}

func (a *app) renderSectionHeader(s section, collapsed, focused bool, w int) string {
	title := sectionTitles[s.id]
	var head string
	if len(s.items) == 0 {
		head = a.st.subtle.Render("  " + title + " 0")
	} else {
		chevron := "▾"
		if collapsed {
			chevron = "▸"
		}
		ts := a.st.bold
		if focused {
			ts = a.st.accentBold
		}
		head = ts.Render(chevron+" "+title) + " " + a.st.accentBold.Render(strconv.Itoa(len(s.items)))
	}
	if s.hidden > 0 {
		head += a.st.muted.Render(" · " + strconv.Itoa(s.hidden) + " hidden")
	}
	line := a.gutter(focused) + head + " "
	if rest := w - ansi.StringWidth(line); rest > 0 {
		line += a.st.divider.Render(strings.Repeat("─", rest))
	}
	return line
}

func (a *app) renderRow(it api.InboxItem, selected bool, w int) (string, string) {
	pr := it.PR
	gutter := a.gutter(selected)
	flag := " "
	if it.Direct {
		flag = a.st.accentBold.Render("★")
	}
	prefix := gutter + " " + flag + " " + a.checkIcon(pr.Checks) + " "
	avail := max(w-rowIndent, 0)

	tail := a.renderTail(it, avail/2)
	tw := ansi.StringWidth(tail)
	var mark string
	if pr.IsDraft {
		mark = a.st.draft.Render("◌") + " "
	}
	titleW := avail - ansi.StringWidth(mark)
	if tw > 0 {
		titleW -= tw + 1
	}
	title := pr.Title
	if title == "" {
		title = "(no title)"
	}
	ts := a.st.text
	if selected {
		ts = a.st.accentBold
	}
	t := mark + ts.Render(ansi.Truncate(title, max(titleW, 0), "…"))
	line1 := prefix + t
	if tw > 0 {
		line1 += strings.Repeat(" ", max(avail-ansi.StringWidth(t)-tw, 1)) + tail
	}

	meta := []string{a.st.muted.Render(pr.Key.String())}
	if pr.Author != "" {
		meta = append(meta, a.st.muted.Render(pr.Author))
	}
	if age := relAge(a.now(), pr.UpdatedAt); age != "" {
		meta = append(meta, a.st.muted.Render(age))
	}
	if pr.IsDraft {
		meta = append(meta, a.st.draft.Render("draft"))
	}
	if d := a.decision(pr.ReviewDecision, it.Reasons); d != "" {
		meta = append(meta, d)
	}
	if len(it.Reasons) > 0 {
		meta = append(meta, a.st.accent.Render(strings.Join(it.Reasons, ", ")))
	}
	if c := a.conflictMarker(it); c != "" {
		meta = append(meta, c)
	}
	line2 := gutter + strings.Repeat(" ", rowIndent-1) + strings.Join(meta, a.st.muted.Render(" · "))
	return ansi.Truncate(line1, w, ""), ansi.Truncate(line2, w, "…")
}

// renderTail renders the score and badges at the right end of a row's first line within
// maxW cells; the score wins when both do not fit.
func (a *app) renderTail(it api.InboxItem, maxW int) string {
	score := a.renderScore(it.Score)
	sw := ansi.StringWidth(score)
	if sw == 0 || sw > maxW {
		return a.renderBadges(it.Annotations, maxW)
	}
	badges := a.renderBadges(it.Annotations, maxW-sw-1)
	if badges == "" {
		return score
	}
	return score + " " + badges
}

// renderScore renders a non-zero priority score: accent when positive, muted when negative.
func (a *app) renderScore(score int) string {
	switch {
	case score > 0:
		return a.st.accent.Render(strconv.Itoa(score))
	case score < 0:
		return a.st.muted.Render(strconv.Itoa(score))
	}
	return ""
}

func (a *app) checkIcon(c model.ChecksState) string {
	switch c {
	case model.ChecksSuccess:
		return a.st.success.Render("✓")
	case model.ChecksFailure:
		return a.st.danger.Render("✗")
	case model.ChecksPending:
		return a.st.warning.Render("●")
	}
	return " "
}

// decision renders a review decision unless the needs_action reasons already say the same.
func (a *app) decision(d string, reasons []string) string {
	label := humanize(d)
	if label == "" || containsFold(reasons, label) {
		return ""
	}
	switch d {
	case "APPROVED":
		return a.st.success.Render(label)
	case "CHANGES_REQUESTED":
		return a.st.danger.Render(label)
	case "REVIEW_REQUIRED":
		return a.st.warning.Render(label)
	}
	return a.st.muted.Render(label)
}

const mergeConflict = "merge conflict"

// conflictMarker renders the merge-conflict marker of a conflicting PR unless the
// needs_action reasons already say the same.
func (a *app) conflictMarker(it api.InboxItem) string {
	if it.PR.Mergeable != "CONFLICTING" || containsFold(it.Reasons, mergeConflict) {
		return ""
	}
	return a.st.warning.Render(mergeConflict)
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func (a *app) renderStatus() string {
	sep := a.st.muted.Render(" · ")
	var left []string
	if a.connected {
		left = append(left, a.st.success.Render("●")+a.st.muted.Render(" connected"))
	} else {
		left = append(left, a.st.dangerBold.Render("○ daemon not reachable at "+a.natsURL+", start `dev-inbox serve`"))
	}
	if a.loaded {
		if a.listErr != nil {
			left = append(left, a.st.danger.Render("refresh failed: "+a.listErr.Error()))
		}
		left = append(left, a.st.muted.Render(plural(a.total(), "PR")))
		if a.viewer.Login != "" {
			left = append(left, a.st.muted.Render("@"+a.viewer.Login))
		}
		left = append(left, a.st.muted.Render("refreshed "+a.refreshedAt.Local().Format(time.TimeOnly)))
	}
	if len(a.plugins) > 0 {
		left = append(left, a.renderPlugins())
	}

	right := a.st.muted.Render("? help")
	if a.flash != "" {
		st := a.st.accent
		if a.flashErr {
			st = a.st.danger
		}
		right = st.Render(ansi.Truncate(a.flash, max(a.width/2, 10), "…"))
	}
	room := a.width - ansi.StringWidth(right) - 3
	// Drop the least important parts first, truncate only what is left.
	for len(left) > 1 && ansi.StringWidth(" "+strings.Join(left, sep)) > room {
		left = left[:len(left)-1]
	}
	l := ansi.Truncate(" "+strings.Join(left, sep), max(room, 0), "…")
	gap := max(a.width-ansi.StringWidth(l)-ansi.StringWidth(right)-1, 1)
	return ansi.Truncate(l+strings.Repeat(" ", gap)+right+" ", a.width, "")
}

func (a *app) renderPlugins() string {
	names := make([]string, len(a.plugins))
	for i, n := range a.plugins {
		names[i] = a.st.muted.Render(n) + " " + a.st.success.Render("●")
	}
	return strings.Join(names, "  ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// relAge formats the time since t compactly, e.g. "5m ago"; empty for a zero t.
func relAge(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy ago", int(d.Hours()/24/365))
}
