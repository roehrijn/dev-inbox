package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/github"
	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/store"
)

const preferredTeam = "acme-co/control-plane"

var urgency = json.RawMessage(`{"badge":"P1","level":"crit"}`)

func teamConfig(dir string) Config {
	cfg := testConfig(dir)
	cfg.PreferredTeam = preferredTeam
	cfg.Greylist = DefaultGreylist
	return cfg
}

// seedTeamCache stores members as a previous run would have cached them.
func seedTeamCache(t *testing.T, dir, team string, members ...string) {
	t.Helper()
	raw, err := json.Marshal(cachedTeam{Team: team, Members: members})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SetMeta(context.Background(), metaPreferredTeam, string(raw)); err != nil {
		t.Fatalf("seed team cache: %v", err)
	}
}

func seedAnnotation(t *testing.T, dir string, a model.Annotation) {
	t.Helper()
	a.UpdatedAt = baseTime
	ev, err := model.NewEvent(model.EventAnnotated, a.PR, model.EventData{Change: a})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SetAnnotation(context.Background(), a, ev); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
}

// waitTeamCached waits until the cache holds n members.
func (r *running) waitTeamCached(t *testing.T, n int) cachedTeam {
	t.Helper()
	var c cachedTeam
	eventually(t, "preferred team cache", func() bool {
		raw, ok := r.meta(t, metaPreferredTeam)
		c = cachedTeam{}
		return ok && json.Unmarshal([]byte(raw), &c) == nil && len(c.Members) == n
	})
	return c
}

func (r *running) scores(t *testing.T) map[model.PRKey]int {
	t.Helper()
	list := request[api.ListResponse](t, r.nc, api.SubjectList, api.ListRequest{})
	if list.Error != "" {
		t.Fatalf("list error: %s", list.Error)
	}
	res := map[model.PRKey]int{}
	for _, it := range list.Items {
		res[it.PR.Key] = it.Score
	}
	return res
}

func (r *running) annotate(t *testing.T, a model.Annotation) {
	t.Helper()
	if resp := request[api.AnnotateResponse](t, r.nc, api.SubjectAnnotate, api.AnnotateRequest{Annotation: a}); resp.Error != "" {
		t.Fatalf("annotate %s: %s", a.Key, resp.Error)
	}
}

func eventScore(t *testing.T, ev streamEvent) int {
	t.Helper()
	var data model.EventData
	if err := json.Unmarshal(ev.ce.Data, &data); err != nil {
		t.Fatalf("decode event data: %v", err)
	}
	return data.Score
}

func TestListScores(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	preferred := requestedPR(1)
	preferred.Author = "alice"
	greylisted := openPR(2, "dependabot[bot]")
	greylisted.ReviewRequests = []model.ReviewRequest{{Team: testViewer.Teams[0]}}
	plain := openPR(3, "other")
	seed(t, dir, true, preferred, greylisted, plain)
	gh := newFake(preferred, greylisted, plain)
	gh.teams[preferredTeam] = []string{"alice", "bob"}

	r := startDaemon(t, teamConfig(dir), gh)
	r.waitTeamCached(t, 2)

	want := map[model.PRKey]int{
		preferred.Key:  derive.DefaultWeights.PreferredTeam + derive.DefaultWeights.Direct,
		greylisted.Key: derive.DefaultWeights.Greylist,
		plain.Key:      0,
	}
	if got := r.scores(t); !reflect.DeepEqual(got, want) {
		t.Errorf("scores = %v, want %v", got, want)
	}

	r.annotate(t, model.Annotation{PR: preferred.Key, Key: "linear.urgency", Value: urgency})
	want[preferred.Key] += derive.DefaultWeights.Urgent
	if got := r.scores(t); !reflect.DeepEqual(got, want) {
		t.Errorf("scores after urgency = %v, want %v", got, want)
	}
	get := request[api.GetResponse](t, r.nc, api.SubjectGet, api.GetRequest{PR: preferred.Key})
	if get.Error != "" || get.Item == nil || get.Item.Score != want[preferred.Key] {
		t.Errorf("get = %+v, want score %d", get, want[preferred.Key])
	}
}

// customWeights differ from derive.DefaultWeights in every term, and Greylist is positive.
var customWeights = derive.Weights{Urgent: 3, PreferredTeam: 5, Greylist: 11, Direct: 2}

func TestListScoresUseConfiguredWeights(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	preferred := requestedPR(1)
	preferred.Author = "alice"
	greylisted := openPR(2, "dependabot[bot]")
	direct := requestedPR(3)
	seed(t, dir, true, preferred, greylisted, direct)
	gh := newFake(preferred, greylisted, direct)
	gh.teams[preferredTeam] = []string{"alice", "bob"}

	cfg := teamConfig(dir)
	cfg.Weights = customWeights
	r := startDaemon(t, cfg, gh)
	r.waitTeamCached(t, 2)

	want := map[model.PRKey]int{
		preferred.Key:  customWeights.PreferredTeam + customWeights.Direct,
		greylisted.Key: customWeights.Greylist,
		direct.Key:     customWeights.Direct,
	}
	if got := r.scores(t); !reflect.DeepEqual(got, want) {
		t.Errorf("scores = %v, want %v", got, want)
	}

	r.annotate(t, model.Annotation{PR: preferred.Key, Key: "linear.urgency", Value: urgency})
	want[preferred.Key] += customWeights.Urgent
	if got := r.scores(t); !reflect.DeepEqual(got, want) {
		t.Errorf("scores after urgency = %v, want %v", got, want)
	}
	get := request[api.GetResponse](t, r.nc, api.SubjectGet, api.GetRequest{PR: preferred.Key})
	if get.Error != "" || get.Item == nil || get.Item.Score != want[preferred.Key] {
		t.Errorf("get = %+v, want score %d", get, want[preferred.Key])
	}
}

func TestEventsCarryConfiguredWeights(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed(t, dir, true)
	pr := requestedPR(1)
	pr.Author = "alice"
	gh := newFake(pr)
	gh.teams[preferredTeam] = []string{"alice"}
	gh.notify(github.Notification{ThreadID: "t", Reason: "review_requested", PR: pr.Key, UpdatedAt: baseTime})

	cfg := teamConfig(dir)
	cfg.Weights = customWeights
	r := startDaemon(t, cfg, gh)
	evs := r.settledEvents(t, 2)
	if got := eventTypes(evs); !reflect.DeepEqual(got, []string{"devinbox.pr.discovered", "devinbox.pr.review_requested"}) {
		t.Fatalf("event types = %q", got)
	}
	score := customWeights.PreferredTeam + customWeights.Direct
	for _, ev := range evs {
		if got := eventScore(t, ev); got != score {
			t.Errorf("%s score = %d, want %d", ev.ce.Type, got, score)
		}
	}

	r.annotate(t, model.Annotation{PR: pr.Key, Key: "linear.urgency", Value: urgency})
	evs = r.settledEvents(t, 3)
	if got, want := evs[2].ce.Type, "devinbox.pr.annotated"; got != want {
		t.Fatalf("third event type = %q, want %q", got, want)
	}
	if got, want := eventScore(t, evs[2]), score+customWeights.Urgent; got != want {
		t.Errorf("pr.annotated score = %d, want %d", got, want)
	}
}

func TestPreferredTeamDisabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pr := openPR(1, "alice")
	seed(t, dir, true, pr)
	seedTeamCache(t, dir, preferredTeam, "alice")
	gh := newFake(pr)
	gh.teams[preferredTeam] = []string{"alice"}

	r := startDaemon(t, testConfig(dir), gh)
	eventually(t, "notification polls", func() bool { return len(gh.callsOf("Notifications")) >= 2 })

	if n := len(gh.callsOf("TeamMembers")); n != 0 {
		t.Errorf("TeamMembers calls = %d, want 0 without a preferred team", n)
	}
	if got := r.scores(t)[pr.Key]; got != 0 {
		t.Errorf("score = %d, want 0 without a preferred team", got)
	}
}

func TestPreferredTeamFetchedOnceAndCached(t *testing.T) {
	t.Parallel()
	gh := newFake()
	gh.teams[preferredTeam] = []string{"alice", "bob"}

	r := startDaemon(t, teamConfig(t.TempDir()), gh)
	cached := r.waitTeamCached(t, 2)
	if want := (cachedTeam{Team: preferredTeam, Members: []string{"alice", "bob"}}); !reflect.DeepEqual(cached, want) {
		t.Errorf("cache = %+v, want %+v", cached, want)
	}

	eventually(t, "notification polls", func() bool { return len(gh.callsOf("Notifications")) >= 3 })
	calls := gh.callsOf("TeamMembers")
	if len(calls) != 1 || calls[0].query != preferredTeam {
		t.Errorf("TeamMembers calls = %+v, want one for %s", calls, preferredTeam)
	}
}

func TestPreferredTeamOfflineRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pr := openPR(1, "alice")
	seed(t, dir, true, pr)
	online := newFake(pr)
	online.teams[preferredTeam] = []string{"alice"}
	first := startDaemon(t, teamConfig(dir), online)
	first.waitTeamCached(t, 1)
	first.stop()

	offline := newFake(pr)
	offline.viewerErr = errors.New("network unreachable")
	offline.teamErr = errors.New("network unreachable")
	second := startDaemon(t, teamConfig(dir), offline)
	eventually(t, "team fetch attempt", func() bool { return len(offline.callsOf("TeamMembers")) >= 1 })

	if got := second.scores(t)[pr.Key]; got != derive.DefaultWeights.PreferredTeam {
		t.Errorf("score after offline restart = %d, want %d", got, derive.DefaultWeights.PreferredTeam)
	}
}

func TestPreferredTeamCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		team string
		want int
	}{
		{name: "same team", team: preferredTeam, want: derive.DefaultWeights.PreferredTeam},
		{name: "same team in other case", team: "Acme-CO/Control-Plane", want: derive.DefaultWeights.PreferredTeam},
		{name: "other team", team: "acme-co/other", want: 0},
		{name: "other org", team: "other-org/control-plane", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			pr := openPR(1, "alice")
			seed(t, dir, true, pr)
			seedTeamCache(t, dir, preferredTeam, "alice")
			gh := newFake(pr)
			gh.teamErr = errors.New("network unreachable")

			cfg := teamConfig(dir)
			cfg.PreferredTeam = tt.team
			r := startDaemon(t, cfg, gh)
			eventually(t, "team fetch attempt", func() bool { return len(gh.callsOf("TeamMembers")) >= 1 })

			if got := r.scores(t)[pr.Key]; got != tt.want {
				t.Errorf("score = %d, want %d", got, tt.want)
			}
		})
	}
}

type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), substr)
}

func TestPreferredTeamNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pr := openPR(1, "alice")
	seed(t, dir, true, pr)
	seedTeamCache(t, dir, preferredTeam, "alice")
	gh := newFake(pr)
	logs := &logBuffer{}

	cfg := teamConfig(dir)
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	r := startDaemon(t, cfg, gh)
	if cached := r.waitTeamCached(t, 0); cached.Team != preferredTeam {
		t.Errorf("cache = %+v, want the configured team with no members", cached)
	}
	eventually(t, "notification polls", func() bool { return len(gh.callsOf("Notifications")) >= 3 })

	if got := r.scores(t)[pr.Key]; got != 0 {
		t.Errorf("score = %d, want 0 for an unknown team", got)
	}
	if n := len(gh.callsOf("TeamMembers")); n != 1 {
		t.Errorf("TeamMembers calls = %d, want 1", n)
	}
	if n := logs.count("preferred team not found"); n != 1 {
		t.Errorf("not-found warnings = %d, want 1", n)
	}
}

func TestAnnotateUpdatesScore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pr := requestedPR(1)
	seed(t, dir, true, pr)
	cfg := testConfig(dir)
	cfg.Greylist = DefaultGreylist
	r := startDaemon(t, cfg, newFake(pr))

	prLevel := model.Annotation{PR: pr.Key, Key: "linear.urgency", Value: urgency}
	commentLevel := model.Annotation{PR: pr.Key, CommentID: "C_1", Key: "linear.urgency", Value: urgency}
	deleted := prLevel
	deleted.Value = json.RawMessage(`null`)
	steps := []struct {
		name string
		a    model.Annotation
		want int
	}{
		{name: "set urgency", a: prLevel, want: derive.DefaultWeights.Direct + derive.DefaultWeights.Urgent},
		{name: "replace urgency", a: prLevel, want: derive.DefaultWeights.Direct + derive.DefaultWeights.Urgent},
		{name: "comment-level urgency", a: commentLevel, want: derive.DefaultWeights.Direct + derive.DefaultWeights.Urgent},
		{name: "delete urgency", a: deleted, want: derive.DefaultWeights.Direct},
	}
	if got := r.scores(t)[pr.Key]; got != derive.DefaultWeights.Direct {
		t.Fatalf("initial score = %d, want %d", got, derive.DefaultWeights.Direct)
	}
	for _, s := range steps {
		r.annotate(t, s.a)
	}

	evs := r.settledEvents(t, len(steps))
	for i, s := range steps {
		if got := eventScore(t, evs[i]); got != s.want {
			t.Errorf("%s: pr.annotated score = %d, want %d", s.name, got, s.want)
		}
	}
	if got := r.scores(t)[pr.Key]; got != derive.DefaultWeights.Direct {
		t.Errorf("score after delete = %d, want %d", got, derive.DefaultWeights.Direct)
	}
}

func TestIngestEventsCarryScores(t *testing.T) {
	t.Parallel()
	t.Run("new PR", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seed(t, dir, true)
		pr := requestedPR(1)
		pr.Author = "alice"
		gh := newFake(pr)
		gh.teams[preferredTeam] = []string{"alice"}
		gh.notify(github.Notification{ThreadID: "t", Reason: "review_requested", PR: pr.Key, UpdatedAt: baseTime})

		r := startDaemon(t, teamConfig(dir), gh)
		evs := r.settledEvents(t, 2)
		if got := eventTypes(evs); !reflect.DeepEqual(got, []string{"devinbox.pr.discovered", "devinbox.pr.review_requested"}) {
			t.Fatalf("event types = %q", got)
		}
		for _, ev := range evs {
			if got := eventScore(t, ev); got != derive.DefaultWeights.PreferredTeam+derive.DefaultWeights.Direct {
				t.Errorf("%s score = %d, want %d", ev.ce.Type, got, derive.DefaultWeights.PreferredTeam+derive.DefaultWeights.Direct)
			}
		}
	})

	t.Run("annotated PR", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pr := requestedPR(1)
		seed(t, dir, true, pr)
		seedAnnotation(t, dir, model.Annotation{PR: pr.Key, Key: "linear.urgency", Value: urgency})
		current := pr
		current.Checks = model.ChecksFailure
		current.UpdatedAt = baseTime.Add(time.Minute)
		gh := newFake(current)
		gh.notify(github.Notification{ThreadID: "t", Reason: "ci_activity", PR: pr.Key, UpdatedAt: baseTime})

		r := startDaemon(t, teamConfig(dir), gh)
		var derived []streamEvent
		eventually(t, "derived events", func() bool {
			derived = nil
			for _, ev := range r.settledEvents(t, 1) {
				if ev.ce.Type != "devinbox.pr.annotated" {
					derived = append(derived, ev)
				}
			}
			return len(derived) >= 2
		})
		for _, ev := range derived {
			if got := eventScore(t, ev); got != derive.DefaultWeights.Direct+derive.DefaultWeights.Urgent {
				t.Errorf("%s score = %d, want %d", ev.ce.Type, got, derive.DefaultWeights.Direct+derive.DefaultWeights.Urgent)
			}
		}
	})
}

func TestWithAnnotation(t *testing.T) {
	t.Parallel()
	key := prKey(1)
	ann := func(commentID, k, value string) model.Annotation {
		return model.Annotation{PR: key, CommentID: commentID, Key: k, Value: json.RawMessage(value)}
	}
	existing := []model.Annotation{ann("", "a", `1`), ann("C_1", "a", `2`)}
	tests := []struct {
		name string
		set  model.Annotation
		want []model.Annotation
	}{
		{name: "add", set: ann("", "b", `3`), want: []model.Annotation{ann("", "a", `1`), ann("C_1", "a", `2`), ann("", "b", `3`)}},
		{name: "replace PR-level", set: ann("", "a", `4`), want: []model.Annotation{ann("C_1", "a", `2`), ann("", "a", `4`)}},
		{name: "replace comment-level", set: ann("C_1", "a", `4`), want: []model.Annotation{ann("", "a", `1`), ann("C_1", "a", `4`)}},
		{name: "delete with null", set: ann("", "a", `null`), want: []model.Annotation{ann("C_1", "a", `2`)}},
		{name: "delete with empty value", set: ann("C_1", "a", ``), want: []model.Annotation{ann("", "a", `1`)}},
		{name: "delete absent", set: ann("", "x", `null`), want: existing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := append([]model.Annotation(nil), existing...)
			got := withAnnotation(existing, tt.set)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("withAnnotation = %+v, want %+v", got, tt.want)
			}
			if !reflect.DeepEqual(existing, before) {
				t.Errorf("input mutated to %+v", existing)
			}
		})
	}
}
