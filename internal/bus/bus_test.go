package bus

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

func startBus(t *testing.T, dir string) *Bus {
	t.Helper()
	b, err := Start(Config{Port: -1, StoreDir: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.relayBackoffMin = 10 * time.Millisecond
	b.relayBackoffMax = 50 * time.Millisecond
	b.retryBase = 10 * time.Millisecond
	b.retryMax = 50 * time.Millisecond
	return b
}

func newEvent(t *testing.T, owner string, number int, typ string) model.Event {
	t.Helper()
	key := model.PRKey{Owner: owner, Repo: "frontend", Number: number}
	ev, err := model.NewEvent(typ, key, model.EventData{PR: model.PR{Key: key, Title: "title"}})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func newEvents(t *testing.T, n int) []model.Event {
	evs := make([]model.Event, n)
	for i := range evs {
		evs[i] = newEvent(t, "acme-co", i+1, model.EventUpdated)
	}
	return evs
}

func ids(evs []model.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.ID
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// streamIDs returns the CloudEvent IDs stored in the events stream, in stream order.
func streamIDs(t *testing.T, b *Bus) []string {
	t.Helper()
	ctx := context.Background()
	s, err := b.js.Stream(ctx, api.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	if info.State.Msgs == 0 {
		return out
	}
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		msg, err := s.GetMsg(ctx, seq)
		if err != nil {
			t.Fatalf("get msg %d: %v", seq, err)
		}
		var ce api.CloudEvent
		if err := json.Unmarshal(msg.Data, &ce); err != nil {
			t.Fatalf("decode msg %d: %v", seq, err)
		}
		out = append(out, ce.ID)
	}
	return out
}

// consumerInfo returns the state of the named consumer, zero until Handle has created it.
func consumerInfo(t *testing.T, b *Bus, name string) *jetstream.ConsumerInfo {
	t.Helper()
	c, err := b.js.Consumer(context.Background(), api.StreamName, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return &jetstream.ConsumerInfo{}
	}
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func connect(t *testing.T, b *Bus) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(b.ClientURL())
	if err != nil {
		t.Fatalf("connect to %s: %v", b.ClientURL(), err)
	}
	t.Cleanup(nc.Close)
	return nc
}

type fakeOutbox struct {
	mu        sync.Mutex
	events    []model.Event
	published map[string]bool
	marked    []string
	reads     int
}

func (f *fakeOutbox) add(evs ...model.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evs...)
}

func (f *fakeOutbox) PendingEvents(_ context.Context, limit int) ([]model.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	var out []model.Event
	for _, ev := range f.events {
		if len(out) == limit {
			break
		}
		if !f.published[ev.ID] {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkPublished(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.published == nil {
		f.published = map[string]bool{}
	}
	for _, id := range ids {
		f.published[id] = true
	}
	f.marked = append(f.marked, ids...)
	return nil
}

func (f *fakeOutbox) snapshot() (marked []string, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.marked), f.reads
}

func runRelay(t *testing.T, b *Bus, src Outbox) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.RunRelay(ctx, src) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("RunRelay: %v", err)
		}
	})
}

// handle registers fn with b.Handle. The returned stop cancels it and waits until no handler of b
// consumes anymore.
func handle(t *testing.T, b *Bus, name, filter string, fn HandlerFunc) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Handle(ctx, name, filter, fn); err != nil {
		cancel()
		t.Fatalf("Handle: %v", err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			b.handlers.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}

type recorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *recorder) record(ev api.CloudEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, ev.ID)
}

func (r *recorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ids)
}

func TestStartClose(t *testing.T) {
	b, err := Start(Config{Port: -1, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !b.Conn().IsConnected() {
		t.Fatal("in-process connection not connected")
	}
	nc, err := nats.Connect(b.ClientURL())
	if err != nil {
		t.Fatalf("connect to %s: %v", b.ClientURL(), err)
	}
	nc.Close()

	s, err := b.js.Stream(context.Background(), api.StreamName)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cfg := s.CachedInfo().Config
	if !slices.Equal(cfg.Subjects, []string{api.EventsSubjectAll}) || cfg.Storage != jetstream.FileStorage ||
		cfg.MaxAge != defaultMaxAge || cfg.Duplicates != duplicateWindow {
		t.Fatalf("unexpected stream config: subjects=%v storage=%v maxAge=%v duplicates=%v",
			cfg.Subjects, cfg.Storage, cfg.MaxAge, cfg.Duplicates)
	}

	url := b.ClientURL()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !b.Conn().IsClosed() {
		t.Fatal("connection still open after Close")
	}
	if nc, err := nats.Connect(url, nats.NoReconnect()); err == nil {
		nc.Close()
		t.Fatal("server still accepts connections after Close")
	}
}

func TestStartRequiresStoreDir(t *testing.T) {
	if _, err := Start(Config{Port: -1}); err == nil {
		t.Fatal("Start without StoreDir succeeded")
	}
}

func TestStartPortInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port

	b, err := Start(Config{Port: port, StoreDir: t.TempDir()})
	if err == nil {
		_ = b.Close()
		t.Fatal("Start on a taken port succeeded")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("error does not explain the taken port: %v", err)
	}
}

func TestPublish(t *testing.T) {
	b := startBus(t, t.TempDir())
	nc := connect(t, b)
	sub, err := nc.SubscribeSync(api.EventsSubjectAll)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	ev := newEvent(t, "Acme-CO", 42, model.EventMerged)
	if err := b.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("no message received: %v", err)
	}
	if want := "devinbox.events.pr.acme-co.frontend.42.merged"; msg.Subject != want {
		t.Errorf("subject = %q, want %q", msg.Subject, want)
	}
	if got := msg.Header.Get("Nats-Msg-Id"); got != ev.ID {
		t.Errorf("Nats-Msg-Id = %q, want %q", got, ev.ID)
	}
	if got := msg.Header.Get("Content-Type"); got != "application/cloudevents+json" {
		t.Errorf("Content-Type = %q", got)
	}
	var ce api.CloudEvent
	if err := json.Unmarshal(msg.Data, &ce); err != nil {
		t.Fatalf("decode CloudEvent: %v", err)
	}
	want := api.NewCloudEvent(ev)
	if ce.SpecVersion != "1.0" || ce.ID != ev.ID || ce.Type != "devinbox.pr.merged" ||
		ce.Source != "https://github.com/Acme-CO/frontend" || ce.Subject != "pull/42" ||
		!ce.Time.Equal(ev.Time) || ce.DataContentType != want.DataContentType {
		t.Errorf("CloudEvent = %+v, want %+v", ce, want)
	}
	var data model.EventData
	if err := json.Unmarshal(ce.Data, &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data.PR.Key != ev.PR || data.PR.Title != "title" {
		t.Errorf("data.PR = %+v", data.PR)
	}
}

func TestPublishDeduplicatesByEventID(t *testing.T) {
	b := startBus(t, t.TempDir())
	ev := newEvent(t, "acme-co", 1, model.EventUpdated)
	for range 2 {
		if err := b.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	if got := streamIDs(t, b); !slices.Equal(got, []string{ev.ID}) {
		t.Fatalf("stream = %v, want one message %s", got, ev.ID)
	}
}

func TestRunRelayPublishesInOrder(t *testing.T) {
	b := startBus(t, t.TempDir())
	evs := newEvents(t, 2*relayBatchSize+5)
	src := &fakeOutbox{}
	src.add(evs...)

	runRelay(t, b, src)

	waitFor(t, "all events marked published", func() bool {
		marked, _ := src.snapshot()
		return len(marked) == len(evs)
	})
	marked, _ := src.snapshot()
	if !slices.Equal(marked, ids(evs)) {
		t.Fatal("events marked published out of order")
	}
	if !slices.Equal(streamIDs(t, b), ids(evs)) {
		t.Fatal("stream does not hold exactly the outbox events in order")
	}
}

func TestRunRelayRetriesFailedPublish(t *testing.T) {
	b := startBus(t, t.TempDir())
	ctx := context.Background()
	s, err := b.js.Stream(ctx, api.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.CachedInfo().Config

	limited := cfg
	limited.MaxMsgs = 2
	limited.Discard = jetstream.DiscardNew
	if _, err := b.js.UpdateStream(ctx, limited); err != nil {
		t.Fatal(err)
	}

	evs := newEvents(t, 5)
	src := &fakeOutbox{}
	src.add(evs...)
	runRelay(t, b, src)

	// The stream rejects the third event; wait for a few retries.
	waitFor(t, "relay retries", func() bool {
		_, reads := src.snapshot()
		return reads >= 3
	})
	if marked, _ := src.snapshot(); !slices.Equal(marked, ids(evs[:2])) {
		t.Fatalf("marked %v while publishing fails, want the first two events", marked)
	}
	if got := streamIDs(t, b); !slices.Equal(got, ids(evs[:2])) {
		t.Fatalf("stream = %v while publishing fails, want the first two events", got)
	}

	if _, err := b.js.UpdateStream(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "all events marked published", func() bool {
		marked, _ := src.snapshot()
		return len(marked) == len(evs)
	})
	if marked, _ := src.snapshot(); !slices.Equal(marked, ids(evs)) {
		t.Fatalf("marked = %v, want all events in order", marked)
	}
	if got := streamIDs(t, b); !slices.Equal(got, ids(evs)) {
		t.Fatalf("stream = %v, want all events in order", got)
	}
}

func TestKickWakesRelay(t *testing.T) {
	b := startBus(t, t.TempDir())
	b.relayPoll = time.Hour
	for range 3 {
		b.Kick() // never blocks, even without a running relay
	}

	src := &fakeOutbox{}
	runRelay(t, b, src)
	waitFor(t, "relay to go idle", func() bool {
		_, reads := src.snapshot()
		return reads >= 2 // the initial pass plus the pass for the pre-start kick
	})

	ev := newEvent(t, "acme-co", 1, model.EventUpdated)
	src.add(ev)
	b.Kick()
	waitFor(t, "kicked event marked published", func() bool {
		marked, _ := src.snapshot()
		return len(marked) == 1
	})
	if got := streamIDs(t, b); !slices.Equal(got, []string{ev.ID}) {
		t.Fatalf("stream = %v, want %s", got, ev.ID)
	}
}

func TestHandleFiltersAndAcks(t *testing.T) {
	b := startBus(t, t.TempDir())
	ctx := context.Background()
	var rec recorder
	var types []string
	var mu sync.Mutex
	handle(t, b, "acme", "devinbox.events.pr.acme-co.>", func(_ context.Context, ev api.CloudEvent) error {
		mu.Lock()
		types = append(types, ev.Type)
		mu.Unlock()
		rec.record(ev)
		return nil
	})

	want := []model.Event{newEvent(t, "Acme-CO", 1, model.EventDiscovered), newEvent(t, "acme-co", 2, model.EventMerged)}
	for _, ev := range []model.Event{want[0], newEvent(t, "other", 3, model.EventUpdated), want[1]} {
		if err := b.Publish(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "matching events handled and acked", func() bool {
		return len(rec.get()) == 2 && consumerInfo(t, b, "acme").AckFloor.Stream == 3
	})
	if got := rec.get(); !slices.Equal(got, ids(want)) {
		t.Fatalf("handled %v, want %v", got, ids(want))
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(types, []string{"devinbox.pr.discovered", "devinbox.pr.merged"}) {
		t.Fatalf("types = %v", types)
	}
}

func TestHandleRedeliversOnError(t *testing.T) {
	b := startBus(t, t.TempDir())
	var mu sync.Mutex
	calls := 0
	handle(t, b, "flaky", api.EventsSubjectAll, func(context.Context, api.CloudEvent) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})

	if err := b.Publish(context.Background(), newEvent(t, "acme-co", 1, model.EventUpdated)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "event acked", func() bool {
		info := consumerInfo(t, b, "flaky")
		return info.AckFloor.Stream == 1 && info.NumAckPending == 0
	})
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("handler called %d times, want 3", calls)
	}
}

func TestHandleDropsEventAfterMaxDeliveries(t *testing.T) {
	b := startBus(t, t.TempDir())
	b.maxDeliver = 3
	failing := newEvent(t, "acme-co", 1, model.EventUpdated)
	next := newEvent(t, "acme-co", 2, model.EventUpdated)
	var rec recorder
	handle(t, b, "failing", api.EventsSubjectAll, func(_ context.Context, ev api.CloudEvent) error {
		rec.record(ev)
		if ev.ID == failing.ID {
			return errors.New("permanent")
		}
		return nil
	})

	for _, ev := range []model.Event{failing, next} {
		if err := b.Publish(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both events settled", func() bool {
		info := consumerInfo(t, b, "failing")
		return info.AckFloor.Stream == 2 && info.NumAckPending == 0
	})
	got := rec.get()
	if n := strings.Count(strings.Join(got, ","), failing.ID); n != 3 {
		t.Fatalf("failing event handled %d times, want 3", n)
	}
	if !slices.Contains(got, next.ID) {
		t.Fatal("next event not handled")
	}
}

func TestHandleTerminatesUndecodableEvent(t *testing.T) {
	b := startBus(t, t.TempDir())
	ctx := context.Background()
	var rec recorder
	handle(t, b, "strict", api.EventsSubjectAll, func(_ context.Context, ev api.CloudEvent) error {
		rec.record(ev)
		return nil
	})

	if _, err := b.js.Publish(ctx, "devinbox.events.pr.acme-co.frontend.1.updated", []byte("not json")); err != nil {
		t.Fatal(err)
	}
	ev := newEvent(t, "acme-co", 2, model.EventUpdated)
	if err := b.Publish(ctx, ev); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both messages settled", func() bool {
		info := consumerInfo(t, b, "strict")
		return info.AckFloor.Stream == 2 && info.NumAckPending == 0 && info.NumRedelivered == 0
	})
	if got := rec.get(); !slices.Equal(got, []string{ev.ID}) {
		t.Fatalf("handled %v, want only %s", got, ev.ID)
	}
}

func TestHandleResumesDurableConsumer(t *testing.T) {
	b := startBus(t, t.TempDir())
	ctx := context.Background()
	var rec recorder
	fn := func(_ context.Context, ev api.CloudEvent) error {
		rec.record(ev)
		return nil
	}

	stop := handle(t, b, "durable", api.EventsSubjectAll, fn)
	first := newEvent(t, "acme-co", 1, model.EventUpdated)
	if err := b.Publish(ctx, first); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first event acked", func() bool { return consumerInfo(t, b, "durable").AckFloor.Stream == 1 })
	stop()

	// Published while the handler is down: the existing consumer must catch up on it.
	second := newEvent(t, "acme-co", 2, model.EventUpdated)
	if err := b.Publish(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := rec.get(); !slices.Equal(got, []string{first.ID}) {
		t.Fatalf("handled %v after stop, want only the first event", got)
	}

	handle(t, b, "durable", api.EventsSubjectAll, fn)
	waitFor(t, "second event acked", func() bool {
		info := consumerInfo(t, b, "durable")
		return info.AckFloor.Stream == 2 && info.NumAckPending == 0
	})
	if got := rec.get(); !slices.Equal(got, []string{first.ID, second.ID}) {
		t.Fatalf("handled %v, want each event once", got)
	}
}

func TestHandleNewConsumerStartsWhenHandleReturns(t *testing.T) {
	b := startBus(t, t.TempDir())
	ctx := context.Background()
	old := newEvent(t, "acme-co", 1, model.EventUpdated)
	if err := b.Publish(ctx, old); err != nil {
		t.Fatal(err)
	}

	var rec recorder
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	err := b.Handle(hctx, "fresh", api.EventsSubjectAll, func(_ context.Context, ev api.CloudEvent) error {
		rec.record(ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	cur := newEvent(t, "acme-co", 2, model.EventUpdated)
	if err := b.Publish(ctx, cur); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new event acked", func() bool {
		info := consumerInfo(t, b, "fresh")
		return info.AckFloor.Stream == 2 && info.NumAckPending == 0
	})
	if got := rec.get(); !slices.Equal(got, []string{cur.ID}) {
		t.Fatalf("handled %v, want only the event published after registration", got)
	}
}

func TestCloseWaitsForInFlightHandler(t *testing.T) {
	b := startBus(t, t.TempDir())
	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	err := b.Handle(context.Background(), "slow", api.EventsSubjectAll, func(context.Context, api.CloudEvent) error {
		close(entered)
		<-release
		finished.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := b.Publish(context.Background(), newEvent(t, "acme-co", 1, model.EventUpdated)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler not called")
	}

	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while a handler call was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Close returned before the handler call finished")
	}

	if err := b.Handle(context.Background(), "late", api.EventsSubjectAll,
		func(context.Context, api.CloudEvent) error { return nil }); err == nil {
		t.Fatal("Handle after Close succeeded")
	}
}

func TestServe(t *testing.T) {
	b := startBus(t, t.TempDir())
	type echo struct {
		Text string `json:"text"`
	}
	err := b.Serve("devinbox.api.test", func(ctx context.Context, req []byte) (any, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, errors.New("request context without deadline")
		}
		var in echo
		if err := json.Unmarshal(req, &in); err != nil {
			return nil, err
		}
		if in.Text == "" {
			return nil, errors.New("text is required")
		}
		return echo{Text: strings.ToUpper(in.Text)}, nil
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	nc := connect(t, b)

	reply, err := nc.Request("devinbox.api.test", []byte(`{"text":"hi"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := string(reply.Data); got != `{"text":"HI"}` {
		t.Fatalf("reply = %s", got)
	}

	reply, err = nc.Request("devinbox.api.test", []byte(`{}`), 5*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := string(reply.Data); got != `{"error":"text is required"}` {
		t.Fatalf("error reply = %s", got)
	}
}

func TestRestartKeepsStreamAndConsumers(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	var rec recorder
	fn := func(_ context.Context, ev api.CloudEvent) error {
		rec.record(ev)
		return nil
	}

	b := startBus(t, dir)
	stop := handle(t, b, "restart", api.EventsSubjectAll, fn)
	evs := newEvents(t, 3)
	for _, ev := range evs[:2] {
		if err := b.Publish(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "events acked", func() bool { return consumerInfo(t, b, "restart").AckFloor.Stream == 2 })
	stop()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b = startBus(t, dir)
	if got := streamIDs(t, b); !slices.Equal(got, ids(evs[:2])) {
		t.Fatalf("stream after restart = %v, want %v", got, ids(evs[:2]))
	}
	if err := b.Publish(ctx, evs[2]); err != nil {
		t.Fatal(err)
	}
	handle(t, b, "restart", api.EventsSubjectAll, fn)
	waitFor(t, "new event acked", func() bool {
		info := consumerInfo(t, b, "restart")
		return info.AckFloor.Stream == 3 && info.NumAckPending == 0
	})
	if got := rec.get(); !slices.Equal(got, ids(evs)) {
		t.Fatalf("handled %v, want each event once", got)
	}
}
