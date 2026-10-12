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

const (
	preferredTeam = "acme-co/control-plane"
	teamPoints    = 30
)

var urgency = json.RawMessage(`{"badge":"P1","level":"crit"}`)

// requestedScore is the score of a PR with a direct review request and nothing else: the request is an
// attention reason, so it also earns the needs-me bonus.
var requestedScore = derive.DefaultWeights.Direct + derive.DefaultWeights.NeedsMe

func teamConfig(dir string) Config {
	cfg := testConfig(dir)
	cfg.PreferredTeams = map[string]int{preferredTeam: teamPoints}
	cfg.Greylist = DefaultGreylist
	return cfg
}

// seedTeamCache stores raw as a previous run would have cached the preferred teams.
func seedTeamCache(t *testing.T, dir, raw string) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SetMeta(context.Background(), metaPreferredTeams, raw); err != nil {
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

// waitTeamsCached waits until the cache holds exactly want.
func (r *running) waitTeamsCached(t *testing.T, want map[string][]string) {
	t.Helper()
	var got map[string][]string
	eventually(t, "preferred teams cache", func() bool {
		raw, ok := r.meta(t, metaPreferredTeams)
		got = nil
		return ok && json.Unmarshal([]byte(raw), &got) == nil && reflect.DeepEqual(got, want)
	})
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
	r.waitTeamsCached(t, map[string][]string{preferredTeam: {"alice", "bob"}})

	want := map[model.PRKey]int{
		preferred.Key:  teamPoints + requestedScore,
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
var customWeights = derive.Weights{Urgent: 3, Greylist: 11, Direct: 2}

const customTeamPoints = 5

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
	cfg.PreferredTeams[preferredTeam] = customTeamPoints
	cfg.Weights = customWeights
	r := startDaemon(t, cfg, gh)
	r.waitTeamsCached(t, map[string][]string{preferredTeam: {"alice", "bob"}})

	want := map[model.PRKey]int{
		preferred.Key:  customTeamPoints + customWeights.Direct,
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
	cfg.PreferredTeams[preferredTeam] = customTeamPoints
	cfg.Weights = customWeights
	r := startDaemon(t, cfg, gh)
	evs := r.settledEvents(t, 2)
	if got := eventTypes(evs); !reflect.DeepEqual(got, []string{"devinbox.pr.discovered", "devinbox.pr.review_requested"}) {
		t.Fatalf("event types = %q", got)
	}
	score := customTeamPoints + customWeights.Direct
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

// TestPreferredTeamsScore ingests PRs only after every team was fetched, so even the first events
// carry the team points.
func TestPreferredTeamsScore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed(t, dir, true)
	authors := []string{"alice", "bob", "Carol", "dave", "eve"}
	var prs []model.PR
	for i, a := range authors {
		prs = append(prs, openPR(i+1, a))
	}
	gh := newFake(prs...)
	gh.teams["org/a"] = []string{"alice", "carol", "dave"}
	gh.teams["org/b"] = []string{"bob", "dave"}
	gh.teams["org/c"] = []string{"carol"}
	for _, pr := range prs {
		gh.notify(github.Notification{ThreadID: pr.NodeID, Reason: "mention", PR: pr.Key, UpdatedAt: baseTime})
	}

	cfg := testConfig(dir)
	cfg.PreferredTeams = map[string]int{"org/a": 30, "org/b": -10, "org/c": 50}
	r := startDaemon(t, cfg, gh)

	want := map[string]int{
		"alice": 30,
		"bob":   -10,
		"Carol": 50, // highest of a and c, matched case-insensitively
		"dave":  30, // highest of a and the negative b
		"eve":   0,
	}
	got := map[string]int{}
	eventually(t, "a discovered event per PR", func() bool {
		for _, ev := range r.streamEvents(t) {
			if ev.ce.Type != "devinbox.pr.discovered" {
				continue
			}
			var data model.EventData
			if err := json.Unmarshal(ev.ce.Data, &data); err != nil {
				t.Fatalf("decode event data: %v", err)
			}
			got[data.PR.Author] = data.Score
		}
		return len(got) == len(prs)
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("discovered scores by author = %v, want %v", got, want)
	}
	if calls := gh.callsOf("TeamMembers"); len(calls) != 3 {
		t.Errorf("TeamMembers calls = %+v, want one per team", calls)
	}
}

func TestPreferredTeamDisabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pr := openPR(1, "alice")
	seed(t, dir, true, pr)
	seedTeamCache(t, dir, `{"`+preferredTeam+`":["alice"]}`)
	gh := newFake(pr)
	gh.teams[preferredTeam] = []string{"alice"}

	cfg := testConfig(dir)
	cfg.PreferredTeams = map[string]int{}
	r := startDaemon(t, cfg, gh)
	eventually(t, "notification polls", func() bool { return len(gh.callsOf("Notifications")) >= 2 })

	if n := len(gh.callsOf("TeamMembers")); n != 0 {
		t.Errorf("TeamMembers calls = %d, want 0 without preferred teams", n)
	}
	if got := r.scores(t)[pr.Key]; got != 0 {
		t.Errorf("score = %d, want 0 without preferred teams", got)
	}
}

func TestPreferredTeamsFetchedOnceAndCached(t *testing.T) {
	t.Parallel()
	gh := newFake()
	gh.teams[preferredTeam] = []string{"alice", "bob"}
	gh.teams["acme-co/other"] = []string{"carol"}

	cfg := teamConfig(t.TempDir())
	cfg.PreferredTeams["acme-co/other"] = 10
	r := startDaemon(t, cfg, gh)
	r.waitTeamsCached(t, map[string][]string{preferredTeam: {"alice", "bob"}, "acme-co/other": {"carol"}})

	eventually(t, "notification polls", func() bool { return len(gh.callsOf("Notifications")) >= 3 })
	var teams []string
	for _, c := range gh.callsOf("TeamMembers") {
		teams = append(teams, c.query)
	}
	if want := []string{preferredTeam, "acme-co/other"}; !reflect.DeepEqual(teams, want) {
		t.Errorf("TeamMembers calls = %q, want %q", teams, want)
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
	first.waitTeamsCached(t, map[string][]string{preferredTeam: {"alice"}})
	first.stop()

	offline := newFake(pr)
	offline.viewerErr = errors.New("network unreachable")
	offline.teamErr = errors.New("network unreachable")
	second := startDaemon(t, teamConfig(dir), offline)
	eventually(t, "team fetch attempt", func() bool { return len(offline.callsOf("TeamMembers")) >= 1 })

	if got := second.scores(t)[pr.Key]; got != teamPoints {
		t.Errorf("score after offline restart = %d, want %d", got, teamPoints)
	}
}

func TestPreferredTeamCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		cache string
		teams map[string]int
		want  map[model.PRKey]int
	}{
		{
			name:  "same team",
			cache: `{"acme-co/control-plane":["alice"]}`,
			teams: map[string]int{preferredTeam: teamPoints},
			want:  map[model.PRKey]int{prKey(1): teamPoints, prKey(2): 0},
		},
		{
			name:  "same team in other case",
			cache: `{"acme-co/control-plane":["alice"]}`,
			teams: map[string]int{"Acme-CO/Control-Plane": teamPoints},
			want:  map[model.PRKey]int{prKey(1): teamPoints, prKey(2): 0},
		},
		{
			name:  "unconfigured team ignored",
			cache: `{"acme-co/control-plane":["alice"],"acme-co/other":["bob"]}`,
			teams: map[string]int{preferredTeam: teamPoints},
			want:  map[model.PRKey]int{prKey(1): teamPoints, prKey(2): 0},
		},
		{
			name:  "only other teams cached",
			cache: `{"acme-co/other":["alice"],"other-org/control-plane":["bob"]}`,
			teams: map[string]int{preferredTeam: teamPoints},
			want:  map[model.PRKey]int{prKey(1): 0, prKey(2): 0},
		},
		{
			name:  "two configured teams",
			cache: `{"acme-co/control-plane":["alice"],"acme-co/other":["bob"]}`,
			teams: map[string]int{preferredTeam: teamPoints, "acme-co/other": -10},
			want:  map[model.PRKey]int{prKey(1): teamPoints, prKey(2): -10},
		},
		{
			name:  "corrupt cache",
			cache: `{"acme-co/control-plane":`,
			teams: map[string]int{preferredTeam: teamPoints},
			want:  map[model.PRKey]int{prKey(1): 0, prKey(2): 0},
		},
		{
			name:  "single-team format",
			cache: `{"team":"acme-co/control-plane","members":["alice"]}`,
			teams: map[string]int{preferredTeam: teamPoints},
			want:  map[model.PRKey]int{prKey(1): 0, prKey(2): 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			alice, bob := openPR(1, "alice"), openPR(2, "bob")
			seed(t, dir, true, alice, bob)
			seedTeamCache(t, dir, tt.cache)
			gh := newFake(alice, bob)
			gh.teamErr = errors.New("network unreachable")

			cfg := teamConfig(dir)
			cfg.PreferredTeams = tt.teams
			r := startDaemon(t, cfg, gh)
			eventually(t, "team fetch attempt", func() bool { return len(gh.callsOf("TeamMembers")) >= len(tt.teams) })

			if got := r.scores(t); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("scores = %v, want %v", got, tt.want)
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
	alice, bob := openPR(1, "alice"), openPR(2, "bob")
	seed(t, dir, true, alice, bob)
	seedTeamCache(t, dir, `{"acme-co/control-plane":["alice"],"acme-co/gone":["bob"]}`)
	gh := newFake(alice, bob)
	gh.teams["acme-co/other"] = []string{"bob"}
	logs := &logBuffer{}

	cfg := teamConfig(dir)
	cfg.PreferredTeams["acme-co/other"] = 10
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	r := startDaemon(t, cfg, gh)
	r.waitTeamsCached(t, map[string][]string{preferredTeam: nil, "acme-co/other": {"bob"}})
	eventually(t, "notification polls", func() bool { return len(gh.callsOf("Notifications")) >= 3 })

	if got, want := r.scores(t), map[model.PRKey]int{alice.Key: 0, bob.Key: 10}; !reflect.DeepEqual(got, want) {
		t.Errorf("scores = %v, want %v", got, want)
	}
	if n := len(gh.callsOf("TeamMembers")); n != 2 {
		t.Errorf("TeamMembers calls = %d, want 2", n)
	}
	if n := logs.count("preferred team not found"); n != 1 {
		t.Errorf("not-found warnings = %d, want 1", n)
	}
	if n := logs.count("team=" + preferredTeam); n != 1 {
		t.Errorf("warnings naming %s = %d, want 1", preferredTeam, n)
	}
}

// TestRefreshTeams drives refreshTeams by hand, since the daemon would only retry after retryInterval.
func TestRefreshTeams(t *testing.T) {
	t.Parallel()
	const other = "acme-co/other"
	gh := newFake()
	gh.teams[preferredTeam] = []string{"alice"}
	gh.teams[other] = []string{"bob"}
	cfg := teamConfig(t.TempDir())
	cfg.PreferredTeams[other] = -10
	cfg.PreferredTeams["acme-co/missing"] = 7
	d, err := start(cfg, gh)
	if err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(d.close)
	ctx := context.Background()
	transient := errors.New("network unreachable")

	teams := func(cp, oth, missing []string) []derive.TeamBoost {
		return []derive.TeamBoost{
			{Team: preferredTeam, Points: teamPoints, Members: cp},
			{Team: "acme-co/missing", Points: 7, Members: missing},
			{Team: other, Points: -10, Members: oth},
		}
	}
	if got, want := d.inputs(nil).Priority.Teams, teams(nil, nil, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("teams before refresh = %+v, want %+v", got, want)
	}

	steps := []struct {
		name      string
		change    func()
		wait      time.Duration
		teams     []derive.TeamBoost
		wantCache map[string][]string
	}{
		{
			name:      "missing team",
			wait:      viewerRefreshInterval,
			teams:     teams([]string{"alice"}, []string{"bob"}, nil),
			wantCache: map[string][]string{preferredTeam: {"alice"}, other: {"bob"}, "acme-co/missing": nil},
		},
		{
			name: "transient failure keeps previous members",
			change: func() {
				gh.teamErrs[preferredTeam] = transient
				gh.teams[other] = []string{"bob", "dave"}
			},
			wait:      retryInterval,
			teams:     teams([]string{"alice"}, []string{"bob", "dave"}, nil),
			wantCache: map[string][]string{preferredTeam: {"alice"}, other: {"bob", "dave"}, "acme-co/missing": nil},
		},
		{
			name: "recovered",
			change: func() {
				delete(gh.teamErrs, preferredTeam)
				gh.teams[preferredTeam] = []string{"carol"}
			},
			wait:      viewerRefreshInterval,
			teams:     teams([]string{"carol"}, []string{"bob", "dave"}, nil),
			wantCache: map[string][]string{preferredTeam: {"carol"}, other: {"bob", "dave"}, "acme-co/missing": nil},
		},
	}
	for _, s := range steps {
		if s.change != nil {
			gh.mu.Lock()
			s.change()
			gh.mu.Unlock()
		}
		if got := d.refreshTeams(ctx); got != s.wait {
			t.Errorf("%s: wait = %v, want %v", s.name, got, s.wait)
		}
		if got := d.inputs(nil).Priority.Teams; !reflect.DeepEqual(got, s.teams) {
			t.Errorf("%s: teams = %+v, want %+v", s.name, got, s.teams)
		}
		raw, _, err := d.store.Meta(ctx, metaPreferredTeams)
		var cache map[string][]string
		if err == nil {
			err = json.Unmarshal([]byte(raw), &cache)
		}
		if err != nil || !reflect.DeepEqual(cache, s.wantCache) {
			t.Errorf("%s: cache = %v (err %v), want %v", s.name, cache, err, s.wantCache)
		}
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
		{name: "set urgency", a: prLevel, want: requestedScore + derive.DefaultWeights.Urgent},
		{name: "replace urgency", a: prLevel, want: requestedScore + derive.DefaultWeights.Urgent},
		{name: "comment-level urgency", a: commentLevel, want: requestedScore + derive.DefaultWeights.Urgent},
		{name: "delete urgency", a: deleted, want: requestedScore},
	}
	if got := r.scores(t)[pr.Key]; got != requestedScore {
		t.Fatalf("initial score = %d, want %d", got, requestedScore)
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
	if got := r.scores(t)[pr.Key]; got != requestedScore {
		t.Errorf("score after delete = %d, want %d", got, requestedScore)
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
			if got, want := eventScore(t, ev), teamPoints+requestedScore; got != want {
				t.Errorf("%s score = %d, want %d", ev.ce.Type, got, want)
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
			if got, want := eventScore(t, ev), requestedScore+derive.DefaultWeights.Urgent; got != want {
				t.Errorf("%s score = %d, want %d", ev.ce.Type, got, want)
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
