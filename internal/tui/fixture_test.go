package tui

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

var testNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func prKey(owner, repo string, n int) model.PRKey {
	return model.PRKey{Owner: owner, Repo: repo, Number: n}
}

// item is a PR in sec. PRs of others need the viewer, so the default view lists them.
func item(k model.PRKey, sec model.Section, title string, updatedAgo time.Duration) api.InboxItem {
	return api.InboxItem{
		PR: model.PR{
			Key:      k,
			Title:    title,
			Author:   "alice",
			URL:      "https://github.com/" + k.Owner + "/" + k.Repo + "/pull/" + strconv.Itoa(k.Number),
			PRStatus: model.PRStatus{State: model.PRStateOpen, UpdatedAt: testNow.Add(-updatedAgo)},
		},
		Classification: model.Classification{Section: sec, NeedsMe: sectionTab[sec] == tabOthers},
	}
}

func badgeAnn(k model.PRKey, annKey, value string) model.Annotation {
	return model.Annotation{PR: k, Key: annKey, Value: json.RawMessage(value)}
}

// fixtureList has two review requests (one direct, scored 10), a needs_action PR with reasons,
// score 20 and badges, a draft and an involved PR. Ready to merge and waiting for review are empty.
func fixtureList() api.ListResponse {
	rrTeam := item(prKey("acme-co", "frontend", 11), model.SectionReviewRequested, "Team request, updated recently", time.Hour)
	rrDirect := item(prKey("acme-co", "frontend", 12), model.SectionReviewRequested, "Direct request, older", 5*time.Hour)
	rrDirect.Direct = true
	rrDirect.Score = 10
	rrDirect.PR.Checks = model.ChecksSuccess
	rrDirect.PR.ReviewDecision = "APPROVED"

	na := item(prKey("roehrijn", "dev-inbox", 3), model.SectionNeedsAction, "Fix the flaky webhook test that keeps failing on main for everyone", 2*time.Hour)
	na.PR.Author = "roehrijn"
	na.PR.Checks = model.ChecksFailure
	na.PR.ReviewDecision = "CHANGES_REQUESTED"
	na.Reasons = []string{"checks failing", "changes requested"}
	na.Score = 20
	na.Annotations = []model.Annotation{
		badgeAnn(na.PR.Key, "linear.urgency", `{"badge":"urgent","level":"crit"}`),
		badgeAnn(na.PR.Key, "linear.id", `"DEVOPS-42"`),
	}

	draft := item(prKey("roehrijn", "dev-inbox", 4), model.SectionDrafts, "WIP: detail pane", 3*24*time.Hour)
	draft.PR.Author = "roehrijn"
	draft.PR.IsDraft = true
	draft.PR.Checks = model.ChecksPending

	inv := item(prKey("other", "repo", 99), model.SectionInvolved, "Something I commented on", 40*time.Minute)

	return api.ListResponse{
		Viewer: model.Viewer{Login: "roehrijn"},
		Items:  []api.InboxItem{rrTeam, inv, na, draft, rrDirect},
	}
}

type fakeBackend struct {
	mu        sync.Mutex
	list      api.ListResponse
	listErr   error
	listCalls int
	details   map[model.PRKey]api.InboxItem
	gets      []model.PRKey
	plugins   []string
	pings     int
	annotated []model.Annotation
	annErr    error
}

func (f *fakeBackend) Annotate(_ context.Context, a model.Annotation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.annotated = append(f.annotated, a)
	return f.annErr
}

func (f *fakeBackend) annotations() []model.Annotation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.annotated)
}

func (f *fakeBackend) List(context.Context) (api.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.list, f.listErr
}

func (f *fakeBackend) Get(_ context.Context, k model.PRKey) (api.GetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, k)
	it, ok := f.details[k]
	if !ok {
		return api.GetResponse{}, errors.New(k.String() + " not found")
	}
	return api.GetResponse{Item: &it}, nil
}

func (f *fakeBackend) Plugins(context.Context) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	return f.plugins
}

func (f *fakeBackend) pingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings
}

func (f *fakeBackend) calls() (int, []model.PRKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, append([]model.PRKey(nil), f.gets...)
}

func newTestApp(be backend) *app {
	a := newApp(context.Background(), be, api.DefaultNATSURL)
	a.now = func() time.Time { return testNow }
	a.debounce = time.Millisecond
	a.retry = time.Millisecond
	a.setDark(false)
	return a
}

// loadedApp returns an app sized w x h that is connected and has applied list.
func loadedApp(t *testing.T, list api.ListResponse, w, h int) (*app, *fakeBackend) {
	t.Helper()
	fb := &fakeBackend{list: list, details: map[model.PRKey]api.InboxItem{}}
	a := newTestApp(fb)
	update(t, a, tea.WindowSizeMsg{Width: w, Height: h})
	drain(t, a, update(t, a, connMsg{connected: true}))
	if !a.loaded {
		t.Fatalf("app not loaded after connect")
	}
	return a, fb
}

func update(t *testing.T, a *app, msg tea.Msg) tea.Cmd {
	t.Helper()
	m, cmd := a.Update(msg)
	if m != a {
		t.Fatalf("Update returned a different model")
	}
	return cmd
}

// drain runs cmd and every command produced while feeding the results back into a, and
// returns the messages in arrival order. It returns once nothing has arrived for a while,
// abandoning long timers such as the minute tick or flash expiry.
func drain(t *testing.T, a *app, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	results := make(chan tea.Msg, 64)
	pending := 0
	start := func(c tea.Cmd) {
		if c != nil {
			pending++
			go func() { results <- c() }()
		}
	}
	start(cmd)
	var msgs []tea.Msg
	for pending > 0 {
		select {
		case msg := <-results:
			pending--
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, c := range batch {
					start(c)
				}
				continue
			}
			msgs = append(msgs, msg)
			start(update(t, a, msg))
		case <-time.After(500 * time.Millisecond):
			return msgs
		}
	}
	return msgs
}

func press(t *testing.T, a *app, keys ...string) {
	t.Helper()
	for _, k := range keys {
		update(t, a, keyPress(k))
	}
}

func keyPress(k string) tea.KeyPressMsg {
	switch k {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "shift+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

func screen(a *app) string { return ansi.Strip(a.View().Content) }

func selectedKey(t *testing.T, a *app) string {
	t.Helper()
	e, ok := a.selected()
	if !ok {
		return ""
	}
	s := a.sections[e.sec]
	if e.row < 0 {
		return "header:" + string(s.id)
	}
	return s.items[e.row].PR.Key.String()
}

func assertContains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in:\n%s", sub, s)
		}
	}
}

// fixtureDetail is the full GetResponse item of roehrijn/dev-inbox#3 from fixtureList.
func fixtureDetail() api.InboxItem {
	it := fixtureList().Items[2]
	k := it.PR.Key
	it.PR.BaseRef, it.PR.HeadRef = "main", "fix/webhook"
	it.PR.Additions, it.PR.Deletions, it.PR.ChangedFiles = 120, 34, 7
	it.PR.CreatedAt = testNow.Add(-72 * time.Hour)
	it.PR.Labels = []string{"bug", "area/sync"}
	it.PR.CheckRuns = []model.CheckRun{
		{Name: "unit", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "e2e", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Name: "lint", Status: "IN_PROGRESS"},
	}
	it.PR.Reviews = []model.Review{
		{ID: "r1", Author: "bob", State: "COMMENTED", SubmittedAt: testNow.Add(-3 * time.Hour)},
		{ID: "r2", Author: "bob", State: "CHANGES_REQUESTED", SubmittedAt: testNow.Add(-2 * time.Hour)},
	}
	it.PR.ReviewRequests = []model.ReviewRequest{{Login: "carol"}, {Team: "acme-co/eng"}}
	it.PR.Comments = []model.Comment{
		{ID: "c1", Kind: model.CommentIssue, Author: "bob", Body: "Please **fix** this.\n\n```go\nfmt.Println(1)\n```", CreatedAt: testNow.Add(-90 * time.Minute)},
		{ID: "c2", Kind: model.CommentInline, Author: "carol", Path: "internal/x.go", Line: 12, Body: "nit: rename `foo` to `bar`", CreatedAt: testNow.Add(-10 * time.Minute)},
	}
	it.Annotations = append(it.Annotations,
		model.Annotation{PR: k, CommentID: "c2", Key: "ai.tone", Value: json.RawMessage(`{"badge":"nitpick"}`)},
		model.Annotation{PR: k, CommentID: "gone", Key: "ai.orphan", Value: json.RawMessage(`{"score":0.5}`)},
	)
	return it
}
