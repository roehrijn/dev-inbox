package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/github"
	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/store"
)

var testViewer = model.Viewer{Login: "me", Teams: []string{"org/team"}}

type ghCall struct {
	method       string
	key          model.PRKey
	query        string
	since        time.Time
	lastModified string
	at           time.Time
}

type fakeGitHub struct {
	mu            sync.Mutex
	viewer        model.Viewer
	viewerErr     error
	prs           map[model.PRKey]model.PR
	search        map[string][]model.PRKey
	notifications []github.Notification   // returned by the next Notifications call
	fetchErrs     map[model.PRKey][]error // returned by FetchPR, one per call, before the snapshot
	fetchHook     func(model.PRKey)       // runs at the start of FetchPR, outside the lock
	teams         map[string][]string     // members by "org/slug"; other teams are not found
	teamErr       error                   // returned by TeamMembers instead of the members
	teamErrs      map[string]error        // like teamErr, for a single team
	calls         []ghCall
}

func newFake(prs ...model.PR) *fakeGitHub {
	f := &fakeGitHub{
		viewer:    testViewer,
		prs:       map[model.PRKey]model.PR{},
		search:    map[string][]model.PRKey{},
		fetchErrs: map[model.PRKey][]error{},
		teams:     map[string][]string{},
		teamErrs:  map[string]error{},
	}
	for _, pr := range prs {
		f.prs[pr.Key] = pr
	}
	return f
}

func (f *fakeGitHub) record(c ghCall) {
	c.at = time.Now()
	f.calls = append(f.calls, c)
}

func (f *fakeGitHub) Viewer(context.Context) (model.Viewer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ghCall{method: "Viewer"})
	return f.viewer, f.viewerErr
}

func (f *fakeGitHub) Notifications(_ context.Context, since time.Time, lastModified string) (github.NotificationsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ghCall{method: "Notifications", since: since, lastModified: lastModified})
	if len(f.notifications) == 0 {
		return github.NotificationsResult{NotModified: lastModified != "", LastModified: lastModified}, nil
	}
	ns := f.notifications
	f.notifications = nil
	return github.NotificationsResult{Notifications: ns, LastModified: "lm-" + ns[len(ns)-1].ThreadID}, nil
}

func (f *fakeGitHub) SearchOpenPRs(_ context.Context, query string) ([]model.PRKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ghCall{method: "SearchOpenPRs", query: query})
	return f.search[query], nil
}

func (f *fakeGitHub) FetchPR(_ context.Context, key model.PRKey) (model.PR, error) {
	if f.fetchHook != nil {
		f.fetchHook(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ghCall{method: "FetchPR", key: key})
	if errs := f.fetchErrs[key]; len(errs) > 0 {
		f.fetchErrs[key] = errs[1:]
		return model.PR{}, errs[0]
	}
	pr, ok := f.prs[key]
	if !ok {
		return model.PR{}, fmt.Errorf("fetch %s: %w", key, github.ErrNotFound)
	}
	pr.FetchedAt = time.Now().UTC()
	return pr, nil
}

func (f *fakeGitHub) FetchStatuses(_ context.Context, keys []model.PRKey) (map[model.PRKey]model.PRStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ghCall{method: "FetchStatuses"})
	res := map[model.PRKey]model.PRStatus{}
	for _, k := range keys {
		if pr, ok := f.prs[k]; ok {
			res[k] = pr.PRStatus
		}
	}
	return res, nil
}

func (f *fakeGitHub) TeamMembers(_ context.Context, team string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ghCall{method: "TeamMembers", query: team})
	if f.teamErr != nil {
		return nil, f.teamErr
	}
	if err := f.teamErrs[team]; err != nil {
		return nil, err
	}
	members, ok := f.teams[team]
	if !ok {
		return nil, fmt.Errorf("team %s: %w", team, github.ErrNotFound)
	}
	return members, nil
}

func (f *fakeGitHub) notify(ns ...github.Notification) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifications = append(f.notifications, ns...)
}

func (f *fakeGitHub) callsOf(method string) []ghCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res []ghCall
	for _, c := range f.calls {
		if c.method == method {
			res = append(res, c)
		}
	}
	return res
}

func (f *fakeGitHub) fetches(key model.PRKey) int {
	n := 0
	for _, c := range f.callsOf("FetchPR") {
		if c.key == key {
			n++
		}
	}
	return n
}

var baseTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func prKey(n int) model.PRKey { return model.PRKey{Owner: "Acme-CO", Repo: "frontend", Number: n} }

func openPR(n int, author string) model.PR {
	return model.PR{
		Key: prKey(n),
		PRStatus: model.PRStatus{
			State:     model.PRStateOpen,
			HeadSHA:   fmt.Sprintf("sha-%d", n),
			UpdatedAt: baseTime,
			Checks:    model.ChecksNone,
			Mergeable: "MERGEABLE",
		},
		NodeID:    fmt.Sprintf("PR_%d", n),
		Title:     fmt.Sprintf("PR %d", n),
		URL:       fmt.Sprintf("https://github.com/acme-co/frontend/pull/%d", n),
		Author:    author,
		BaseRef:   "main",
		HeadRef:   fmt.Sprintf("branch-%d", n),
		CreatedAt: baseTime.Add(-time.Hour),
	}
}

func requestedPR(n int) model.PR {
	pr := openPR(n, "other")
	pr.ReviewRequests = []model.ReviewRequest{{Login: testViewer.Login}}
	return pr
}

func testLogger() *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

func testConfig(dir string) Config {
	return Config{
		StateDir:          dir,
		NATSPort:          -1,
		PollInterval:      20 * time.Millisecond,
		ReconcileInterval: time.Hour,
		Logger:            testLogger(),
		Weights:           derive.DefaultWeights,
	}
}

// seed stores PRs before the daemon starts, as if a previous run had ingested them.
func seed(t *testing.T, dir string, backfillDone bool, prs ...model.PR) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	for _, pr := range prs {
		if err := st.ApplySnapshot(ctx, pr, nil); err != nil {
			t.Fatalf("seed %s: %v", pr.Key, err)
		}
	}
	if backfillDone {
		if err := st.SetMeta(ctx, metaBackfillDone, "seeded"); err != nil {
			t.Fatalf("seed backfill marker: %v", err)
		}
	}
}

type running struct {
	d    *daemon
	nc   *nats.Conn
	stop func()
}

func startDaemon(t *testing.T, cfg Config, gh GitHub) *running {
	t.Helper()
	d, err := start(cfg, gh)
	if err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.run(ctx) }()
	nc, err := nats.Connect(d.bus.ClientURL())
	if err != nil {
		cancel()
		<-done
		t.Fatalf("connect to bus: %v", err)
	}
	r := &running{d: d, nc: nc}
	r.stop = sync.OnceFunc(func() {
		nc.Close()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	t.Cleanup(r.stop)
	return r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *running) meta(t *testing.T, key string) (string, bool) {
	t.Helper()
	v, ok, err := r.d.store.Meta(context.Background(), key)
	if err != nil {
		t.Fatalf("meta %s: %v", key, err)
	}
	return v, ok
}

func (r *running) storedPR(t *testing.T, key model.PRKey) (model.PR, bool) {
	t.Helper()
	pr, ok, err := r.d.store.GetPR(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return pr, ok
}

func (r *running) pending(t *testing.T) int {
	t.Helper()
	evs, err := r.d.store.PendingEvents(context.Background(), 1000)
	if err != nil {
		t.Fatalf("pending events: %v", err)
	}
	return len(evs)
}

type streamEvent struct {
	subject string
	ce      api.CloudEvent
}

// streamEvents reads every message of the events stream.
func (r *running) streamEvents(t *testing.T) []streamEvent {
	t.Helper()
	ctx := context.Background()
	js, err := jetstream.New(r.nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	s, err := js.Stream(ctx, api.StreamName)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	var res []streamEvent
	for seq := info.State.FirstSeq; info.State.Msgs > 0 && seq <= info.State.LastSeq; seq++ {
		msg, err := s.GetMsg(ctx, seq)
		if err != nil {
			t.Fatalf("get message %d: %v", seq, err)
		}
		var ce api.CloudEvent
		if err := json.Unmarshal(msg.Data, &ce); err != nil {
			t.Fatalf("decode message %d: %v", seq, err)
		}
		res = append(res, streamEvent{subject: msg.Subject, ce: ce})
	}
	return res
}

// settledEvents waits until the outbox is drained and the stream holds at least min events.
func (r *running) settledEvents(t *testing.T, min int) []streamEvent {
	t.Helper()
	var evs []streamEvent
	eventually(t, fmt.Sprintf("%d published events", min), func() bool {
		if r.pending(t) > 0 {
			return false
		}
		evs = r.streamEvents(t)
		return len(evs) >= min
	})
	return evs
}

func eventTypes(evs []streamEvent) []string {
	var res []string
	for _, ev := range evs {
		res = append(res, ev.ce.Type)
	}
	return res
}

func request[Resp any](t *testing.T, nc *nats.Conn, subject string, req any) Resp {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := nc.Request(subject, data, 5*time.Second)
	if err != nil {
		t.Fatalf("request %s: %v", subject, err)
	}
	var resp Resp
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("decode %s response %s: %v", subject, msg.Data, err)
	}
	return resp
}

func TestBackfillEmitsOnlyDiscovered(t *testing.T) {
	t.Parallel()
	a, b, c := requestedPR(1), openPR(2, "other"), openPR(3, testViewer.Login)
	b.Comments = []model.Comment{{ID: "C_1", Kind: model.CommentIssue, Author: "other", Body: "hi", CreatedAt: baseTime}}
	gh := newFake(a, b, c)
	gh.search["involves:@me"] = []model.PRKey{a.Key, b.Key}
	gh.search["review-requested:@me"] = []model.PRKey{b.Key, c.Key}
	gh.search["reviewed-by:@me"] = []model.PRKey{a.Key}

	r := startDaemon(t, testConfig(t.TempDir()), gh)
	eventually(t, "backfill marker", func() bool { _, ok := r.meta(t, metaBackfillDone); return ok })

	var queries []string
	for _, c := range gh.callsOf("SearchOpenPRs") {
		queries = append(queries, c.query)
	}
	if !slices.Equal(queries, backfillQueries) {
		t.Errorf("search queries = %q, want %q", queries, backfillQueries)
	}
	for _, pr := range []model.PR{a, b, c} {
		if n := gh.fetches(pr.Key); n != 1 {
			t.Errorf("FetchPR(%s) calls = %d, want 1", pr.Key, n)
		}
		if _, ok := r.storedPR(t, pr.Key); !ok {
			t.Errorf("%s not stored", pr.Key)
		}
	}

	evs := r.settledEvents(t, 3)
	want := []string{"devinbox.pr.discovered", "devinbox.pr.discovered", "devinbox.pr.discovered"}
	if got := eventTypes(evs); !slices.Equal(got, want) {
		t.Errorf("event types = %q, want %q", got, want)
	}
}

func TestBackfillSkipsInaccessiblePRs(t *testing.T) {
	t.Parallel()
	existing, missing := openPR(1, "other"), prKey(404)
	gh := newFake(existing)
	gh.search["involves:@me"] = []model.PRKey{missing, existing.Key}

	r := startDaemon(t, testConfig(t.TempDir()), gh)
	// The marker waits for every backfilled PR, so a retried missing PR would hold it back.
	eventually(t, "backfill marker", func() bool { _, ok := r.meta(t, metaBackfillDone); return ok })
	if n := gh.fetches(missing); n != 1 {
		t.Errorf("FetchPR(missing) calls = %d, want 1", n)
	}
	if _, ok := r.storedPR(t, missing); ok {
		t.Error("missing PR was stored")
	}
	if _, ok := r.storedPR(t, existing.Key); !ok {
		t.Error("existing PR not stored")
	}
}

func TestNotificationTracksNewPR(t *testing.T) {
	t.Parallel()
	pr := requestedPR(7)
	gh := newFake(pr)
	updated := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	gh.notify(github.Notification{ThreadID: "t1", Reason: "review_requested", PR: pr.Key, UpdatedAt: updated})

	r := startDaemon(t, testConfig(t.TempDir()), gh)
	eventually(t, "PR stored", func() bool { _, ok := r.storedPR(t, pr.Key); return ok })

	evs := r.settledEvents(t, 2)
	var subjects []string
	for _, ev := range evs {
		subjects = append(subjects, ev.subject)
	}
	wantSubjects := []string{
		"devinbox.events.pr.acme-co.frontend.7.discovered",
		"devinbox.events.pr.acme-co.frontend.7.review_requested",
	}
	if !slices.Equal(subjects, wantSubjects) {
		t.Errorf("event subjects = %q, want %q", subjects, wantSubjects)
	}

	// The cursor follows GitHub's clock and Last-Modified is passed on.
	eventually(t, "poll with Last-Modified", func() bool {
		return slices.ContainsFunc(gh.callsOf("Notifications"), func(c ghCall) bool { return c.lastModified == "lm-t1" })
	})
	since, _ := r.meta(t, metaNotificationsSince)
	if got, err := time.Parse(time.RFC3339Nano, since); err != nil || !got.Equal(updated) {
		t.Errorf("notifications since = %q, want %s", since, updated)
	}
}

func TestNotificationRouting(t *testing.T) {
	t.Parallel()
	closed := openPR(3, "other")
	closed.State = model.PRStateClosed
	tests := []struct {
		name      string
		stored    *model.PR
		reason    string
		wantFetch bool
	}{
		{name: "subscribed untracked", reason: "subscribed"},
		{name: "subscribed tracked", stored: ptr(openPR(2, "other")), reason: "subscribed", wantFetch: true},
		{name: "subscribed tracked closed", stored: &closed, reason: "subscribed", wantFetch: true},
		{name: "mention untracked", reason: "mention", wantFetch: true},
		{name: "comment on closed", stored: &closed, reason: "comment", wantFetch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			pr := openPR(1, "other")
			if tt.stored != nil {
				pr = *tt.stored
				seed(t, dir, true, pr)
			} else {
				seed(t, dir, true)
			}
			current := pr
			current.State = model.PRStateOpen
			gh := newFake(current)
			gh.notify(github.Notification{ThreadID: "t", Reason: tt.reason, PR: pr.Key, UpdatedAt: time.Now().UTC()})

			r := startDaemon(t, testConfig(dir), gh)
			if tt.wantFetch {
				eventually(t, "PR refreshed as open", func() bool {
					got, ok := r.storedPR(t, pr.Key)
					return ok && got.State == model.PRStateOpen
				})
				return
			}
			// Notifications are handled before the next poll starts.
			eventually(t, "second poll", func() bool { return len(gh.callsOf("Notifications")) >= 2 })
			if n := gh.fetches(pr.Key); n != 0 {
				t.Errorf("FetchPR calls = %d, want 0", n)
			}
			if _, ok := r.storedPR(t, pr.Key); ok {
				t.Error("untracked PR was stored")
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestReconcileFetchesOnlyChangedPRs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	same, changed := openPR(1, testViewer.Login), openPR(2, testViewer.Login)
	seed(t, dir, true, same, changed)

	changed.Checks = model.ChecksFailure
	changed.UpdatedAt = baseTime.Add(time.Minute)
	gh := newFake(same, changed)
	cfg := testConfig(dir)
	cfg.ReconcileInterval = 20 * time.Millisecond
	r := startDaemon(t, cfg, gh)

	eventually(t, "changed PR refreshed", func() bool {
		pr, _ := r.storedPR(t, changed.Key)
		return pr.Checks == model.ChecksFailure
	})
	// Two more full reconcile rounds see the refreshed snapshot and must not fetch again.
	n := len(gh.callsOf("FetchStatuses"))
	eventually(t, "more reconcile rounds", func() bool { return len(gh.callsOf("FetchStatuses")) >= n+2 })
	fetched := gh.fetches(changed.Key)
	eventually(t, "two further reconcile rounds", func() bool { return len(gh.callsOf("FetchStatuses")) >= n+4 })

	if got := gh.fetches(changed.Key); got != fetched || got < 1 || got > 2 {
		t.Errorf("FetchPR(changed) calls = %d, then %d; want 1 or 2 and stable", fetched, got)
	}
	if got := gh.fetches(same.Key); got != 0 {
		t.Errorf("FetchPR(unchanged) calls = %d, want 0", got)
	}
	evs := r.settledEvents(t, 1)
	if !slices.Contains(eventTypes(evs), "devinbox.pr.checks_failed") {
		t.Errorf("event types = %q, want pr.checks_failed", eventTypes(evs))
	}
}

func TestRateLimitPausesAllCalls(t *testing.T) {
	t.Parallel()
	pr := openPR(5, "other")
	gh := newFake(pr)
	reset := time.Now().Add(1500 * time.Millisecond)
	gh.fetchErrs[pr.Key] = []error{fmt.Errorf("graphql: %w", &github.RateLimitError{Reset: reset})}
	gh.notify(github.Notification{ThreadID: "t", Reason: "mention", PR: pr.Key, UpdatedAt: time.Now().UTC()})

	r := startDaemon(t, testConfig(t.TempDir()), gh)
	eventually(t, "PR stored after the pause", func() bool { _, ok := r.storedPR(t, pr.Key); return ok })

	fetches := gh.callsOf("FetchPR")
	if len(fetches) != 2 {
		t.Fatalf("FetchPR calls = %d, want 2", len(fetches))
	}
	if fetches[1].at.Before(reset) {
		t.Errorf("retry at %s, before reset %s", fetches[1].at.Format(time.StampMilli), reset.Format(time.StampMilli))
	}
	// Calls already in flight when the limit hit may land shortly after it.
	limited := fetches[0].at.Add(100 * time.Millisecond)
	gh.mu.Lock()
	calls := slices.Clone(gh.calls)
	gh.mu.Unlock()
	for _, c := range calls {
		if c.at.After(limited) && c.at.Before(reset) {
			t.Errorf("%s called at %s during the pause", c.method, c.at.Format(time.StampMilli))
		}
	}
	eventually(t, "notification polls resumed", func() bool {
		return slices.ContainsFunc(gh.callsOf("Notifications"), func(c ghCall) bool { return !c.at.Before(reset) })
	})
}

func TestAPI(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := requestedPR(1)
	open.Comments = []model.Comment{
		{ID: "C_1", Kind: model.CommentIssue, Author: "other", Body: "first", CreatedAt: baseTime},
		{ID: "C_2", Kind: model.CommentIssue, Author: "other", Body: "second", CreatedAt: baseTime.Add(time.Minute)},
	}
	closed := openPR(2, testViewer.Login)
	closed.State = model.PRStateMerged
	seed(t, dir, true, open, closed)
	r := startDaemon(t, testConfig(dir), newFake(open, closed))

	list := request[api.ListResponse](t, r.nc, api.SubjectList, api.ListRequest{})
	if list.Error != "" {
		t.Fatalf("list error: %s", list.Error)
	}
	if list.Viewer.Login != testViewer.Login {
		t.Errorf("viewer = %+v, want %+v", list.Viewer, testViewer)
	}
	if len(list.Items) != 1 || list.Items[0].PR.Key != open.Key {
		t.Fatalf("list items = %+v, want only %s", list.Items, open.Key)
	}
	if got := list.Items[0]; got.PR.Comments != nil || got.Section != model.SectionReviewRequested || !got.Direct {
		t.Errorf("list item = %+v, want no comments, direct review_requested", got)
	}

	get := request[api.GetResponse](t, r.nc, api.SubjectGet, api.GetRequest{PR: open.Key})
	if get.Error != "" || get.Item == nil || len(get.Item.PR.Comments) != 2 {
		t.Fatalf("get = %+v, want the PR with 2 comments", get)
	}
	if unknown := request[api.GetResponse](t, r.nc, api.SubjectGet, api.GetRequest{PR: prKey(99)}); unknown.Error == "" {
		t.Errorf("get unknown PR succeeded: %+v", unknown)
	}

	prLevel := model.Annotation{PR: open.Key, Key: "linear.urgency", Value: json.RawMessage(`{"badge":"P1","level":"crit"}`), Source: "test"}
	commentLevel := model.Annotation{PR: open.Key, CommentID: "C_2", Key: "triage.kind", Value: json.RawMessage(`"question"`)}
	for _, a := range []model.Annotation{prLevel, commentLevel} {
		if resp := request[api.AnnotateResponse](t, r.nc, api.SubjectAnnotate, api.AnnotateRequest{Annotation: a}); resp.Error != "" {
			t.Fatalf("annotate %s: %s", a.Key, resp.Error)
		}
	}

	invalid := []struct {
		name string
		a    model.Annotation
	}{
		{name: "empty key", a: model.Annotation{PR: open.Key, Value: json.RawMessage(`1`)}},
		{name: "unknown PR", a: model.Annotation{PR: prKey(99), Key: "x.y", Value: json.RawMessage(`1`)}},
	}
	for _, tt := range invalid {
		if resp := request[api.AnnotateResponse](t, r.nc, api.SubjectAnnotate, api.AnnotateRequest{Annotation: tt.a}); resp.Error == "" {
			t.Errorf("annotate with %s succeeded", tt.name)
		}
	}

	list = request[api.ListResponse](t, r.nc, api.SubjectList, api.ListRequest{})
	if anns := list.Items[0].Annotations; len(anns) != 1 || anns[0].Key != prLevel.Key || anns[0].UpdatedAt.IsZero() {
		t.Errorf("list annotations = %+v, want only %s with UpdatedAt", anns, prLevel.Key)
	}
	get = request[api.GetResponse](t, r.nc, api.SubjectGet, api.GetRequest{PR: open.Key})
	if anns := get.Item.Annotations; len(anns) != 2 {
		t.Errorf("get annotations = %+v, want PR- and comment-level", anns)
	}

	evs := r.settledEvents(t, 2)
	if len(evs) != 2 {
		t.Fatalf("events = %q, want 2 pr.annotated", eventTypes(evs))
	}
	for i, want := range []model.Annotation{prLevel, commentLevel} {
		ev := evs[i]
		if ev.subject != "devinbox.events.pr.acme-co.frontend.1.annotated" || ev.ce.Type != "devinbox.pr.annotated" {
			t.Errorf("event %d: subject %s, type %s", i, ev.subject, ev.ce.Type)
		}
		var data struct {
			model.EventData
			Change model.Annotation `json:"change"`
		}
		if err := json.Unmarshal(ev.ce.Data, &data); err != nil {
			t.Fatalf("decode event data: %v", err)
		}
		if data.PR.Key != open.Key || data.Section != model.SectionReviewRequested || data.PR.Comments != nil {
			t.Errorf("event %d data = %+v, want snapshot without comments and classification", i, data.EventData)
		}
		if data.Change.Key != want.Key || data.Change.CommentID != want.CommentID || data.Change.UpdatedAt.IsZero() {
			t.Errorf("event %d change = %+v, want %+v with UpdatedAt", i, data.Change, want)
		}
	}

	del := prLevel
	del.Value = json.RawMessage(`null`)
	if resp := request[api.AnnotateResponse](t, r.nc, api.SubjectAnnotate, api.AnnotateRequest{Annotation: del}); resp.Error != "" {
		t.Fatalf("delete annotation: %s", resp.Error)
	}
	list = request[api.ListResponse](t, r.nc, api.SubjectList, api.ListRequest{})
	if anns := list.Items[0].Annotations; len(anns) != 0 {
		t.Errorf("annotations after delete = %+v, want none", anns)
	}
}

func TestRestartKeepsStateAndSkipsBackfill(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pr := openPR(1, "other")
	gh := newFake(pr)
	gh.search["involves:@me"] = []model.PRKey{pr.Key}

	first := startDaemon(t, testConfig(dir), gh)
	eventually(t, "backfill marker", func() bool { _, ok := first.meta(t, metaBackfillDone); return ok })
	before := first.settledEvents(t, 1)
	since, _ := first.meta(t, metaNotificationsSince)
	first.stop()
	for _, p := range []string{"inbox.db", filepath.Join("jetstream", "$G", "streams", api.StreamName)} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("state directory lacks %s: %v", p, err)
		}
	}

	// Offline restart: the cached viewer is used.
	offline := newFake(pr)
	offline.viewerErr = errors.New("network unreachable")
	offline.search["involves:@me"] = []model.PRKey{pr.Key}
	second := startDaemon(t, testConfig(dir), offline)
	eventually(t, "notification polls", func() bool { return len(offline.callsOf("Notifications")) >= 2 })

	if n := len(offline.callsOf("SearchOpenPRs")); n != 0 {
		t.Errorf("SearchOpenPRs calls after restart = %d, want 0", n)
	}
	if got := offline.callsOf("Notifications")[0].since.UTC().Format(time.RFC3339Nano); got != since {
		t.Errorf("notifications since after restart = %s, want %s", got, since)
	}
	list := request[api.ListResponse](t, second.nc, api.SubjectList, api.ListRequest{})
	if list.Viewer.Login != testViewer.Login || len(list.Items) != 1 || list.Items[0].PR.Key != pr.Key {
		t.Errorf("list after restart = %+v, want cached viewer and %s", list, pr.Key)
	}
	if after := second.streamEvents(t); len(after) != len(before) {
		t.Errorf("stream events after restart = %d, want %d", len(after), len(before))
	}
}

func TestShutdownFinishesCurrentItemOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed(t, dir, true)
	prs := []model.PR{openPR(1, "other"), openPR(2, "other"), openPR(3, "other")}
	gh := newFake(prs...)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gh.fetchHook = func(model.PRKey) {
		once.Do(func() {
			close(started)
			<-release
		})
	}
	for _, pr := range prs {
		gh.notify(github.Notification{ThreadID: pr.NodeID, Reason: "mention", PR: pr.Key, UpdatedAt: time.Now().UTC()})
	}

	r := startDaemon(t, testConfig(dir), gh)
	<-started
	stopped := make(chan struct{})
	go func() {
		r.stop()
		close(stopped)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-stopped

	if n := len(gh.callsOf("FetchPR")); n != 1 {
		t.Errorf("FetchPR calls = %d, want 1", n)
	}
	st, err := store.Open(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	stored, err := st.ListPRs(context.Background(), false)
	if err != nil {
		t.Fatalf("list PRs: %v", err)
	}
	if len(stored) != 1 || stored[0].Key != prs[0].Key {
		t.Errorf("stored PRs = %d, want only the one in flight at shutdown (%s)", len(stored), prs[0].Key)
	}
}

func TestQueue(t *testing.T) {
	t.Parallel()
	q := newQueue()
	var done []string
	q.push(item{key: prKey(1), baseline: true, done: []func(){func() { done = append(done, "a") }}})
	q.push(item{key: prKey(2), baseline: true})
	q.push(item{key: prKey(1), baseline: false, done: []func(){func() { done = append(done, "b") }}})
	q.push(item{key: prKey(2), baseline: true})
	q.push(item{key: prKey(3), attempts: 2})
	q.push(item{key: prKey(3)})

	ctx, cancelAll := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAll()
	want := []struct {
		key      model.PRKey
		baseline bool
		attempts int
		done     int
	}{
		{key: prKey(1), baseline: false, done: 2},
		{key: prKey(2), baseline: true},
		{key: prKey(3), attempts: 0},
	}
	for _, w := range want {
		it, ok := q.pop(ctx)
		if !ok || it.key != w.key || it.baseline != w.baseline || it.attempts != w.attempts || len(it.done) != w.done {
			t.Fatalf("pop = %+v, %v; want %+v", it, ok, w)
		}
		it.finish()
	}
	if !slices.Equal(done, []string{"a", "b"}) {
		t.Errorf("done callbacks = %q, want a then b", done)
	}

	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if it, ok := q.pop(cctx); ok {
		t.Errorf("pop on empty queue returned %+v", it)
	}
	q.push(item{key: prKey(5)})
	if it, ok := q.pop(cctx); ok {
		t.Errorf("pop after ctx ended returned %+v", it)
	}
	if it, ok := q.pop(ctx); !ok || it.key != prKey(5) {
		t.Errorf("pop = %+v, %v; want %s still queued", it, ok, prKey(5))
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		q.push(item{key: prKey(4)})
	}()
	if it, ok := q.pop(ctx); !ok || it.key != prKey(4) {
		t.Errorf("blocking pop = %+v, %v; want %s", it, ok, prKey(4))
	}
}

func TestGatePause(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name    string
		current time.Time
		reset   time.Time
		want    time.Time
	}{
		{name: "future reset", reset: now.Add(time.Hour), want: now.Add(time.Hour)},
		{name: "unknown reset", want: now.Add(defaultRateLimitPause)},
		{name: "reset in the past", reset: now.Add(-time.Hour), want: now.Add(minRateLimitPause)},
		{name: "never shortens", current: now.Add(2 * time.Hour), reset: now.Add(time.Hour), want: now.Add(2 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &gate{log: testLogger(), until: tt.current}
			g.pause(tt.reset)
			if diff := g.until.Sub(tt.want); diff < 0 || diff > time.Second {
				t.Errorf("until = %s, want %s", g.until, tt.want)
			}
		})
	}
}

func TestCallRetriesAfterRateLimit(t *testing.T) {
	t.Parallel()
	g := &gate{log: testLogger()}
	reset := time.Now().Add(1200 * time.Millisecond)
	attempts := 0
	start := time.Now()
	got, err := call(context.Background(), g, func() (string, error) {
		attempts++
		if attempts == 1 {
			return "", fmt.Errorf("wrapped: %w", &github.RateLimitError{Reset: reset})
		}
		return "ok", nil
	})
	if err != nil || got != "ok" || attempts != 2 {
		t.Fatalf("call = %q, %v after %d attempts; want ok after 2", got, err, attempts)
	}
	if time.Now().Before(reset) {
		t.Errorf("retried after %s, before the reset", time.Since(start))
	}

	// Other calls wait for the same pause.
	g.pause(time.Now().Add(1100 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := call(ctx, g, func() (int, error) { t.Error("called during pause"); return 0, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("call during pause = %v, want deadline exceeded", err)
	}
}

func TestSameStatus(t *testing.T) {
	t.Parallel()
	base := openPR(1, "me").PRStatus
	berlin := time.FixedZone("CEST", 2*60*60)
	tests := []struct {
		name string
		mod  func(*model.PRStatus)
		want bool
	}{
		{name: "identical", mod: func(*model.PRStatus) {}, want: true},
		{name: "same instant, other zone", mod: func(s *model.PRStatus) { s.UpdatedAt = s.UpdatedAt.In(berlin) }, want: true},
		{name: "updated", mod: func(s *model.PRStatus) { s.UpdatedAt = s.UpdatedAt.Add(time.Second) }, want: false},
		{name: "checks", mod: func(s *model.PRStatus) { s.Checks = model.ChecksFailure }, want: false},
		{name: "merge queue", mod: func(s *model.PRStatus) { s.InMergeQueue = true }, want: false},
		{name: "state", mod: func(s *model.PRStatus) { s.State = model.PRStateMerged }, want: false},
	}
	for _, tt := range tests {
		other := base
		tt.mod(&other)
		if got := sameStatus(base, other); got != tt.want {
			t.Errorf("%s: sameStatus = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDefaultStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct {
		xdg  string
		want string
	}{
		{xdg: "/var/state", want: "/var/state/dev-inbox"},
		{xdg: "", want: filepath.Join(home, ".local/state/dev-inbox")},
		{xdg: "relative/state", want: filepath.Join(home, ".local/state/dev-inbox")},
	}
	for _, tt := range tests {
		t.Setenv("XDG_STATE_HOME", tt.xdg)
		got, err := DefaultStateDir()
		if err != nil || got != tt.want {
			t.Errorf("XDG_STATE_HOME=%q: DefaultStateDir = %q, %v; want %q", tt.xdg, got, err, tt.want)
		}
	}
}
