package slackplugin

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

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/slack"
)

var (
	cest    = time.FixedZone("CEST", 2*60*60)
	now     = time.Date(2026, 10, 12, 10, 0, 0, 0, cest)
	timeout = 10 * time.Second
)

// ts returns the Slack timestamp of a message posted ago before now.
func ts(ago time.Duration) string {
	t := now.Add(-ago)
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openItem(key model.PRKey) api.InboxItem {
	return api.InboxItem{
		PR: model.PR{
			Key:      key,
			PRStatus: model.PRStatus{State: model.PRStateOpen},
			URL:      fmt.Sprintf("https://github.com/%s/%s/pull/%d", key.Owner, key.Repo, key.Number),
		},
		Classification: model.Classification{Section: model.SectionInvolved},
	}
}

// fakeDaemon is the daemon's API in memory; serve exposes it on NATS like the daemon does.
type fakeDaemon struct {
	mu     sync.Mutex
	clock  func() time.Time
	items  map[model.PRKey]*api.InboxItem
	writes []model.Annotation
	tracks []model.PRKey
	gets   int
	// ingest adds a PR the daemon is asked to track, as ingestion would.
	ingest  bool
	listErr error
}

func newFakeDaemon(clock func() time.Time, items ...api.InboxItem) *fakeDaemon {
	d := &fakeDaemon{clock: clock, items: make(map[model.PRKey]*api.InboxItem)}
	for _, it := range items {
		d.put(it)
	}
	return d
}

func (d *fakeDaemon) put(it api.InboxItem) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items[it.PR.Key] = &it
}

func (d *fakeDaemon) update(key model.PRKey, fn func(it *api.InboxItem)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d.items[key])
}

func (d *fakeDaemon) List(context.Context) (api.ListResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listErr != nil {
		return api.ListResponse{}, d.listErr
	}
	resp := api.ListResponse{Viewer: model.Viewer{Login: "me"}}
	for _, it := range d.items {
		item := *it
		item.Annotations = slices.Clone(it.Annotations)
		resp.Items = append(resp.Items, item)
	}
	return resp, nil
}

func (d *fakeDaemon) Get(_ context.Context, key model.PRKey) (api.InboxItem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gets++
	it, ok := d.items[key]
	if !ok {
		return api.InboxItem{}, fmt.Errorf("get %s: unknown PR %s", key, key)
	}
	item := *it
	item.Annotations = slices.Clone(it.Annotations)
	return item, nil
}

func (d *fakeDaemon) Annotate(_ context.Context, a model.Annotation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a.Source == "" {
		a.Source = Source
	}
	a.UpdatedAt = d.clock()
	it, ok := d.items[a.PR]
	if !ok {
		return fmt.Errorf("annotate %s: unknown PR", a.PR)
	}
	d.writes = append(d.writes, a)
	it.Annotations = slices.DeleteFunc(it.Annotations, func(o model.Annotation) bool {
		return o.CommentID == a.CommentID && o.Key == a.Key
	})
	if string(a.Value) != "null" {
		it.Annotations = append(it.Annotations, a)
	}
	return nil
}

func (d *fakeDaemon) Track(_ context.Context, key model.PRKey) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.items[key]; ok {
		return true, nil
	}
	d.tracks = append(d.tracks, key)
	if d.ingest {
		it := openItem(key)
		d.items[key] = &it
	}
	return false, nil
}

func (d *fakeDaemon) writesOf(key model.PRKey) []model.Annotation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(d.writes), func(a model.Annotation) bool { return a.PR != key })
}

func (d *fakeDaemon) tracked() []model.PRKey {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.tracks)
}

// value returns the decoded mentions annotation of key, or nil without one.
func (d *fakeDaemon) value(t *testing.T, key model.PRKey) *mentionsValue {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	it, ok := d.items[key]
	if !ok {
		return nil
	}
	for _, a := range it.Annotations {
		if a.CommentID != "" || a.Key != MentionsKey {
			continue
		}
		if a.Source != Source {
			t.Errorf("annotation source %q, want %q", a.Source, Source)
		}
		var v mentionsValue
		if err := json.Unmarshal(a.Value, &v); err != nil {
			t.Fatalf("annotation %s: %v", a.Value, err)
		}
		return &v
	}
	return nil
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
	ctx := context.Background()
	handle(api.SubjectList, func([]byte) any {
		resp, err := d.List(ctx)
		if err != nil {
			return api.ListResponse{Error: err.Error()}
		}
		return resp
	})
	handle(api.SubjectGet, func(data []byte) any {
		var req api.GetRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return api.GetResponse{Error: err.Error()}
		}
		it, err := d.Get(ctx, req.PR)
		if err != nil {
			return api.GetResponse{Error: "unknown PR " + req.PR.String()}
		}
		return api.GetResponse{Item: &it}
	})
	handle(api.SubjectAnnotate, func(data []byte) any {
		var req api.AnnotateRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return api.AnnotateResponse{Error: err.Error()}
		}
		if req.Annotation.Source == "" {
			return api.AnnotateResponse{Error: "no source"}
		}
		if err := d.Annotate(ctx, req.Annotation); err != nil {
			return api.AnnotateResponse{Error: err.Error()}
		}
		return api.AnnotateResponse{}
	})
	handle(api.SubjectTrack, func(data []byte) any {
		var req api.TrackRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return api.TrackResponse{Error: err.Error()}
		}
		known, _ := d.Track(ctx, req.PR)
		return api.TrackResponse{Known: known}
	})
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

var errSlack = errors.New("slack: 503 service unavailable")

type fakeSlack struct {
	mu       sync.Mutex
	authFail int
	channels []slack.Channel
	history  map[string][]slack.Message // by channel ID
	threads  map[threadKey][]slack.Message
	names    map[string]string
	histFail map[string]bool
	repFail  map[threadKey]bool
	repCalls map[threadKey]int
	oldest   []time.Time
}

func newFakeSlack() *fakeSlack {
	return &fakeSlack{
		channels: []slack.Channel{reviews, {ID: "C2", Name: "dev-chat"}},
		history:  make(map[string][]slack.Message),
		threads:  make(map[threadKey][]slack.Message),
		names:    map[string]string{"UALICE": "alice", "UBOB": "bob"},
		histFail: make(map[string]bool),
		repFail:  make(map[threadKey]bool),
		repCalls: make(map[threadKey]int),
	}
}

func (f *fakeSlack) setHistory(channel string, ms ...slack.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history[channel] = ms
}

func (f *fakeSlack) setThread(channel, parent string, ms ...slack.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threads[threadKey{channel, parent}] = ms
}

func (f *fakeSlack) replyCalls(channel, parent string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repCalls[threadKey{channel, parent}]
}

func (f *fakeSlack) AuthTest(context.Context) (slack.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authFail > 0 {
		f.authFail--
		return slack.Identity{}, errSlack
	}
	return slack.Identity{UserID: viewerID, TeamURL: teamURL}, nil
}

func (f *fakeSlack) Channel(_ context.Context, idOrName string) (slack.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.channels {
		if c.ID == idOrName || c.Name == strings.TrimPrefix(idOrName, "#") {
			return c, nil
		}
	}
	return slack.Channel{}, &slack.APIError{Method: "conversations.list", Code: "channel_not_found"}
}

func (f *fakeSlack) History(_ context.Context, channel string, oldest time.Time) ([]slack.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oldest = append(f.oldest, oldest)
	if f.histFail[channel] {
		return nil, errSlack
	}
	return slices.Clone(f.history[channel]), nil
}

func (f *fakeSlack) Replies(_ context.Context, channel, threadTS string) ([]slack.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := threadKey{channel, threadTS}
	f.repCalls[k]++
	if f.repFail[k] {
		return nil, errSlack
	}
	return slices.Clone(f.threads[k]), nil
}

func (f *fakeSlack) UserName(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.names[userID]; ok {
		return n, nil
	}
	return "", errors.New("user_not_found")
}

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

// newTestPlugin returns a plugin connected to Slack and d without NATS.
func newTestPlugin(t *testing.T, s *fakeSlack, d *fakeDaemon, c *clock, channels ...string) *plugin {
	t.Helper()
	if len(channels) == 0 {
		channels = []string{"dev-pr-reviews"}
	}
	p, err := newPlugin(Config{
		Channels: channels,
		Groups:   groups,
		Slack:    s,
		Logger:   slog.New(slog.NewJSONHandler(&syncBuffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:      c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.d = d
	if err := p.connectSlack(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCollectThreadCache(t *testing.T) {
	s := newFakeSlack()
	c := &clock{t: now}
	parent := slack.Message{TS: ts(3 * time.Hour), ThreadTS: ts(3 * time.Hour), User: "UALICE",
		Text: "new PR " + linkAPI, ReplyCount: 1, LatestReply: ts(2 * time.Hour)}
	first := slack.Message{TS: ts(2 * time.Hour), ThreadTS: parent.TS, User: "UBOB", Text: "<@UME> can you take this?"}
	s.setHistory("C1", parent)
	s.setThread("C1", parent.TS, parent, first)
	p := newTestPlugin(t, s, newFakeDaemon(c.Now), c)
	ctx := context.Background()

	ms, complete := p.collect(ctx, c.Now())
	if !complete || len(ms) != 1 || ms[0].ts != first.TS || !reflect.DeepEqual(ms[0].prs, []model.PRKey{keyAPI}) {
		t.Fatalf("first poll: %+v, complete %v", ms, complete)
	}
	if !s.oldest[0].Equal(now.Add(-DefaultLookback)) {
		t.Errorf("history since %v, want %v", s.oldest[0], now.Add(-DefaultLookback))
	}
	ms, _ = p.collect(ctx, c.Now())
	if n := s.replyCalls("C1", parent.TS); n != 1 || len(ms) != 1 {
		t.Errorf("unchanged thread: %d replies calls, %d mentions; want 1, 1", n, len(ms))
	}

	second := slack.Message{TS: ts(time.Hour), ThreadTS: parent.TS, User: "UALICE", Text: "<!subteam^SREV> " + linkWeb}
	parent.ReplyCount, parent.LatestReply = 2, second.TS
	s.setHistory("C1", parent)
	s.setThread("C1", parent.TS, parent, first, second)
	ms, _ = p.collect(ctx, c.Now())
	if n := s.replyCalls("C1", parent.TS); n != 2 || len(ms) != 2 {
		t.Errorf("changed thread: %d replies calls, %d mentions; want 2, 2", n, len(ms))
	}

	s.setHistory("C1")
	if ms, _ = p.collect(ctx, c.Now()); len(ms) != 0 || len(p.threads) != 0 {
		t.Errorf("parent out of the window: %d mentions, %d cached threads", len(ms), len(p.threads))
	}
}

func TestCollectBroadcastReplyCountsOnce(t *testing.T) {
	s := newFakeSlack()
	c := &clock{t: now}
	parent := slack.Message{TS: ts(3 * time.Hour), ThreadTS: ts(3 * time.Hour), User: "UALICE",
		Text: "new PR " + linkAPI, ReplyCount: 1, LatestReply: ts(2 * time.Hour)}
	reply := slack.Message{TS: ts(2 * time.Hour), ThreadTS: parent.TS, User: "UBOB", Subtype: "thread_broadcast",
		Text: "<@UME> please"}
	s.setHistory("C1", reply, parent)
	s.setThread("C1", parent.TS, parent, reply)
	p := newTestPlugin(t, s, newFakeDaemon(c.Now), c)
	ms, _ := p.collect(context.Background(), c.Now())
	if len(ms) != 1 || ms[0].threadTS != parent.TS || !reflect.DeepEqual(ms[0].prs, []model.PRKey{keyAPI}) {
		t.Errorf("got %+v", ms)
	}
}

func TestCollectFailures(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: now}
	parent := slack.Message{TS: ts(3 * time.Hour), ThreadTS: ts(3 * time.Hour), User: "UALICE",
		Text: "<@UME> " + linkAPI, ReplyCount: 1, LatestReply: ts(2 * time.Hour)}
	reply := slack.Message{TS: ts(2 * time.Hour), ThreadTS: parent.TS, User: "UBOB", Text: "<@UME> ping"}
	setup := func() *fakeSlack {
		s := newFakeSlack()
		s.setHistory("C1", parent)
		s.setThread("C1", parent.TS, parent, reply)
		s.setHistory("C2", slack.Message{TS: ts(time.Hour), User: "UBOB", Text: "<@UME> " + linkWeb})
		return s
	}

	t.Run("channel without an earlier read", func(t *testing.T) {
		s := setup()
		s.histFail["C1"] = true
		p := newTestPlugin(t, s, newFakeDaemon(c.Now), c, "dev-pr-reviews", "dev-chat")
		ms, complete := p.collect(ctx, c.Now())
		if complete || len(ms) != 1 || ms[0].channel.ID != "C2" {
			t.Errorf("got %+v, complete %v; want only the other channel, incomplete", ms, complete)
		}
	})
	t.Run("channel with an earlier read", func(t *testing.T) {
		s := setup()
		p := newTestPlugin(t, s, newFakeDaemon(c.Now), c)
		if ms, _ := p.collect(ctx, c.Now()); len(ms) != 2 {
			t.Fatalf("first poll: %+v", ms)
		}
		s.mu.Lock()
		s.histFail["C1"] = true
		s.mu.Unlock()
		ms, complete := p.collect(ctx, c.Now())
		if !complete || len(ms) != 2 || len(p.threads) != 1 {
			t.Errorf("got %+v, complete %v, %d threads; want the earlier read", ms, complete, len(p.threads))
		}
		c := &clock{t: now.Add(DefaultLookback - 150*time.Minute)}
		p.cfg.Now = c.Now
		if ms, _ := p.collect(ctx, c.Now()); len(ms) != 0 || len(p.threads) != 0 {
			t.Errorf("earlier read not limited to the window: %+v, %d threads", ms, len(p.threads))
		}
	})
	t.Run("thread without an earlier read", func(t *testing.T) {
		s := setup()
		s.repFail[threadKey{"C1", parent.TS}] = true
		p := newTestPlugin(t, s, newFakeDaemon(c.Now), c)
		ms, complete := p.collect(ctx, c.Now())
		if complete || len(ms) != 1 || ms[0].ts != parent.TS {
			t.Errorf("got %+v, complete %v; want the parent only, incomplete", ms, complete)
		}
		if len(p.threads) != 0 {
			t.Errorf("failed thread cached: %+v", p.threads)
		}
	})
	t.Run("thread with an earlier read", func(t *testing.T) {
		s := setup()
		p := newTestPlugin(t, s, newFakeDaemon(c.Now), c)
		p.collect(ctx, c.Now())
		changed := parent
		changed.ReplyCount, changed.LatestReply = 2, ts(time.Hour)
		s.mu.Lock()
		s.history["C1"] = []slack.Message{changed}
		s.repFail[threadKey{"C1", parent.TS}] = true
		s.mu.Unlock()
		ms, complete := p.collect(ctx, c.Now())
		if !complete || len(ms) != 2 {
			t.Errorf("got %+v, complete %v; want the earlier replies", ms, complete)
		}
		s.mu.Lock()
		s.repFail[threadKey{"C1", parent.TS}] = false
		s.mu.Unlock()
		p.collect(ctx, c.Now())
		if n := s.replyCalls("C1", parent.TS); n != 3 {
			t.Errorf("%d replies calls, want 3: the failed read is retried", n)
		}
	})
}

func mentionAt(ago time.Duration, kind tagKind, user, text string, prs ...model.PRKey) mention {
	return mention{channel: reviews, ts: ts(ago), user: user, text: text, kind: kind, prs: prs, time: now.Add(-ago)}
}

func TestApplyWritesUpdatesAndDeletes(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: now}
	d := newFakeDaemon(c.Now, openItem(keyAPI))
	p := newTestPlugin(t, newFakeSlack(), d, c)

	direct := mentionAt(2*time.Hour, tagDirect, "UALICE", "<@UME> please review "+linkAPI, keyAPI)
	reply := mentionAt(time.Hour, tagGroup, "UBOB", "can someone from <!subteam^SREV|@reviewers> take this?", keyAPI)
	reply.threadTS = direct.ts
	ms := []mention{direct, reply}

	st, err := p.apply(ctx, ms, true)
	if err != nil {
		t.Fatal(err)
	}
	want := &mentionsValue{
		Score:     DefaultScore,
		Attention: []string{"tagged your team in #dev-pr-reviews", "tagged you in #dev-pr-reviews"},
		Label:     "Slack",
		Links: []link{
			{ID: "@bob", Title: "can someone from @reviewers take this?",
				URL:    "https://acme-co.slack.com/archives/C1/p" + strings.Replace(reply.ts, ".", "", 1) + "?thread_ts=" + direct.ts + "&cid=C1",
				Detail: "#dev-pr-reviews · Oct 12 09:00"},
			{ID: "@alice", Title: "@UME please review https://github.com/acme/api/pull/1",
				URL:    "https://acme-co.slack.com/archives/C1/p" + strings.Replace(direct.ts, ".", "", 1),
				Detail: "#dev-pr-reviews · Oct 12 08:00"},
		},
	}
	if got := d.value(t, keyAPI); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if st.written != 1 || st.prs != 1 {
		t.Errorf("stats %+v", st)
	}

	c.advance(time.Minute)
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	if n := len(d.writesOf(keyAPI)); n != 1 {
		t.Errorf("%d writes after an unchanged poll, want 1", n)
	}

	d.update(keyAPI, func(it *api.InboxItem) {
		it.PR.Reviews = []model.Review{{Author: "me", State: "COMMENTED", SubmittedAt: now.Add(-90 * time.Minute)}}
	})
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	if got := d.value(t, keyAPI); got == nil || len(got.Links) != 1 || got.Links[0].ID != "@bob" ||
		!reflect.DeepEqual(got.Attention, []string{"tagged your team in #dev-pr-reviews"}) {
		t.Errorf("after a review between the tags: %+v", got)
	}

	d.update(keyAPI, func(it *api.InboxItem) {
		it.PR.Comments = []model.Comment{{Author: "Me", CreatedAt: now.Add(-30 * time.Minute)}}
	})
	if _, err := p.apply(ctx, ms, false); err != nil {
		t.Fatal(err)
	}
	if d.value(t, keyAPI) == nil {
		t.Error("deleted during an incomplete poll")
	}
	st, err = p.apply(ctx, ms, true)
	if err != nil {
		t.Fatal(err)
	}
	if d.value(t, keyAPI) != nil || st.deleted != 1 {
		t.Errorf("after a comment newer than all tags: %+v, stats %+v", d.value(t, keyAPI), st)
	}
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	writes := d.writesOf(keyAPI)
	if len(writes) != 3 || string(writes[2].Value) != "null" {
		t.Errorf("writes %v, want set, update, delete", writes)
	}
}

func TestApplyRewritesAnnotationOlderThanActivity(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: now}
	d := newFakeDaemon(c.Now, openItem(keyAPI))
	p := newTestPlugin(t, newFakeSlack(), d, c)
	ms := []mention{mentionAt(time.Hour, tagDirect, "UALICE", "<@UME> "+linkAPI, keyAPI)}
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	// The viewer's clock-skewed activity is newer than the write but older than the tag.
	d.update(keyAPI, func(it *api.InboxItem) {
		it.Annotations[0].UpdatedAt = now.Add(-3 * time.Hour)
		it.PR.Reviews = []model.Review{{Author: "me", SubmittedAt: now.Add(-2 * time.Hour)}}
	})
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	if n := len(d.writesOf(keyAPI)); n != 2 {
		t.Errorf("%d writes, want 2: the stale equal annotation is rewritten", n)
	}
}

func TestApplyDeletesStaleAnnotations(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: now}
	mine := openItem(keyAPI)
	mine.Annotations = []model.Annotation{{PR: keyAPI, Key: MentionsKey, Value: json.RawMessage(`{"score":10}`), Source: Source}}
	other := openItem(keyWeb)
	other.Annotations = []model.Annotation{
		{PR: keyWeb, Key: "linear.issues", Value: json.RawMessage(`{}`), Source: "dev-inbox-linear"},
		{PR: keyWeb, Key: MentionsKey, Value: json.RawMessage(`{}`), Source: "someone-else"},
	}
	d := newFakeDaemon(c.Now, mine, other)
	p := newTestPlugin(t, newFakeSlack(), d, c)

	if _, err := p.apply(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	if n := len(d.writesOf(keyAPI)); n != 0 {
		t.Errorf("incomplete poll deleted: %d writes", n)
	}
	st, err := p.apply(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if writes := d.writesOf(keyAPI); len(writes) != 1 || string(writes[0].Value) != "null" || writes[0].Key != MentionsKey {
		t.Errorf("writes %v, want a deletion", writes)
	}
	if n := len(d.writesOf(keyWeb)); n != 0 || st.deleted != 1 {
		t.Errorf("foreign annotations written %d times, stats %+v", n, st)
	}
}

func TestApplyTracksUnknownPRs(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: now}
	d := newFakeDaemon(c.Now)
	p := newTestPlugin(t, newFakeSlack(), d, c)
	ms := []mention{mentionAt(time.Hour, tagDirect, "UALICE", "<@UME> "+linkAPI, keyAPI)}

	st, err := p.apply(ctx, ms, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.tracked(); !reflect.DeepEqual(got, []model.PRKey{keyAPI}) || st.tracked != 1 {
		t.Fatalf("tracked %v, stats %+v", got, st)
	}
	c.advance(59 * time.Minute)
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	if n := len(d.tracked()); n != 1 {
		t.Errorf("tracked again within the hour: %d requests", n)
	}
	c.advance(time.Minute)
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	if n := len(d.tracked()); n != 2 {
		t.Errorf("%d track requests after an hour, want 2", n)
	}

	d.put(openItem(keyAPI))
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	if d.value(t, keyAPI) == nil || len(p.tracked) != 0 {
		t.Errorf("known PR not annotated or still remembered: %+v", p.tracked)
	}
	if n := len(d.tracked()); n != 2 {
		t.Errorf("%d track requests, want 2", n)
	}
}

func TestApplyMatchesLinksCaseInsensitively(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: now}
	d := newFakeDaemon(c.Now)
	d.put(openItem(keyAPI))
	p := newTestPlugin(t, newFakeSlack(), d, c)
	upper := model.PRKey{Owner: "Acme", Repo: "API", Number: keyAPI.Number}
	ms := []mention{mentionAt(time.Hour, tagDirect, "UALICE", "<@UME> https://github.com/Acme/API/pull/1", upper, keyAPI)}

	st, err := p.apply(ctx, ms, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.tracked()) != 0 || st.prs != 1 || st.written != 1 {
		t.Fatalf("tracked %v, stats %+v", d.tracked(), st)
	}
	if v := d.value(t, keyAPI); v == nil || len(v.Links) != 1 {
		t.Errorf("annotation %+v, want the message linked once", v)
	}
}

func TestApplySkipsClosedPRsAndListFailures(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: now}
	closed := openItem(keyAPI)
	closed.PR.State, closed.Section = model.PRStateMerged, model.SectionNone
	d := newFakeDaemon(c.Now, closed)
	p := newTestPlugin(t, newFakeSlack(), d, c)
	ms := []mention{mentionAt(time.Hour, tagDirect, "UALICE", "<@UME> "+linkAPI, keyAPI)}
	if _, err := p.apply(ctx, ms, true); err != nil {
		t.Fatal(err)
	}
	if n := len(d.writesOf(keyAPI)); n != 0 {
		t.Errorf("closed PR written %d times", n)
	}

	d.listErr = errors.New("no responders")
	if _, err := p.apply(ctx, ms, true); err == nil {
		t.Error("list failure not reported")
	}
	if d.gets != 1 {
		t.Errorf("%d gets, want 1: nothing after the list failed", d.gets)
	}
}

func TestRunValidatesConfig(t *testing.T) {
	ctx := context.Background()
	s := newFakeSlack()
	ch := []string{"dev-pr-reviews"}
	for _, tt := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "no Slack", cfg: Config{Channels: ch}, want: "Slack"},
		{name: "no channels", cfg: Config{Slack: s}, want: "channel"},
		{name: "negative score", cfg: Config{Slack: s, Channels: ch, Score: -1}, want: "negative"},
		{name: "negative poll interval", cfg: Config{Slack: s, Channels: ch, PollInterval: -time.Second}, want: "negative"},
		{name: "negative lookback", cfg: Config{Slack: s, Channels: ch, Lookback: -time.Hour}, want: "negative"},
	} {
		if err := Run(ctx, tt.cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	p, err := newPlugin(Config{Slack: s, Channels: ch})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.Score != DefaultScore || p.cfg.PollInterval != DefaultPollInterval || p.cfg.Lookback != DefaultLookback ||
		p.log == nil || p.cfg.Now == nil || len(p.cfg.Groups) != 0 {
		t.Errorf("defaults not applied: %+v", p.cfg)
	}
}

func TestConnectSlack(t *testing.T) {
	c := &clock{t: now}
	s := newFakeSlack()
	s.authFail = 1
	p, err := newPlugin(Config{Channels: []string{"C2", "#dev-pr-reviews"}, Slack: s, Now: c.Now,
		Logger: slog.New(slog.NewJSONHandler(&syncBuffer{}, nil))})
	if err != nil {
		t.Fatal(err)
	}
	p.retryBase, p.retryMax = time.Millisecond, time.Millisecond
	if err := p.connectSlack(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []slack.Channel{{ID: "C2", Name: "dev-chat"}, reviews}
	if !reflect.DeepEqual(p.channels, want) || p.me.UserID != viewerID || p.me.TeamURL != teamURL {
		t.Errorf("channels %+v, identity %+v", p.channels, p.me)
	}

	p, err = newPlugin(Config{Channels: []string{"dev-typo"}, Slack: s, Now: c.Now,
		Logger: slog.New(slog.NewJSONHandler(&syncBuffer{}, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.connectSlack(context.Background()); err == nil || !strings.Contains(err.Error(), "dev-typo") {
		t.Errorf("unknown channel: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.authFail = 1000
	p.retryBase, p.retryMax = time.Millisecond, time.Millisecond
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := p.connectSlack(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled auth.test retries: %v", err)
	}
}

func TestValueJSON(t *testing.T) {
	raw, err := json.Marshal(mentionsValue{Score: 10, Attention: []string{"tagged you in #dev-pr-reviews"}, Label: "Slack",
		Links: []link{{ID: "@alice", Title: "t", URL: "u", Detail: "d"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"score":10,"attention":["tagged you in #dev-pr-reviews"],"label":"Slack",` +
		`"links":[{"id":"@alice","title":"t","url":"u","detail":"d"}]}`
	if string(raw) != want {
		t.Errorf("got %s, want %s", raw, want)
	}
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

func TestEndToEnd(t *testing.T) {
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: server.RANDOM_PORT, NoLog: true, NoSigs: true})
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

	c := &clock{t: now}
	d := newFakeDaemon(c.Now, openItem(keyAPI))
	d.ingest = true
	d.serve(t, nc)
	s := newFakeSlack()
	s.authFail = 2
	parent := slack.Message{TS: ts(3 * time.Hour), ThreadTS: ts(3 * time.Hour), User: "UALICE",
		Text: "please review " + linkWeb, ReplyCount: 1, LatestReply: ts(2 * time.Hour)}
	direct := slack.Message{TS: ts(4 * time.Hour), User: "UALICE", Text: "<@UME> " + linkAPI}
	s.setHistory("C1", parent, direct)
	s.setThread("C1", parent.TS, parent,
		slack.Message{TS: ts(2 * time.Hour), ThreadTS: parent.TS, User: "UBOB", Text: "<!subteam^SREV> anyone?"})

	logs := &syncBuffer{}
	p, err := newPlugin(Config{
		NATSURL:      ns.ClientURL(),
		Channels:     []string{"#dev-pr-reviews"},
		Groups:       groups,
		PollInterval: 20 * time.Millisecond,
		Slack:        s,
		Logger:       slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:          c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.retryBase, p.retryMax = 10*time.Millisecond, 20*time.Millisecond
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

	eventually(t, "the annotations", func() bool { return d.value(t, keyAPI) != nil && d.value(t, keyWeb) != nil })
	if got := d.tracked(); !reflect.DeepEqual(got, []model.PRKey{keyWeb}) {
		t.Errorf("tracked %v, want the unknown PR once", got)
	}
	if got := d.value(t, keyWeb); got.Links[0].ID != "@bob" ||
		!reflect.DeepEqual(got.Attention, []string{"tagged your team in #dev-pr-reviews"}) {
		t.Errorf("inherited thread tag: %+v", got)
	}
	if !strings.Contains(logs.String(), `"msg":"slack auth.test"`) {
		t.Error("auth.test retries not logged")
	}
	if _, err := nc.Request("$SRV.PING."+ServiceName, nil, timeout); err != nil {
		t.Errorf("service %s not registered: %v", ServiceName, err)
	}
	if n := s.replyCalls("C1", parent.TS); n != 1 {
		t.Errorf("%d replies calls for an unchanged thread, want 1", n)
	}

	s.setHistory("C1", parent)
	eventually(t, "the deletion", func() bool { return d.value(t, keyAPI) == nil })
	polls := strings.Count(logs.String(), `"msg":"poll done"`)
	eventually(t, "two more polls", func() bool { return strings.Count(logs.String(), `"msg":"poll done"`) >= polls+2 })
	if writes := d.writesOf(keyAPI); len(writes) != 2 || string(writes[1].Value) != "null" {
		t.Errorf("writes %v, want set and delete", writes)
	}
	if n := len(d.writesOf(keyWeb)); n != 1 {
		t.Errorf("%d writes of the unchanged PR, want 1", n)
	}
}
