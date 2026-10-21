package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	glamourstyles "charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	maxBadgeWidth = 24
	// maxListedSuccesses is the number of successful check runs listed one by one;
	// beyond that they are summarized in one line.
	maxListedSuccesses = 8
	maxMarkdownCache   = 512
)

type badge struct {
	key, text, level string
}

// parseBadge reads an annotation value of the form {"badge": "...", "level": "info|warn|crit"}.
func parseBadge(ann model.Annotation) (badge, bool) {
	var v struct {
		Badge string `json:"badge"`
		Level string `json:"level"`
	}
	if err := json.Unmarshal(ann.Value, &v); err != nil || strings.TrimSpace(v.Badge) == "" {
		return badge{}, false
	}
	switch v.Level {
	case "warn", "crit":
	default:
		v.Level = "info"
	}
	return badge{key: ann.Key, text: v.Badge, level: v.Level}, true
}

// prBadges returns the badges of PR-level annotations sorted by key.
func prBadges(anns []model.Annotation) []badge {
	var out []badge
	for _, ann := range anns {
		if ann.CommentID != "" {
			continue
		}
		if b, ok := parseBadge(ann); ok {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(x, y badge) int { return strings.Compare(x.key, y.key) })
	return out
}

func (a *app) renderBadge(b badge) string {
	st := a.st.badgeInfo
	switch b.level {
	case "warn":
		st = a.st.badgeWarn
	case "crit":
		st = a.st.badgeCrit
	}
	return st.Render(ansi.Truncate(b.text, maxBadgeWidth, "…"))
}

// renderBadges renders as many badges as fit into maxW cells.
func (a *app) renderBadges(anns []model.Annotation, maxW int) string {
	var out []string
	used := 0
	for _, b := range prBadges(anns) {
		s := a.renderBadge(b)
		w := ansi.StringWidth(s)
		if len(out) > 0 {
			w++
		}
		if used+w > maxW {
			break
		}
		out = append(out, s)
		used += w
	}
	return strings.Join(out, " ")
}

// syncDetail sizes the detail viewport, re-renders its content when anything shown changed
// and fetches the selected PR when its details are not cached yet.
func (a *app) syncDetail() tea.Cmd {
	if !a.showDetail {
		return nil
	}
	w, h := a.layout().detailSize()
	a.viewport.SetWidth(w)
	a.viewport.SetHeight(h)

	e, _ := a.selected()
	sig := detailSig{entry: e, width: w, dark: a.dark, rev: a.rev}
	var cmd tea.Cmd
	if it := a.selectedItem(); it != nil {
		k := it.PR.Key
		sig.key = k
		if a.details[k] == nil && a.detailErrs[k] == nil && !a.inflight[k] {
			cmd = a.fetchDetail(k)
		}
	}
	if sig == a.detailSig || w <= 0 {
		return cmd
	}
	prev := a.detailSig
	a.detailSig = sig
	a.viewport.SetContent(a.detailContent(w))
	if prev.entry != sig.entry || prev.key != sig.key {
		a.viewport.GotoTop()
	}
	return cmd
}

func (a *app) detailContent(w int) string {
	e, ok := a.selected()
	if !ok {
		return a.st.muted.Render("No pull request selected.")
	}
	s := a.sections[e.sec]
	if e.row < 0 {
		return a.st.accentBold.Render(sectionTitles[s.id]) + "\n\n" +
			a.st.muted.Render(ansi.Wordwrap(plural(len(s.items), "pull request")+" folded. Press space to expand.", w, ""))
	}
	it := s.items[e.row]
	full := a.details[it.PR.Key]
	if full != nil {
		it = *full
	}
	return a.renderPR(it, full != nil, a.detailErrs[it.PR.Key], w)
}

func (a *app) renderPR(it api.InboxItem, full bool, loadErr error, w int) string {
	pr := it.PR
	now := a.now()
	var b strings.Builder
	line := func(s string) {
		b.WriteString(ansi.Wrap(s, w, " "))
		b.WriteByte('\n')
	}
	dot := a.st.muted.Render(" · ")

	title := pr.Title
	if title == "" {
		title = "(no title)"
	}
	line(a.st.bold.Render(title))

	meta := []string{a.st.code.Hyperlink(prURL(pr)).Render(pr.Key.String()), a.stateLabel(pr)}
	if pr.Author != "" {
		meta = append(meta, a.st.muted.Render("by ")+a.st.text.Render(pr.Author))
	}
	if age := relAge(now, pr.CreatedAt); age != "" {
		meta = append(meta, a.st.muted.Render("opened "+age))
	}
	if age := relAge(now, pr.UpdatedAt); age != "" {
		meta = append(meta, a.st.muted.Render("updated "+age))
	}
	line(strings.Join(meta, dot))
	if pr.BaseRef != "" || pr.HeadRef != "" {
		line(a.st.code.Render(orDash(pr.BaseRef)) + a.st.muted.Render(" ← ") + a.st.code.Render(orDash(pr.HeadRef)))
	}
	if pr.Additions+pr.Deletions+pr.ChangedFiles > 0 {
		line(a.st.success.Render(fmt.Sprintf("+%d", pr.Additions)) + " " +
			a.st.danger.Render(fmt.Sprintf("−%d", pr.Deletions)) + dot +
			a.st.muted.Render(plural(pr.ChangedFiles, "file")+" changed"))
	}

	var status []string
	if t := sectionTitles[it.Section]; t != "" {
		status = append(status, a.st.text.Render(t))
	}
	status = append(status, a.priorityLabel(it.Score))
	if it.Direct {
		status = append(status, a.st.accentBold.Render("★ requested from you"))
	}
	if len(it.Reasons) > 0 {
		status = append(status, a.st.accent.Render(strings.Join(it.Reasons, ", ")))
	}
	if d := a.decision(pr.ReviewDecision, it.Reasons); d != "" {
		status = append(status, d)
	}
	if c := a.conflictMarker(it); c != "" {
		status = append(status, c)
	}
	if pr.InMergeQueue {
		status = append(status, a.st.success.Render("in merge queue"))
	}
	line(strings.Join(status, dot))
	if len(pr.Labels) > 0 {
		line(a.st.muted.Render("Labels    ") + strings.Join(pr.Labels, ", "))
	}
	if len(pr.Assignees) > 0 {
		line(a.st.muted.Render("Assignees ") + strings.Join(pr.Assignees, ", "))
	}

	commentIDs := map[string]bool{}
	for _, c := range pr.Comments {
		commentIDs[c.ID] = true
	}
	a.writeScore(&b, it, w)
	a.writeLinks(&b, it.Annotations, w)
	a.writeAnnotations(&b, it.Annotations, commentIDs, w)
	a.writeChecks(&b, pr, w)
	a.writeReviews(&b, pr, w)
	a.writeComments(&b, it, full, loadErr, w)
	return strings.TrimRight(b.String(), "\n")
}

func (a *app) priorityLabel(score int) string {
	n := a.renderScore(score)
	if n == "" {
		n = a.st.muted.Render("0")
	}
	return a.st.muted.Render("Priority ") + n
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *app) stateLabel(pr model.PR) string {
	switch {
	case pr.State == model.PRStateMerged:
		return a.st.success.Render("merged")
	case pr.State == model.PRStateClosed:
		return a.st.danger.Render("closed")
	case pr.IsDraft:
		return a.st.draft.Render("draft")
	}
	return a.st.success.Render("open")
}

func (a *app) writeHeading(b *strings.Builder, title, summary string) {
	b.WriteString("\n" + a.st.accentBold.Render(title))
	if summary != "" {
		b.WriteString(" " + a.st.muted.Render(summary))
	}
	b.WriteByte('\n')
}

// writeScore lists the terms the score is made of, one line each with the signed points in a
// right-aligned column. Labels are truncated so a line never exceeds w.
func (a *app) writeScore(b *strings.Builder, it api.InboxItem, w int) {
	if len(it.ScoreTerms) == 0 {
		return
	}
	a.writeHeading(b, "Score", strconv.Itoa(it.Score))
	labelW, pointsW := 0, 0
	points := make([]string, len(it.ScoreTerms))
	for i, t := range it.ScoreTerms {
		points[i] = fmt.Sprintf("%+d", t.Points)
		labelW = max(labelW, ansi.StringWidth(t.Term))
		pointsW = max(pointsW, len(points[i]))
	}
	const indent, gap = 2, 2
	labelW = max(0, min(labelW, w-indent-gap-pointsW))
	for i, t := range it.ScoreTerms {
		label := ansi.Truncate(t.Term, labelW, "…")
		pad := strings.Repeat(" ", labelW-ansi.StringWidth(label)+gap+pointsW-len(points[i]))
		st := a.st.muted
		if t.Points > 0 {
			st = a.st.accent
		}
		line := strings.Repeat(" ", indent) + a.st.text.Render(label) + pad + st.Render(points[i])
		b.WriteString(ansi.Truncate(line, w, "") + "\n")
	}
}

// writeAnnotations lists PR-level annotations and those of comments that are not shown. The
// hidden mark is left out: the section already says the PR is hidden, its value is bookkeeping.
// Links annotations have sections of their own.
func (a *app) writeAnnotations(b *strings.Builder, anns []model.Annotation, shownComments map[string]bool, w int) {
	var list []model.Annotation
	for _, ann := range anns {
		_, isLinks := parseLinks(ann)
		switch {
		case ann.CommentID == "" && (ann.Key == derive.HiddenKey || isLinks):
		case ann.CommentID == "" || !shownComments[ann.CommentID]:
			list = append(list, ann)
		}
	}
	if len(list) == 0 {
		return
	}
	a.writeHeading(b, "Annotations", "")
	a.writeAnnotationLines(b, list, true, w)
}

// writeAnnotationLines writes one aligned line per annotation: badges rendered as such,
// any other value as raw JSON.
func (a *app) writeAnnotationLines(b *strings.Builder, anns []model.Annotation, withComment bool, w int) {
	anns = slices.Clone(anns)
	slices.SortFunc(anns, func(x, y model.Annotation) int {
		return strings.Compare(x.CommentID+"\x00"+x.Key, y.CommentID+"\x00"+y.Key)
	})
	keyW := 0
	for _, ann := range anns {
		keyW = max(keyW, ansi.StringWidth(ann.Key))
	}
	for _, ann := range anns {
		s := "  " + a.st.muted.Render(ann.Key+strings.Repeat(" ", keyW-ansi.StringWidth(ann.Key))) + "  "
		if bd, ok := parseBadge(ann); ok {
			s += a.renderBadge(bd)
		} else {
			s += rawJSON(ann.Value)
		}
		if withComment && ann.CommentID != "" {
			s += a.st.muted.Render("  on comment " + ann.CommentID)
		}
		if ann.Source != "" {
			s += a.st.muted.Render("  via " + ann.Source)
		}
		b.WriteString(ansi.Wrap(s, w, " ") + "\n")
	}
}

// humanize turns GitHub enum values such as IN_PROGRESS into "in progress".
func humanize(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", " "))
}

func rawJSON(v json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, v); err != nil {
		return string(v)
	}
	return buf.String()
}

type checkState int

const (
	checkFailed checkState = iota
	checkRunning
	checkNeutral
	checkPassed
)

func runState(r model.CheckRun) checkState {
	v := r.Conclusion
	if v == "" {
		v = r.Status // status contexts carry their state here
	}
	switch strings.ToUpper(v) {
	case "SUCCESS":
		return checkPassed
	case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return checkFailed
	case "NEUTRAL", "SKIPPED", "STALE", "COMPLETED":
		return checkNeutral
	}
	return checkRunning
}

func (a *app) writeChecks(b *strings.Builder, pr model.PR, w int) {
	if len(pr.CheckRuns) == 0 {
		if pr.Checks != model.ChecksNone {
			a.writeHeading(b, "Checks", "")
			b.WriteString("  " + a.checkIcon(pr.Checks) + " " + a.st.muted.Render(humanize(string(pr.Checks))) + "\n")
		}
		return
	}
	runs := slices.Clone(pr.CheckRuns)
	slices.SortStableFunc(runs, func(x, y model.CheckRun) int { return int(runState(x)) - int(runState(y)) })
	counts := map[checkState]int{}
	for _, r := range runs {
		counts[runState(r)]++
	}
	var summary []string
	for _, c := range []struct {
		s    checkState
		noun string
	}{{checkFailed, "failed"}, {checkRunning, "running"}, {checkNeutral, "skipped/neutral"}, {checkPassed, "passed"}} {
		if counts[c.s] > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", counts[c.s], c.noun))
		}
	}
	a.writeHeading(b, "Checks", strings.Join(summary, ", "))
	foldPassed := counts[checkPassed] > maxListedSuccesses
	for _, r := range runs {
		s := runState(r)
		if s == checkPassed && foldPassed {
			continue
		}
		icon := map[checkState]string{
			checkFailed:  a.st.danger.Render("✗"),
			checkRunning: a.st.warning.Render("●"),
			checkNeutral: a.st.muted.Render("-"),
			checkPassed:  a.st.success.Render("✓"),
		}[s]
		state := r.Conclusion
		if state == "" {
			state = r.Status
		}
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		b.WriteString(ansi.Truncate("  "+icon+" "+a.st.text.Render(name)+"  "+a.st.muted.Render(humanize(state)), w, "…") + "\n")
	}
	if foldPassed {
		b.WriteString("  " + a.st.success.Render("✓") + " " + a.st.muted.Render(plural(counts[checkPassed], "successful check")) + "\n")
	}
}

func (a *app) writeReviews(b *strings.Builder, pr model.PR, w int) {
	latest := reviewVerdicts(pr)
	if len(latest) == 0 && len(pr.ReviewRequests) == 0 {
		return
	}
	a.writeHeading(b, "Reviews", "")
	now := a.now()
	for _, r := range latest {
		var icon string
		switch r.State {
		case "APPROVED":
			icon = a.st.success.Render("✓")
		case "CHANGES_REQUESTED":
			icon = a.st.danger.Render("✗")
		default:
			icon = a.st.muted.Render("●")
		}
		s := "  " + icon + " " + a.st.text.Render(orDash(r.Author)) + " " +
			a.st.muted.Render(humanize(r.State))
		if age := relAge(now, r.SubmittedAt); age != "" {
			s += a.st.muted.Render(" · " + age)
		}
		b.WriteString(ansi.Truncate(s, w, "…") + "\n")
	}
	if len(pr.ReviewRequests) > 0 {
		var names []string
		for _, rr := range pr.ReviewRequests {
			if n := reviewRequestName(rr); n != "" {
				names = append(names, n)
			}
		}
		b.WriteString(ansi.Wrap("  "+a.st.warning.Render("○")+" "+a.st.muted.Render("awaiting ")+strings.Join(names, ", "), w, " ") + "\n")
	}
}

func (a *app) writeComments(b *strings.Builder, it api.InboxItem, full bool, loadErr error, w int) {
	comments := it.PR.Comments
	summary := ""
	if full {
		summary = fmt.Sprint(len(comments))
	}
	a.writeHeading(b, "Comments", summary)
	switch {
	case loadErr != nil:
		b.WriteString(ansi.Wrap(a.st.danger.Render("Could not load comments: "+loadErr.Error()), w, " ") + "\n")
		return
	case !full:
		b.WriteString(a.st.muted.Render("Loading comments…") + "\n")
		return
	case len(comments) == 0:
		b.WriteString(a.st.muted.Render("No comments.") + "\n")
		return
	}
	byComment := map[string][]model.Annotation{}
	for _, ann := range it.Annotations {
		if ann.CommentID != "" {
			byComment[ann.CommentID] = append(byComment[ann.CommentID], ann)
		}
	}
	now := a.now()
	for i, c := range comments {
		if i > 0 {
			b.WriteString(a.st.divider.Render(strings.Repeat("─", w)) + "\n")
		}
		head := a.st.bold.Render(orDash(c.Author))
		if age := relAge(now, c.CreatedAt); age != "" {
			head += a.st.muted.Render(" · " + age)
		}
		switch c.Kind {
		case model.CommentInline:
			loc := c.Path
			if c.Line > 0 {
				loc += fmt.Sprintf(":%d", c.Line)
			}
			head += a.st.muted.Render(" · on ") + a.st.code.Render(loc)
		case model.CommentReview:
			head += a.st.muted.Render(" · review")
		}
		b.WriteString(ansi.Wrap(head, w, " ") + "\n")
		a.writeAnnotationLines(b, byComment[c.ID], false, w)
		b.WriteString(a.renderMarkdown(c.Body, w) + "\n")
	}
}

func (a *app) renderMarkdown(body string, w int) string {
	if strings.TrimSpace(body) == "" {
		return a.st.muted.Render("  (empty)")
	}
	k := markdownKey{body: body, rendererKey: rendererKey{width: w, dark: a.dark}}
	if s, ok := a.markdown[k]; ok {
		return s
	}
	out, err := a.renderer(k.rendererKey).Render(body)
	if err != nil {
		out = ansi.Wrap(body, w, " ")
	}
	out = fitLines(strings.Trim(out, "\n"), w)
	if len(a.markdown) >= maxMarkdownCache {
		clear(a.markdown)
	}
	a.markdown[k] = out
	return out
}

func (a *app) renderer(k rendererKey) *glamour.TermRenderer {
	if r, ok := a.renderers[k]; ok {
		return r
	}
	style := glamourstyles.LightStyle
	if k.dark {
		style = glamourstyles.DarkStyle
	}
	// Glamour indents the document by two cells, which the wrap width must leave room for.
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(max(k.width-4, 10)), glamour.WithEmoji())
	if err != nil {
		r, _ = glamour.NewTermRenderer(glamour.WithStandardStyle(glamourstyles.NoTTYStyle))
	}
	a.renderers[k] = r
	return r
}

// fitLines truncates every line of s to w cells, trimming trailing padding.
func fitLines(s string, w int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(ansi.Truncate(l, w, ""), " ")
	}
	return strings.Join(lines, "\n")
}
