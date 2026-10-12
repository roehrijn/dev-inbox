package tui

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	defaultLinksLabel = "Links"
	pickerTitle       = "Open linked issue"
	maxPickerWidth    = 80
)

type link struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Detail string `json:"detail"`
}

type linkGroup struct {
	key, label string
	links      []link
}

// parseLinks reads a links annotation, a value of the form
// {"label": "...", "links": [{"id": "...", "title": "...", "url": "...", "detail": "..."}]}.
// Links without an http(s) URL are left out.
func parseLinks(ann model.Annotation) (linkGroup, bool) {
	var v struct {
		Label string             `json:"label"`
		Links *[]json.RawMessage `json:"links"`
	}
	if err := json.Unmarshal(ann.Value, &v); err != nil || v.Links == nil {
		return linkGroup{}, false
	}
	g := linkGroup{key: ann.Key, label: strings.TrimSpace(v.Label)}
	if g.label == "" {
		g.label = defaultLinksLabel
	}
	for _, raw := range *v.Links {
		var l link
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		l.URL = strings.TrimSpace(l.URL)
		if webURL(l.URL) {
			g.links = append(g.links, l)
		}
	}
	return g, true
}

// prLinkGroups returns the links annotations among the PR-level annotations, sorted by key.
func prLinkGroups(anns []model.Annotation) []linkGroup {
	var out []linkGroup
	for _, ann := range anns {
		if ann.CommentID != "" {
			continue
		}
		if g, ok := parseLinks(ann); ok {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(x, y linkGroup) int { return strings.Compare(x.key, y.key) })
	return out
}

// prLinks returns the links of all PR-level links annotations, each URL once.
func prLinks(anns []model.Annotation) []link {
	var out []link
	seen := map[string]bool{}
	for _, g := range prLinkGroups(anns) {
		for _, l := range g.links {
			if !seen[l.URL] {
				seen[l.URL] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// writeLinks writes a section per links annotation, headed by its label.
func (a *app) writeLinks(b *strings.Builder, anns []model.Annotation, w int) {
	for _, g := range prLinkGroups(anns) {
		if len(g.links) == 0 {
			continue
		}
		a.writeHeading(b, g.label, "")
		for _, l := range g.links {
			s := a.linkName(l, a.st.accentBold, a.st.text)
			if l.Detail != "" {
				s += "  " + a.st.muted.Render(l.Detail)
			}
			b.WriteString(ansi.Wrap("  "+s, w, " ") + "\n")
		}
	}
}

// linkName renders the ID and title of l, or its URL when it has neither.
func (a *app) linkName(l link, idSt, titleSt lipgloss.Style) string {
	var parts []string
	if l.ID != "" {
		parts = append(parts, idSt.Hyperlink(l.URL).Render(l.ID))
	}
	if l.Title != "" {
		parts = append(parts, titleSt.Render(l.Title))
	}
	if len(parts) == 0 {
		return a.st.code.Hyperlink(l.URL).Render(l.URL)
	}
	return strings.Join(parts, "  ")
}

// linkPicker is the popup listing the links of a PR to open one of them.
type linkPicker struct {
	links  []link
	cursor int
}

// openLinks opens the only link of it, or the picker when it has several.
func (a *app) openLinks(it api.InboxItem) tea.Cmd {
	if full := a.details[it.PR.Key]; full != nil {
		it = *full
	}
	links := prLinks(it.Annotations)
	switch len(links) {
	case 0:
		return a.setFlash("no linked issues", false)
	case 1:
		return a.openCmd(links[0].URL)
	}
	a.picker = &linkPicker{links: links}
	return nil
}

// handlePickerKey handles every key while the picker is open; only ctrl+c gets through.
func (a *app) handlePickerKey(msg tea.KeyPressMsg) tea.Cmd {
	p, k := a.picker, a.keys
	pick := -1
	switch s := msg.String(); {
	case s == "ctrl+c":
		return tea.Quit
	case key.Matches(msg, k.Back, k.Quit):
		a.picker = nil
	case key.Matches(msg, k.Up):
		p.cursor = max(p.cursor-1, 0)
	case key.Matches(msg, k.Down):
		p.cursor = min(p.cursor+1, len(p.links)-1)
	case key.Matches(msg, k.Top):
		p.cursor = 0
	case key.Matches(msg, k.Bottom):
		p.cursor = len(p.links) - 1
	case s == "enter" || key.Matches(msg, k.Open, k.OpenLink):
		pick = p.cursor
	case len(s) == 1 && s[0] >= '1' && s[0] <= '9':
		if n := int(s[0] - '1'); n < len(p.links) {
			pick = n
		}
	}
	if pick < 0 {
		return nil
	}
	a.picker = nil
	return a.openCmd(p.links[pick].URL)
}

func (a *app) renderPicker() string {
	p := a.picker
	// The border and padding take two cells on each side, one more keeps the box off the edges.
	inner := max(min(a.width-6, maxPickerWidth), 1)
	// Title, hint and the blank lines around the rows, plus the border.
	rows := max(min(len(p.links), a.height-6), 1)
	start := max(p.cursor-rows+1, 0)

	lines := []string{a.st.accentBold.Render(pickerTitle), ""}
	for i, l := range p.links[start:min(start+rows, len(p.links))] {
		i += start
		num := "  "
		if i < 9 {
			num = strconv.Itoa(i+1) + " "
		}
		idSt, titleSt := a.st.bold, a.st.text
		if i == p.cursor {
			idSt, titleSt = a.st.accentBold, a.st.accentBold
		}
		lines = append(lines, a.gutter(i == p.cursor)+a.st.muted.Render(num)+a.linkName(l, idSt, titleSt))
	}
	lines = append(lines, "", a.st.muted.Render("enter open · 1-9 pick · esc close"))

	w := 0
	for _, l := range lines {
		w = max(w, ansi.StringWidth(l))
	}
	w = min(w, inner)
	for i, l := range lines {
		lines[i] = fit(l, w)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(a.st.p.accent).
		Padding(0, 1).
		Render(strings.Join(lines, "\n"))
}

// overlay draws box centered over base, a screen of a.width x a.height cells.
func (a *app) overlay(base, box string) string {
	x := max((a.width-lipgloss.Width(box))/2, 0)
	y := max((a.height-lipgloss.Height(box))/2, 0)
	c := lipgloss.NewCompositor(lipgloss.NewLayer(base), lipgloss.NewLayer(box).X(x).Y(y).Z(1))
	return lipgloss.NewCanvas(a.width, a.height).Compose(c).Render()
}
