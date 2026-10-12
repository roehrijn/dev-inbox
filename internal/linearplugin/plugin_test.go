package linearplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/linear"
	"github.com/roehrijn/dev-inbox/internal/model"
)

var (
	cest    = time.FixedZone("CEST", 2*60*60)
	now     = time.Date(2026, 10, 12, 10, 0, 0, 0, cest)
	keyAPI  = model.PRKey{Owner: "acme", Repo: "api", Number: 1}
	keyWeb  = model.PRKey{Owner: "acme", Repo: "web", Number: 2}
	timeout = 10 * time.Second
)

func openItem(key model.PRKey) api.InboxItem {
	return api.InboxItem{
		PR: model.PR{
			Key:      key,
			PRStatus: model.PRStatus{State: model.PRStateOpen},
			Title:    "Add retries to the client",
			URL:      fmt.Sprintf("https://github.com/%s/%s/pull/%d", key.Owner, key.Repo, key.Number),
			HeadRef:  "devops-1/retries",
		},
		Classification: model.Classification{Section: model.SectionWaitingForReview},
	}
}

func dueIn(days int) string { return now.AddDate(0, 0, days).Format(dateLayout) }

type fakeDaemon struct {
	mu        sync.Mutex
	items     map[model.PRKey]*api.InboxItem
	gets      map[model.PRKey]int
	annotated []model.Annotation
}

func (d *fakeDaemon) put(it api.InboxItem) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items[it.PR.Key] = &it
}

func (d *fakeDaemon) update(key model.PRKey, fn func(pr *model.PR)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(&d.items[key].PR)
}

func (d *fakeDaemon) annotation(key model.PRKey, annKey string) (model.Annotation, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.items[key].Annotations {
		if a.CommentID == "" && a.Key == annKey {
			return a, true
		}
	}
	return model.Annotation{}, false
}

func (d *fakeDaemon) writes(key model.PRKey) []model.Annotation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(d.annotated), func(a model.Annotation) bool { return a.PR != key })
}

func (d *fakeDaemon) writesOf(key model.PRKey, annKey string) []model.Annotation {
	return slices.DeleteFunc(d.writes(key), func(a model.Annotation) bool { return a.Key != annKey })
}

func (d *fakeDaemon) getCalls(key model.PRKey) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gets[key]
}

func (d *fakeDaemon) serve(t *testing.T, nc *nats.Conn) {
	t.Helper()
	handle := func(subject string, fn func(data []byte) any) {
		if _, err := nc.Subscribe(subject, func(m *nats.Msg) {
			body, err := json.Marshal(fn(m.Data))
			if err != nil {
				t.Error(err)
				return
			}
			_ = m.Respond(body)
		}); err != nil {
			t.Fatal(err)
		}
	}
	handle(api.SubjectList, func([]byte) any {
		d.mu.Lock()
		defer d.mu.Unlock()
		resp := api.ListResponse{Viewer: model.Viewer{Login: "me"}}
		for _, it := range d.items {
			item := *it
			item.Annotations = slices.Clone(it.Annotations)
			resp.Items = append(resp.Items, item)
		}
		return resp
	})
	handle(api.SubjectGet, func(data []byte) any {
		var req api.GetRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return api.GetResponse{Error: err.Error()}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.gets[req.PR]++
		it, ok := d.items[req.PR]
		if !ok {
			return api.GetResponse{Error: "unknown PR " + req.PR.String()}
		}
		item := *it
		item.Annotations = slices.Clone(it.Annotations)
		return api.GetResponse{Item: &item}
	})
	handle(api.SubjectAnnotate, func(data []byte) any {
		var req api.AnnotateRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return api.AnnotateResponse{Error: err.Error()}
		}
		a := req.Annotation
		a.UpdatedAt = time.Now().UTC()
		d.mu.Lock()
		defer d.mu.Unlock()
		d.annotated = append(d.annotated, a)
		it := d.items[a.PR]
		it.Annotations = slices.DeleteFunc(it.Annotations, func(o model.Annotation) bool {
			return o.CommentID == a.CommentID && o.Key == a.Key
		})
		if string(a.Value) != "null" {
			it.Annotations = append(it.Annotations, a)
		}
		return api.AnnotateResponse{}
	})
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

// fakeLinear returns the issues configured for a PR URL.
type fakeLinear struct {
	mu     sync.Mutex
	issues map[string][]linear.Issue
	fail   int // the next fail calls return an error
	calls  []linear.PRRef
}

func (f *fakeLinear) set(url string, issues ...linear.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[url] = issues
}

func (f *fakeLinear) failNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = n
}

func (f *fakeLinear) seen() []linear.PRRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeLinear) IssuesForPR(_ context.Context, pr linear.PRRef) ([]linear.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pr)
	if f.fail > 0 {
		f.fail--
		return nil, errLinear
	}
	return slices.Clone(f.issues[pr.URL]), nil
}

var errLinear = errors.New("linear: 503 service unavailable")

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	url    string
	js     jetstream.JetStream
	daemon *fakeDaemon
	linear *fakeLinear
	logs   *syncBuffer
}

func newHarness(t *testing.T, items ...api.InboxItem) *harness {
	t.Helper()
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: server.RANDOM_PORT, JetStream: true,
		StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(timeout) {
		t.Fatal("NATS server not ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name: api.StreamName, Subjects: []string{api.EventsSubjectAll}, Storage: jetstream.MemoryStorage,
	}); err != nil {
		t.Fatal(err)
	}
	h := &harness{
		url:    ns.ClientURL(),
		js:     js,
		daemon: &fakeDaemon{items: make(map[model.PRKey]*api.InboxItem), gets: make(map[model.PRKey]int)},
		linear: &fakeLinear{issues: make(map[string][]linear.Issue)},
		logs:   &syncBuffer{},
	}
	for _, it := range items {
		h.daemon.put(it)
	}
	h.daemon.serve(t, nc)
	return h
}

// start runs the plugin until the test ends.
func (h *harness) start(t *testing.T, configure func(*Config)) {
	t.Helper()
	cfg := Config{
		NATSURL:       h.url,
		HorizonDays:   DefaultHorizonDays,
		SweepInterval: time.Hour,
		Linear:        h.linear,
		Logger:        slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           func() time.Time { return now },
	}
	if configure != nil {
		configure(&cfg)
	}
	p, err := newPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.retryBase = 20 * time.Millisecond
	p.retryMax = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("plugin returned %v", err)
			}
		case <-time.After(timeout):
			t.Error("plugin did not stop")
		}
	})
}

func (h *harness) sweeps() int { return strings.Count(h.logs.String(), `"msg":"sweep done"`) }

func (h *harness) publish(t *testing.T, typ string, pr model.PR) {
	t.Helper()
	ev, err := model.NewEvent(typ, pr.Key, model.EventData{PR: pr})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(api.NewCloudEvent(ev))
	if err != nil {
		t.Fatal(err)
	}
	msg := nats.NewMsg(api.EventSubject(ev))
	msg.Header.Set(jetstream.MsgIDHeader, ev.ID)
	msg.Data = body
	if _, err := h.js.PublishMsg(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) consumer(t *testing.T) *jetstream.ConsumerInfo {
	t.Helper()
	c, err := h.js.Consumer(context.Background(), api.StreamName, ConsumerName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// settled reports whether the consumer has no undelivered or unacknowledged events left.
func (h *harness) settled(t *testing.T) bool {
	info := h.consumer(t)
	return info.NumPending == 0 && info.NumAckPending == 0
}

// value returns the urgency annotation of key, or nil without one.
func (h *harness) value(t *testing.T, key model.PRKey) *urgencyValue {
	t.Helper()
	a, ok := h.daemon.annotation(key, derive.UrgencyKey)
	if !ok {
		return nil
	}
	if a.Source != Source {
		t.Errorf("annotation source %q, want %q", a.Source, Source)
	}
	var v urgencyValue
	if err := json.Unmarshal(a.Value, &v); err != nil {
		t.Fatalf("annotation %s: %v", a.Value, err)
	}
	return &v
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUrgency(t *testing.T) {
	project := func(name, date string, done bool) *linear.Project {
		return &linear.Project{Name: name, TargetDate: date, Done: done}
	}
	tests := []struct {
		name    string
		now     time.Time
		horizon int
		issues  []linear.Issue
		want    *urgencyValue
	}{
		{name: "no issues"},
		{name: "issue without dates", issues: []linear.Issue{{Identifier: "A-1"}}},
		{
			name:   "due today",
			issues: []linear.Issue{{Identifier: "A-1", DueDate: dueIn(0)}},
			want:   &urgencyValue{Badge: "due today", Level: "crit", DueDate: "2026-10-12", Reason: "issue A-1", Issues: []string{"A-1"}},
		},
		{
			name:   "overdue",
			issues: []linear.Issue{{Identifier: "A-1", DueDate: dueIn(-3)}},
			want:   &urgencyValue{Badge: "overdue 3d", Level: "crit", DueDate: "2026-10-09", Reason: "issue A-1", Issues: []string{"A-1"}},
		},
		{
			name:   "overdue beyond any horizon",
			issues: []linear.Issue{{Identifier: "A-1", DueDate: dueIn(-400)}},
			want: &urgencyValue{Badge: "overdue 400d", Level: "crit", DueDate: dueIn(-400), Reason: "issue A-1",
				Issues: []string{"A-1"}},
		},
		{
			name:   "due tomorrow",
			issues: []linear.Issue{{Identifier: "A-1", DueDate: dueIn(1)}},
			want:   &urgencyValue{Badge: "due tomorrow", Level: "warn", DueDate: "2026-10-13", Reason: "issue A-1", Issues: []string{"A-1"}},
		},
		{
			name:   "due in 2 days",
			issues: []linear.Issue{{Identifier: "A-1", DueDate: dueIn(2)}},
			want:   &urgencyValue{Badge: "due in 2d", Level: "warn", DueDate: "2026-10-14", Reason: "issue A-1", Issues: []string{"A-1"}},
		},
		{
			name:   "due in 3 days",
			issues: []linear.Issue{{Identifier: "A-1", DueDate: dueIn(3)}},
			want:   &urgencyValue{Badge: "due in 3d", Level: "info", DueDate: "2026-10-15", Reason: "issue A-1", Issues: []string{"A-1"}},
		},
		{
			name:   "at the default horizon",
			issues: []linear.Issue{{Identifier: "A-1", DueDate: dueIn(7)}},
			want:   &urgencyValue{Badge: "due in 7d", Level: "info", DueDate: "2026-10-19", Reason: "issue A-1", Issues: []string{"A-1"}},
		},
		{name: "beyond the default horizon", issues: []linear.Issue{{Identifier: "A-1", DueDate: dueIn(8)}}},
		{
			name:    "beyond a custom horizon",
			horizon: 2,
			issues:  []linear.Issue{{Identifier: "A-1", DueDate: dueIn(3)}},
		},
		{
			name:   "just after midnight local time, still the previous day in UTC",
			now:    time.Date(2026, 10, 12, 0, 30, 0, 0, cest),
			issues: []linear.Issue{{Identifier: "A-1", DueDate: "2026-10-12"}},
			want:   &urgencyValue{Badge: "due today", Level: "crit", DueDate: "2026-10-12", Reason: "issue A-1", Issues: []string{"A-1"}},
		},
		{
			name:   "half an hour before a due date is tomorrow",
			now:    time.Date(2026, 10, 12, 23, 30, 0, 0, cest),
			issues: []linear.Issue{{Identifier: "A-1", DueDate: "2026-10-13"}},
			want:   &urgencyValue{Badge: "due tomorrow", Level: "warn", DueDate: "2026-10-13", Reason: "issue A-1", Issues: []string{"A-1"}},
		},
		{
			name: "done issues and projects ignored",
			issues: []linear.Issue{
				{Identifier: "A-1", DueDate: dueIn(-1), Done: true, Project: project("Old", dueIn(-1), false)},
				{Identifier: "B-2", DueDate: dueIn(5), Project: project("Shipped", dueIn(1), true)},
			},
			want: &urgencyValue{Badge: "due in 5d", Level: "info", DueDate: dueIn(5), Reason: "issue B-2", Issues: []string{"B-2"}},
		},
		{
			name: "all done",
			issues: []linear.Issue{
				{Identifier: "A-1", DueDate: dueIn(-1), Done: true},
			},
		},
		{
			name: "earliest wins",
			issues: []linear.Issue{
				{Identifier: "A-1", DueDate: dueIn(5), Project: project("Foo", dueIn(4), false),
					Milestone: &linear.Milestone{Name: "Beta", TargetDate: dueIn(3)}},
				{Identifier: "B-2", Project: project("Bar", dueIn(6), false)},
			},
			want: &urgencyValue{Badge: "due in 3d", Level: "info", DueDate: dueIn(3), Reason: "milestone Beta",
				Issues: []string{"A-1", "B-2"}},
		},
		{
			name: "project",
			issues: []linear.Issue{
				{Identifier: "A-1", Project: project("Foo", dueIn(1), false)},
			},
			want: &urgencyValue{Badge: "due tomorrow", Level: "warn", DueDate: dueIn(1), Reason: "project Foo", Issues: []string{"A-1"}},
		},
		{
			name: "tie: issue before milestone before project",
			issues: []linear.Issue{
				{Identifier: "B-2", Project: project("Foo", dueIn(2), false)},
				{Identifier: "C-3", Milestone: &linear.Milestone{Name: "Beta", TargetDate: dueIn(2)}},
				{Identifier: "D-4", DueDate: dueIn(2)},
			},
			want: &urgencyValue{Badge: "due in 2d", Level: "warn", DueDate: dueIn(2), Reason: "issue D-4",
				Issues: []string{"B-2", "C-3", "D-4"}},
		},
		{
			name: "tie: milestone before project",
			issues: []linear.Issue{
				{Identifier: "A-1", Project: project("Foo", dueIn(2), false)},
				{Identifier: "C-3", Milestone: &linear.Milestone{Name: "Beta", TargetDate: dueIn(2)}},
			},
			want: &urgencyValue{Badge: "due in 2d", Level: "warn", DueDate: dueIn(2), Reason: "milestone Beta",
				Issues: []string{"A-1", "C-3"}},
		},
		{
			name: "tie of the same kind: lower identifier",
			issues: []linear.Issue{
				{Identifier: "B-1", Project: project("Alpha", dueIn(2), false)},
				{Identifier: "A-2", Project: project("Zeta", dueIn(2), false)},
			},
			want: &urgencyValue{Badge: "due in 2d", Level: "warn", DueDate: dueIn(2), Reason: "project Zeta",
				Issues: []string{"A-2", "B-1"}},
		},
		{
			name: "invalid date skipped",
			issues: []linear.Issue{
				{Identifier: "A-1", DueDate: "12/10/2026", Project: project("Foo", dueIn(4), false)},
			},
			want: &urgencyValue{Badge: "due in 4d", Level: "info", DueDate: dueIn(4), Reason: "project Foo", Issues: []string{"A-1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs syncBuffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			n := tt.now
			if n.IsZero() {
				n = now
			}
			h := tt.horizon
			if h == 0 {
				h = DefaultHorizonDays
			}
			got := urgency(log, tt.issues, n, h)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if invalid := strings.Contains(logs.String(), "ignoring invalid Linear date"); invalid != (tt.name == "invalid date skipped") {
				t.Errorf("invalid date logged = %v: %s", invalid, logs.String())
			}
		})
	}
}

func TestUrgencyValueJSON(t *testing.T) {
	raw, err := json.Marshal(urgencyValue{Badge: "due today", Level: "crit", DueDate: "2026-10-12", Reason: "issue A-1",
		Issues: []string{"A-1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"badge":"due today","level":"crit","dueDate":"2026-10-12","reason":"issue A-1","issues":["A-1"]}`
	if string(raw) != want {
		t.Errorf("got %s, want %s", raw, want)
	}
}

func TestSweepSetsUpdatesAndDeletes(t *testing.T) {
	apiItem, webItem := openItem(keyAPI), openItem(keyWeb)
	h := newHarness(t, apiItem, webItem)
	h.linear.set(apiItem.PR.URL, linear.Issue{Identifier: "DEVOPS-1", DueDate: dueIn(2)})
	h.linear.failNext(1)

	h.start(t, func(c *Config) { c.SweepInterval = 20 * time.Millisecond })

	soon := &urgencyValue{Badge: "due in 2d", Level: "warn", DueDate: dueIn(2), Reason: "issue DEVOPS-1",
		Issues: []string{"DEVOPS-1"}}
	eventually(t, "the annotation", func() bool { return reflect.DeepEqual(h.value(t, keyAPI), soon) })
	if !strings.Contains(h.logs.String(), `"failed":1`) {
		t.Error("failed sweep not logged")
	}
	settle := func() {
		n := h.sweeps()
		eventually(t, "two more sweeps", func() bool { return h.sweeps() >= n+2 })
	}
	settle()
	if n := len(h.daemon.writesOf(keyAPI, derive.UrgencyKey)); n != 1 {
		t.Errorf("%d writes after unchanged sweeps, want 1", n)
	}

	h.linear.set(apiItem.PR.URL, linear.Issue{Identifier: "DEVOPS-1", DueDate: dueIn(2),
		Project: &linear.Project{Name: "Foo", TargetDate: dueIn(-1)}})
	overdue := &urgencyValue{Badge: "overdue 1d", Level: "crit", DueDate: dueIn(-1), Reason: "project Foo",
		Issues: []string{"DEVOPS-1"}}
	eventually(t, "the updated annotation", func() bool { return reflect.DeepEqual(h.value(t, keyAPI), overdue) })
	settle()
	if n := len(h.daemon.writesOf(keyAPI, derive.UrgencyKey)); n != 2 {
		t.Errorf("%d writes after the update, want 2", n)
	}

	h.linear.set(apiItem.PR.URL, linear.Issue{Identifier: "DEVOPS-1", DueDate: dueIn(-1), Done: true})
	eventually(t, "the deletion", func() bool { return h.value(t, keyAPI) == nil })
	settle()
	writes := h.daemon.writesOf(keyAPI, derive.UrgencyKey)
	if len(writes) != 3 || string(writes[2].Value) != "null" {
		t.Errorf("writes %v, want a third one deleting the annotation", writes)
	}
	if n := len(h.daemon.writes(keyWeb)); n != 0 {
		t.Errorf("PR without urgent issues written %d times", n)
	}
	if refs := h.linear.seen(); !slices.Contains(refs, linear.PRRef{URL: apiItem.PR.URL, Title: apiItem.PR.Title, Branch: apiItem.PR.HeadRef}) {
		t.Errorf("Linear saw %v", refs)
	}
	if !strings.Contains(h.logs.String(), `"msg":"PR no longer urgent","pr":"acme/api#1","badge":"overdue 1d"`) {
		t.Error("clearing not logged with the badge")
	}
}

// issuesValue returns the decoded IssuesKey annotation of key, or nil without one.
func (h *harness) issuesValue(t *testing.T, key model.PRKey) *linksValue {
	t.Helper()
	a, ok := h.daemon.annotation(key, IssuesKey)
	if !ok {
		return nil
	}
	if a.Source != Source {
		t.Errorf("annotation source %q, want %q", a.Source, Source)
	}
	var v linksValue
	if err := json.Unmarshal(a.Value, &v); err != nil {
		t.Fatalf("annotation %s: %v", a.Value, err)
	}
	return &v
}

func TestLinkedIssues(t *testing.T) {
	tests := []struct {
		name  string
		issue linear.Issue
		want  string
	}{
		{name: "nothing", issue: linear.Issue{}, want: ""},
		{name: "state", issue: linear.Issue{State: "In Progress"}, want: "In Progress"},
		{name: "due date", issue: linear.Issue{DueDate: "2026-10-14"}, want: "due 2026-10-14"},
		{name: "milestone", issue: linear.Issue{Milestone: &linear.Milestone{Name: "Beta"}}, want: "milestone Beta"},
		{name: "project", issue: linear.Issue{Project: &linear.Project{Name: "Checkout v2"}}, want: "project Checkout v2"},
		{name: "unnamed project and milestone", issue: linear.Issue{
			Milestone: &linear.Milestone{TargetDate: "2026-11-01"}, Project: &linear.Project{},
		}, want: ""},
		{
			name: "everything",
			issue: linear.Issue{
				State: "In Progress", DueDate: "2026-10-14",
				Milestone: &linear.Milestone{Name: "Beta"}, Project: &linear.Project{Name: "Checkout v2"},
			},
			want: "In Progress · due 2026-10-14 · milestone Beta · project Checkout v2",
		},
		{
			name:  "no state",
			issue: linear.Issue{DueDate: "2026-10-14", Project: &linear.Project{Name: "Checkout v2"}},
			want:  "due 2026-10-14 · project Checkout v2",
		},
		{name: "done issue", issue: linear.Issue{State: "Done", Done: true}, want: "Done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.issue.Identifier, tt.issue.Title, tt.issue.URL = "ABC-1", "Fix login", "https://linear.app/acme/issue/ABC-1/fix-login"
			got := linkedIssues([]linear.Issue{tt.issue})
			want := &linksValue{Label: "Linear issues", Links: []link{{
				ID: "ABC-1", Title: "Fix login", URL: "https://linear.app/acme/issue/ABC-1/fix-login", Detail: tt.want,
			}}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
	if got := linkedIssues(nil); got != nil {
		t.Errorf("no issues: got %+v, want nil", got)
	}
}

func TestLinksValueJSON(t *testing.T) {
	raw, err := json.Marshal(linkedIssues([]linear.Issue{
		{Identifier: "ABC-1", Title: "Fix login", URL: "https://linear.app/acme/issue/ABC-1/fix-login", State: "In Progress",
			DueDate: "2026-10-14", Project: &linear.Project{Name: "Checkout v2"}},
		{Identifier: "ABC-2", Title: "Add retries", URL: "https://linear.app/acme/issue/ABC-2/add-retries"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"label":"Linear issues","links":[` +
		`{"id":"ABC-1","title":"Fix login","url":"https://linear.app/acme/issue/ABC-1/fix-login",` +
		`"detail":"In Progress · due 2026-10-14 · project Checkout v2"},` +
		`{"id":"ABC-2","title":"Add retries","url":"https://linear.app/acme/issue/ABC-2/add-retries"}]}`
	if string(raw) != want {
		t.Errorf("got %s, want %s", raw, want)
	}
}

func TestIssuesAnnotationSetsUpdatesAndDeletes(t *testing.T) {
	item := openItem(keyAPI)
	h := newHarness(t, item)
	abc1 := linear.Issue{Identifier: "ABC-1", Title: "Fix login", URL: "https://linear.app/acme/issue/ABC-1", State: "In Progress"}
	abc2 := linear.Issue{Identifier: "ABC-2", Title: "Add retries", URL: "https://linear.app/acme/issue/ABC-2", State: "Done", Done: true}
	h.linear.set(item.PR.URL, abc1, abc2)
	h.start(t, func(c *Config) { c.SweepInterval = 20 * time.Millisecond })

	want := linkedIssues([]linear.Issue{abc1, abc2})
	eventually(t, "the annotation", func() bool { return reflect.DeepEqual(h.issuesValue(t, keyAPI), want) })
	if got := h.issuesValue(t, keyAPI); len(got.Links) != 2 || got.Links[1].ID != "ABC-2" {
		t.Errorf("done issue missing or out of order: %+v", got)
	}
	if v := h.value(t, keyAPI); v != nil {
		t.Errorf("only done issues and no dates, got urgency %+v", v)
	}
	settle := func() {
		n := h.sweeps()
		eventually(t, "two more sweeps", func() bool { return h.sweeps() >= n+2 })
	}
	settle()
	if n := len(h.daemon.writesOf(keyAPI, IssuesKey)); n != 1 {
		t.Errorf("%d writes after unchanged sweeps, want 1", n)
	}

	abc1.State = "In Review"
	abc1.DueDate = dueIn(2)
	h.linear.set(item.PR.URL, abc1, abc2)
	want = linkedIssues([]linear.Issue{abc1, abc2})
	eventually(t, "the updated annotation", func() bool { return reflect.DeepEqual(h.issuesValue(t, keyAPI), want) })
	settle()
	if n := len(h.daemon.writesOf(keyAPI, IssuesKey)); n != 2 {
		t.Errorf("%d writes after the update, want 2", n)
	}
	if v := h.value(t, keyAPI); v == nil || v.Reason != "issue ABC-1" || !reflect.DeepEqual(v.Issues, []string{"ABC-1"}) {
		t.Errorf("urgency %+v", v)
	}
	if n := len(h.daemon.writesOf(keyAPI, derive.UrgencyKey)); n != 1 {
		t.Errorf("%d urgency writes, want 1", n)
	}

	h.linear.set(item.PR.URL)
	eventually(t, "the deletion", func() bool { return h.issuesValue(t, keyAPI) == nil && h.value(t, keyAPI) == nil })
	settle()
	writes := h.daemon.writesOf(keyAPI, IssuesKey)
	if len(writes) != 3 || string(writes[2].Value) != "null" {
		t.Errorf("writes %v, want a third one deleting the annotation", writes)
	}
}

func TestEventsTriggerEvaluation(t *testing.T) {
	item := openItem(keyAPI)
	h := newHarness(t, item)
	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })
	if n := len(h.linear.seen()); n != 1 {
		t.Fatalf("sweep made %d Linear calls, want 1", n)
	}
	info := h.consumer(t)
	if info.Config.Durable != ConsumerName || info.Config.AckPolicy != jetstream.AckExplicitPolicy ||
		info.Config.AckWait != ackWait || info.Config.MaxDeliver != -1 ||
		!reflect.DeepEqual(info.Config.FilterSubjects, FilterSubjects()) {
		t.Errorf("consumer config %+v", info.Config)
	}

	h.linear.set(item.PR.URL, linear.Issue{Identifier: "DEVOPS-1", DueDate: dueIn(1)})
	h.publish(t, model.EventDiscovered, item.PR)
	eventually(t, "the annotation", func() bool { return h.value(t, keyAPI) != nil && h.settled(t) })
	if n := len(h.linear.seen()); n != 2 {
		t.Errorf("%d Linear calls, want 2", n)
	}

	unrelated := item.PR
	unrelated.Labels = []string{"needs-review"}
	h.publish(t, model.EventUpdated, unrelated)
	eventually(t, "the update to be acknowledged", func() bool { return h.settled(t) })
	if n := len(h.linear.seen()); n != 2 {
		t.Errorf("%d Linear calls after an update that Linear does not see, want 2", n)
	}

	h.daemon.update(keyAPI, func(pr *model.PR) { pr.Title = "DEVOPS-2: add retries" })
	retitled := item.PR
	retitled.Title = "DEVOPS-2: add retries"
	h.publish(t, model.EventUpdated, retitled)
	eventually(t, "the retitled PR to be evaluated", func() bool { return len(h.linear.seen()) == 3 && h.settled(t) })
	if got := h.linear.seen()[2]; got.Title != retitled.Title || got.Branch != item.PR.HeadRef || got.URL != item.PR.URL {
		t.Errorf("Linear saw %+v", got)
	}

	h.publish(t, model.EventReopened, retitled)
	eventually(t, "the reopened PR to be evaluated", func() bool { return len(h.linear.seen()) == 4 && h.settled(t) })
	if n := len(h.daemon.writesOf(keyAPI, derive.UrgencyKey)); n != 1 {
		t.Errorf("%d writes, want 1: the value never changed", n)
	}
}

func TestLinearFailuresAreRedelivered(t *testing.T) {
	item := openItem(keyAPI)
	h := newHarness(t, item)
	h.linear.failNext(1)
	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })
	if !strings.Contains(h.logs.String(), `"failed":1`) {
		t.Error("sweep failure not counted")
	}

	h.linear.set(item.PR.URL, linear.Issue{Identifier: "DEVOPS-1", DueDate: dueIn(0)})
	h.linear.failNext(2)
	h.publish(t, model.EventDiscovered, item.PR)
	eventually(t, "the redelivered event to be acknowledged", func() bool {
		return h.value(t, keyAPI) != nil && h.settled(t)
	})
	if n := len(h.linear.seen()); n != 4 {
		t.Errorf("%d Linear calls, want 4: 1 for the sweep and 3 for the event", n)
	}
	if info := h.consumer(t); info.Delivered.Consumer != 3 {
		t.Errorf("event delivered %d times, want 3", info.Delivered.Consumer)
	}
	if !strings.Contains(h.logs.String(), `"msg":"evaluation failed, redelivering"`) {
		t.Error("nak not logged")
	}
}

func TestClosedPRsUntouched(t *testing.T) {
	item := openItem(keyAPI)
	item.PR.State = model.PRStateMerged
	item.Section = model.SectionNone
	stale, _ := json.Marshal(urgencyValue{Badge: "due today", Level: "crit", DueDate: dueIn(0), Reason: "issue A-1",
		Issues: []string{"A-1"}})
	item.Annotations = []model.Annotation{{PR: keyAPI, Key: derive.UrgencyKey, Value: stale}}
	h := newHarness(t, item)
	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })

	h.publish(t, model.EventDiscovered, item.PR)
	eventually(t, "the event to be acknowledged", func() bool { return h.settled(t) && h.daemon.getCalls(keyAPI) == 1 })
	if n := len(h.linear.seen()); n != 0 {
		t.Errorf("%d Linear calls for a closed PR", n)
	}
	if n := len(h.daemon.writes(keyAPI)); n != 0 {
		t.Errorf("closed PR written %d times", n)
	}
}

func TestUndecodableEventIsDropped(t *testing.T) {
	h := newHarness(t)
	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })
	if _, err := h.js.Publish(context.Background(), "devinbox.events.pr.acme.api.1.updated", []byte("{")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event to be terminated", func() bool { return h.settled(t) })
	if !strings.Contains(h.logs.String(), `"msg":"dropping undecodable event"`) {
		t.Error("drop not logged")
	}
}

func TestRunValidatesConfig(t *testing.T) {
	ctx := context.Background()
	if err := Run(ctx, Config{}); err == nil || !strings.Contains(err.Error(), "Linear") {
		t.Errorf("missing Linear: %v", err)
	}
	if err := Run(ctx, Config{Linear: &fakeLinear{}, HorizonDays: -1}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("negative horizon: %v", err)
	}
	p, err := newPlugin(Config{Linear: &fakeLinear{}})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.HorizonDays != 0 || p.cfg.SweepInterval != DefaultSweepInterval || p.log == nil || p.cfg.Now == nil {
		t.Errorf("defaults not applied: %+v", p.cfg)
	}
}

func TestFilterSubjects(t *testing.T) {
	want := []string{
		"devinbox.events.pr.*.*.*.discovered",
		"devinbox.events.pr.*.*.*.reopened",
		"devinbox.events.pr.*.*.*.updated",
	}
	if got := FilterSubjects(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
