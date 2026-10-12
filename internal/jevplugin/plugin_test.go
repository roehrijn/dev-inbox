package jevplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
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
	"github.com/roehrijn/dev-inbox/internal/jev"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const me = "me"

var (
	t0      = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	keyAPI  = model.PRKey{Owner: "acme", Repo: "api", Number: 1}
	keyWeb  = model.PRKey{Owner: "other", Repo: "web", Number: 2}
	viewer  = model.Viewer{Login: me}
	timeout = 10 * time.Second
)

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

// foreignItem is someone else's open PR on which the viewer's review is requested directly.
func foreignItem(key model.PRKey, comments ...model.Comment) api.InboxItem {
	return api.InboxItem{
		PR: model.PR{
			Key:            key,
			PRStatus:       model.PRStatus{State: model.PRStateOpen, HeadSHA: "head"},
			Title:          "Add retries to the client",
			Author:         "alice",
			ReviewRequests: []model.ReviewRequest{{Login: me}},
			Comments:       comments,
		},
		Classification: model.Classification{Section: model.SectionReviewRequested, Direct: true},
	}
}

func inline(id, author, thread string, minute int, body string) model.Comment {
	return model.Comment{ID: id, Kind: model.CommentInline, Author: author, ThreadID: thread, Path: "client.go",
		CreatedAt: at(minute), Body: body}
}

func issue(id, author string, minute int, body string) model.Comment {
	return model.Comment{ID: id, Kind: model.CommentIssue, Author: author, CreatedAt: at(minute), Body: body}
}

func intentAnnotation(key model.PRKey, commentID string, v intentValue) model.Annotation {
	raw, _ := json.Marshal(v)
	return model.Annotation{PR: key, CommentID: commentID, Key: KeyIntent, Value: raw, UpdatedAt: at(0)}
}

// fakeDaemon answers the daemon API from in-memory items.
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

// addComment adds c to the PR as a fresh fetch would.
func (d *fakeDaemon) addComment(key model.PRKey, c model.Comment) {
	d.mu.Lock()
	defer d.mu.Unlock()
	it := d.items[key]
	it.PR.Comments = append(slices.Clone(it.PR.Comments), c)
}

// update changes the stored PR as a fresh fetch would.
func (d *fakeDaemon) update(key model.PRKey, fn func(pr *model.PR)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	it := d.items[key]
	pr := it.PR
	pr.Comments = slices.Clone(pr.Comments)
	fn(&pr)
	it.PR = pr
}

func (d *fakeDaemon) annotation(key model.PRKey, commentID, annKey string) (model.Annotation, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.items[key].Annotations {
		if a.CommentID == commentID && a.Key == annKey {
			return a, true
		}
	}
	return model.Annotation{}, false
}

func (d *fakeDaemon) annotateCalls() []model.Annotation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.annotated)
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
		resp := api.ListResponse{Viewer: viewer}
		for _, it := range d.items {
			item := *it
			item.PR.Comments = nil
			item.Annotations = slices.DeleteFunc(slices.Clone(it.Annotations), func(a model.Annotation) bool { return a.CommentID != "" })
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

type answer struct {
	choice string
	p      float64
}

// seenRequest is a Jev request as the fake API decodes it.
type seenRequest struct {
	State     prState `json:"state"`
	Model     string  `json:"model"`
	Questions map[string]struct {
		Type         string              `json:"type"`
		Instructions commentInstructions `json:"instructions"`
		Criteria     json.RawMessage     `json:"criteria"`
	} `json:"questions"`
}

// fakeJev answers each question with the answer configured for its comment ID, by default not_for_me.
type fakeJev struct {
	mu       sync.Mutex
	answers  map[string]answer
	fail     int // the next fail requests get a 503
	requests []seenRequest
	auth     []string
}

func (f *fakeJev) setAnswers(a map[string]answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	maps.Copy(f.answers, a)
}

func (f *fakeJev) failNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = n
}

func (f *fakeJev) seen() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeJev) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.auth)
}

func (f *fakeJev) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req seenRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if f.fail > 0 {
		f.fail--
		http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
		return
	}
	answers := make(map[string]jev.Answer)
	for id := range req.Questions {
		a, ok := f.answers[id]
		if !ok {
			a = answer{IntentNotForMe, 0.95}
		}
		probs := make(map[string]float64)
		for _, name := range Intents {
			probs[name] = (1 - a.p) / float64(len(Intents)-1)
		}
		probs[a.choice] = a.p
		answers[id] = jev.Answer{Type: "choice", Choice: a.choice, Confidence: a.p, Probabilities: probs}
	}
	_ = json.NewEncoder(w).Encode(jev.Response{Model: "jev-1.13.0", Answers: answers, Usage: jev.Usage{InputTokens: 100}})
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

type harness struct {
	url    string
	js     jetstream.JetStream
	daemon *fakeDaemon
	jev    *fakeJev
	jevURL string
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
		jev:    &fakeJev{answers: make(map[string]answer)},
		logs:   &syncBuffer{},
	}
	for _, it := range items {
		h.daemon.put(it)
	}
	h.daemon.serve(t, nc)
	srv := httptest.NewServer(h.jev)
	t.Cleanup(srv.Close)
	h.jevURL = srv.URL
	return h
}

// start runs the plugin until the returned stop is called or the test ends.
func (h *harness) start(t *testing.T, configure func(*Config)) (stop func()) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client, err := jev.New(jev.Config{APIKey: "test-key", BaseURL: h.jevURL, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		NATSURL:   h.url,
		Threshold: DefaultThreshold,
		Weights:   DefaultWeights,
		Debounce:  50 * time.Millisecond,
		Jev:       client,
		Logger:    logger,
	}
	if configure != nil {
		configure(&cfg)
	}
	p := newPlugin(cfg)
	p.retryBase = 20 * time.Millisecond
	p.retryMax = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
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
	t.Cleanup(stop)
	return stop
}

// sweeps counts the completed sweeps in the log.
func (h *harness) sweeps() int { return strings.Count(h.logs.String(), `"msg":"sweep done"`) }

func (h *harness) publish(t *testing.T, typ string, key model.PRKey) {
	t.Helper()
	ev, err := model.NewEvent(typ, key, model.EventData{PR: model.PR{Key: key}})
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

func decodeValue(t *testing.T, a model.Annotation) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(a.Value, &m); err != nil {
		t.Fatalf("annotation %s on %q: %v", a.Key, a.CommentID, err)
	}
	return m
}

func TestSweepClassifiesCandidates(t *testing.T) {
	item := foreignItem(keyAPI,
		inline("mine", me, "T1", 0, "Please handle the error here."),
		issue("my-top", me, 0, "Looks mostly good."),
		issue("old", "alice", -5, "Ready for review."),
		inline("reply", "alice", "T1", 5, "Why? It cannot fail."),
		inline("other-thread", "bob", "T2", 6, "nit: rename this"),
		inline("mention", "bob", "T2", 7, "@me what do you think?"),
		issue("top", "carol", 8, "Thanks for the review!"),
		issue("bot", "dependabot[bot]", 9, "Bumped a dependency."),
		issue("classified", "alice", 10, "Please take another look."),
	)
	item.Annotations = []model.Annotation{
		intentAnnotation(keyAPI, "classified", intentValue{Intent: IntentRereviewRequest, Probability: 0.9}),
	}
	h := newHarness(t, item)
	h.jev.setAnswers(map[string]answer{
		"reply":   {IntentPushback, 0.9},
		"mention": {IntentQuestionForMe, 0.8},
		"top":     {IntentAck, 0.95},
	})

	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })

	reqs := h.jev.seen()
	if len(reqs) != 1 {
		t.Fatalf("got %d Jev requests, want 1", len(reqs))
	}
	req := reqs[0]
	if got := slices.Sorted(maps.Keys(req.Questions)); !reflect.DeepEqual(got, []string{"mention", "reply", "top"}) {
		t.Fatalf("classified comments %v, want mention, reply and top", got)
	}
	if req.State.PullRequest.Title != "Add retries to the client" || req.State.PullRequest.Author != "alice" ||
		req.State.Viewer != me || req.Model != jev.DefaultModel {
		t.Errorf("state %+v, model %q", req.State, req.Model)
	}
	for id, want := range map[string]commentInstructions{
		"reply": {
			Comment: commentView{Author: "alice", Kind: "inline review comment", Path: "client.go", Body: "Why? It cannot fail."},
			Thread:  []threadEntry{{Author: me, Body: "Please handle the error here."}},
		},
		"mention": {
			Comment: commentView{Author: "bob", Kind: "inline review comment", Path: "client.go",
				Body: "@me what do you think?", MentionsViewer: true},
			Thread: []threadEntry{{Author: "bob", Body: "nit: rename this"}},
		},
		"top": {
			Comment: commentView{Author: "carol", Kind: "conversation comment", Body: "Thanks for the review!"},
			Thread:  []threadEntry{{Author: me, Body: "Looks mostly good."}},
		},
	} {
		q := req.Questions[id]
		got := q.Instructions
		got.Question = ""
		if q.Type != "choice" || !reflect.DeepEqual(got, want) {
			t.Errorf("question %s: type %q, instructions %+v, want %+v", id, q.Type, got, want)
		}
		if !strings.HasPrefix(string(q.Criteria), `{"question_for_me":`) {
			t.Errorf("question %s: criteria %s do not start with question_for_me", id, q.Criteria)
		}
	}
	if auth := h.jev.authHeaders(); auth[0] != "Bearer test-key" {
		t.Errorf("authorization %q", auth[0])
	}

	for id, want := range map[string]map[string]any{
		"reply":   {"intent": IntentPushback, "probability": 0.9},
		"mention": {"intent": IntentQuestionForMe, "probability": 0.8},
		"top":     {"intent": IntentAck, "probability": 0.95, derive.AnnotationAttention: false},
	} {
		a, ok := h.daemon.annotation(keyAPI, id, KeyIntent)
		if !ok {
			t.Errorf("comment %s has no %s annotation", id, KeyIntent)
			continue
		}
		v := decodeValue(t, a)
		probs, _ := v["probabilities"].(map[string]any)
		delete(v, "probabilities")
		if !reflect.DeepEqual(v, want) || len(probs) != len(Intents) || a.Source != Source {
			t.Errorf("comment %s: value %v (%d probabilities), source %q, want %v", id, v, len(probs), a.Source, want)
		}
	}
	if _, ok := h.daemon.annotation(keyAPI, "other-thread", KeyIntent); ok {
		t.Error("comment in someone else's thread was classified")
	}

	a, ok := h.daemon.annotation(keyAPI, "", KeyAttention)
	if !ok {
		t.Fatalf("PR has no %s annotation", KeyAttention)
	}
	got := decodeValue(t, a)
	want := map[string]any{
		derive.AnnotationScore:     float64(100),
		derive.AnnotationAttention: []any{"question for you", "pushback on your comment", "asks for re-review"},
		"badge":                    "question, pushback, re-review",
		"level":                    "warn",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", KeyAttention, got, want)
	}

	logs := h.logs.String()
	for _, s := range []string{`"msg":"jev request"`, `"msg":"jev response"`, `"pr":"acme/api#1"`, `"comment_ids":["reply","mention","top"]`} {
		if !strings.Contains(logs, s) {
			t.Errorf("logs lack %s", s)
		}
	}
	if strings.Contains(logs, "test-key") {
		t.Error("logs contain the API key")
	}
}

func TestIntentContributions(t *testing.T) {
	tests := []struct {
		name        string
		threshold   float64
		coreReasons []string
		answer      answer
		wantIntent  string
		wantPR      map[string]any // nil: no PR-level annotation
	}{
		{
			name:       "re-review request",
			answer:     answer{IntentRereviewRequest, 0.9},
			wantIntent: IntentRereviewRequest,
			wantPR: map[string]any{derive.AnnotationScore: float64(30),
				derive.AnnotationAttention: []any{"asks for re-review"}, "badge": "re-review", "level": "info"},
		},
		{
			name:        "re-review request the core already reports",
			coreReasons: []string{derive.ReasonReviewRequested, derive.ReasonRereview},
			answer:      answer{IntentRereviewRequest, 0.9},
			wantIntent:  IntentRereviewRequest,
		},
		{
			name:       "below threshold",
			answer:     answer{IntentQuestionForMe, 0.55},
			wantIntent: IntentUnclear,
		},
		{
			name:       "lower threshold",
			threshold:  0.5,
			answer:     answer{IntentQuestionForMe, 0.55},
			wantIntent: IntentQuestionForMe,
			wantPR: map[string]any{derive.AnnotationScore: float64(40),
				derive.AnnotationAttention: []any{"question for you"}, "badge": "question", "level": "warn"},
		},
		{
			name:       "addressed",
			answer:     answer{IntentAddressed, 0.9},
			wantIntent: IntentAddressed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := foreignItem(keyAPI,
				inline("mine", me, "T1", 0, "This leaks the connection."),
				inline("c1", "alice", "T1", 5, "Fixed, please have a look."))
			item.Reasons = tt.coreReasons
			h := newHarness(t, item)
			h.jev.setAnswers(map[string]answer{"c1": tt.answer})

			h.start(t, func(c *Config) {
				if tt.threshold != 0 {
					c.Threshold = tt.threshold
				}
			})
			eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })

			a, ok := h.daemon.annotation(keyAPI, "c1", KeyIntent)
			if !ok {
				t.Fatal("comment not annotated")
			}
			v := decodeValue(t, a)
			if v["intent"] != tt.wantIntent || v["probability"] != tt.answer.p {
				t.Errorf("intent annotation %v, want intent %s with probability %v", v, tt.wantIntent, tt.answer.p)
			}
			if _, ok := v[derive.AnnotationAttention]; ok {
				t.Errorf("intent %s suppresses the comment", v["intent"])
			}
			pr, ok := h.daemon.annotation(keyAPI, "", KeyAttention)
			switch {
			case tt.wantPR == nil && ok:
				t.Errorf("unexpected %s %s", KeyAttention, pr.Value)
			case tt.wantPR != nil && !ok:
				t.Errorf("no %s, want %v", KeyAttention, tt.wantPR)
			case ok && !reflect.DeepEqual(decodeValue(t, pr), tt.wantPR):
				t.Errorf("%s = %s, want %v", KeyAttention, pr.Value, tt.wantPR)
			}
		})
	}
}

func TestAttentionAnnotationUpdates(t *testing.T) {
	question := attentionValue{Score: 40, Attention: []string{"question for you"}, Badge: "question", Level: "warn"}
	tests := []struct {
		name      string
		existing  attentionValue
		updatedAt time.Time
		newAnswer answer
		wantWrite bool
		wantValue string // after the run; "" for none
	}{
		{name: "equal and fresh", existing: question, updatedAt: at(6), wantWrite: false,
			wantValue: string(mustMarshal(t, question))},
		{name: "equal but older than the viewer's last activity", existing: question, updatedAt: at(3), wantWrite: true,
			wantValue: string(mustMarshal(t, question))},
		{name: "deleted when nothing is needed", existing: question, updatedAt: at(6), newAnswer: answer{IntentAck, 0.9},
			wantWrite: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The viewer answered at minute 4; alice asked again at 5.
			item := foreignItem(keyAPI,
				issue("mine", me, 4, "I think the timeout is too short."),
				issue("question", "alice", 5, "What timeout would you suggest?"))
			raw := mustMarshal(t, tt.existing)
			item.Annotations = []model.Annotation{{PR: keyAPI, Key: KeyAttention, Value: raw, UpdatedAt: tt.updatedAt}}
			if tt.newAnswer.choice == "" {
				item.Annotations = append(item.Annotations,
					intentAnnotation(keyAPI, "question", intentValue{Intent: IntentQuestionForMe, Probability: 0.9}))
			}
			h := newHarness(t, item)
			if tt.newAnswer.choice != "" {
				h.jev.setAnswers(map[string]answer{"question": tt.newAnswer})
			}

			h.start(t, nil)
			eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })

			var writes []model.Annotation
			for _, a := range h.daemon.annotateCalls() {
				if a.Key == KeyAttention {
					writes = append(writes, a)
				}
			}
			if (len(writes) > 0) != tt.wantWrite {
				t.Errorf("got %d writes of %s, want write %v", len(writes), KeyAttention, tt.wantWrite)
			}
			a, ok := h.daemon.annotation(keyAPI, "", KeyAttention)
			switch {
			case tt.wantValue == "" && ok:
				t.Errorf("%s = %s, want it deleted", KeyAttention, a.Value)
			case tt.wantValue != "" && (!ok || string(a.Value) != tt.wantValue):
				t.Errorf("%s = %s, want %s", KeyAttention, a.Value, tt.wantValue)
			}
		})
	}
}

func TestHandledCommentStopsContributing(t *testing.T) {
	pushback := map[string]any{derive.AnnotationScore: float64(30),
		derive.AnnotationAttention: []any{"pushback on your comment"}, "badge": "pushback", "level": "warn"}
	tests := []struct {
		name          string
		event         string
		change        func(pr *model.PR)
		wantAttention bool
		wantWrites    int // of the PR annotation: the sweep's and the reevaluation's, a deletion included
	}{
		{
			name: "acknowledging reaction", event: model.EventUpdated, wantWrites: 2,
			change: func(pr *model.PR) { pr.Comments[1].ViewerReactions = []string{"THUMBS_UP"} },
		},
		{
			name: "reaction that does not acknowledge", event: model.EventUpdated, wantAttention: true, wantWrites: 1,
			change: func(pr *model.PR) { pr.Comments[1].ViewerReactions = []string{"EYES"} },
		},
		{
			name: "reply in the same thread", event: model.EventCommentAdded, wantWrites: 2,
			change: func(pr *model.PR) {
				pr.Comments = append(pr.Comments, inline("answer", me, "T1", 10, "It is not, see Close()."))
			},
		},
		{
			// The core ignores a PR annotation older than the viewer's last activity, so it is rewritten.
			name: "reply in another thread", event: model.EventCommentAdded, wantAttention: true, wantWrites: 2,
			change: func(pr *model.PR) {
				c := inline("elsewhere", me, "T2", 0, "nit: rename this.")
				c.CreatedAt = time.Now().Add(time.Hour) // the fake daemon stamps annotations with the current time
				pr.Comments = append(pr.Comments, c)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, foreignItem(keyAPI,
				inline("mine", me, "T1", 0, "This leaks the connection."),
				inline("push", "alice", "T1", 5, "I disagree, the caller closes it.")))
			h.jev.setAnswers(map[string]answer{"push": {IntentPushback, 0.9}})
			h.start(t, nil)
			eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })
			a, ok := h.daemon.annotation(keyAPI, "", KeyAttention)
			if !ok || !reflect.DeepEqual(decodeValue(t, a), pushback) {
				t.Fatalf("%s after the sweep = %s, want %v", KeyAttention, a.Value, pushback)
			}

			h.daemon.update(keyAPI, tt.change)
			h.publish(t, tt.event, keyAPI)
			eventually(t, "the reevaluation", func() bool { return h.daemon.getCalls(keyAPI) == 2 && h.settled(t) })

			a, ok = h.daemon.annotation(keyAPI, "", KeyAttention)
			switch {
			case ok != tt.wantAttention:
				t.Errorf("%s present = %v (%s), want %v", KeyAttention, ok, a.Value, tt.wantAttention)
			case ok && !reflect.DeepEqual(decodeValue(t, a), pushback):
				t.Errorf("%s = %s, want %v", KeyAttention, a.Value, pushback)
			}
			var writes int
			for _, c := range h.daemon.annotateCalls() {
				if c.Key == KeyAttention {
					writes++
				}
			}
			if writes != tt.wantWrites {
				t.Errorf("%d writes of %s, want %d", writes, KeyAttention, tt.wantWrites)
			}
			if _, ok := h.daemon.annotation(keyAPI, "push", KeyIntent); !ok {
				t.Errorf("intent annotation of the comment was removed")
			}
			if n := len(h.jev.seen()); n != 1 {
				t.Errorf("got %d Jev requests, want 1: the comment is never reclassified", n)
			}
		})
	}
}

func TestHandledCommentsAreNotSentToJev(t *testing.T) {
	item := foreignItem(keyAPI,
		inline("mine", me, "T1", 0, "This leaks the connection."),
		inline("replied", "alice", "T1", 5, "Why?"),
		inline("answer", me, "T1", 6, "Because the connection stays open."),
		inline("mine2", me, "T2", 1, "Please add a test."),
		inline("acked", "bob", "T2", 7, "Done, I added one."))
	item.PR.Comments[4].ViewerReactions = []string{"HEART"}
	pushback := mustMarshal(t, attentionValue{Score: 30, Attention: []string{"pushback on your comment"}, Badge: "pushback",
		Level: "warn"})
	item.Annotations = []model.Annotation{
		intentAnnotation(keyAPI, "acked", intentValue{Intent: IntentPushback, Probability: 0.9}),
		{PR: keyAPI, Key: KeyAttention, Value: pushback, UpdatedAt: at(8)},
	}
	h := newHarness(t, item)

	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })

	if n := len(h.jev.seen()); n != 0 {
		t.Errorf("got %d Jev requests, want none: both comments are handled", n)
	}
	if a, ok := h.daemon.annotation(keyAPI, "", KeyAttention); ok {
		t.Errorf("%s = %s, want it deleted", KeyAttention, a.Value)
	}
	if _, ok := h.daemon.annotation(keyAPI, "replied", KeyIntent); ok {
		t.Error("handled comment was classified")
	}
	if _, ok := h.daemon.annotation(keyAPI, "acked", KeyIntent); !ok {
		t.Error("intent annotation of the handled comment was removed")
	}
}

func TestConsumerFilterIncludesUpdated(t *testing.T) {
	h := newHarness(t, foreignItem(keyAPI, inline("mine", me, "T1", 0, "This leaks the connection.")))
	earlier := slices.DeleteFunc(FilterSubjects(), func(s string) bool { return strings.HasSuffix(s, ".updated") })
	if _, err := h.js.CreateConsumer(context.Background(), api.StreamName, jetstream.ConsumerConfig{
		Durable: ConsumerName, FilterSubjects: earlier, DeliverPolicy: jetstream.DeliverNewPolicy,
		AckPolicy: jetstream.AckExplicitPolicy, AckWait: ackWait, MaxDeliver: -1,
	}); err != nil {
		t.Fatal(err)
	}

	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })

	if got := h.consumer(t).Config.FilterSubjects; !reflect.DeepEqual(got, FilterSubjects()) {
		t.Fatalf("filter subjects %v, want %v", got, FilterSubjects())
	}
	h.publish(t, model.EventUpdated, keyAPI)
	eventually(t, "the update to be evaluated", func() bool { return h.daemon.getCalls(keyAPI) == 2 && h.settled(t) })
}

func TestReposAllowlist(t *testing.T) {
	h := newHarness(t,
		foreignItem(keyAPI, issue("api-q", "alice", 5, "@me can you check this?")),
		foreignItem(keyWeb, issue("web-q", "alice", 5, "@me can you check this?")))
	h.jev.setAnswers(map[string]answer{"api-q": {IntentQuestionForMe, 0.9}, "web-q": {IntentQuestionForMe, 0.9}})

	h.start(t, func(c *Config) { c.Repos = []string{"ACME"} })
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })
	h.publish(t, model.EventCommentAdded, keyWeb)
	eventually(t, "the event to be acknowledged", func() bool { return h.settled(t) })

	if n := len(h.jev.seen()); n != 1 {
		t.Errorf("got %d Jev requests, want 1 for acme/api only", n)
	}
	if _, ok := h.daemon.annotation(keyAPI, "", KeyAttention); !ok {
		t.Error("allowed PR not annotated")
	}
	if n := h.daemon.getCalls(keyWeb); n != 0 {
		t.Errorf("PR outside the allowlist was fetched %d times", n)
	}
}

func TestEventsTriggerDebouncedEvaluation(t *testing.T) {
	h := newHarness(t, foreignItem(keyAPI, inline("mine", me, "T1", 0, "Use a context here.")))
	h.start(t, func(c *Config) { c.Debounce = 300 * time.Millisecond })
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })
	if n := len(h.jev.seen()); n != 0 {
		t.Fatalf("sweep without candidates sent %d Jev requests", n)
	}

	h.daemon.addComment(keyAPI, inline("reply", "alice", "T1", 5, "Do you mean context.WithTimeout?"))
	h.jev.setAnswers(map[string]answer{"reply": {IntentQuestionForMe, 0.9}})
	h.publish(t, model.EventCommentAdded, keyAPI)
	h.publish(t, model.EventReviewSubmitted, keyAPI)
	h.publish(t, model.EventCommitsPushed, keyAPI)
	// Neither the plugin's own annotations nor unrelated events trigger it.
	h.publish(t, model.EventAnnotated, keyAPI)
	h.publish(t, model.EventChecksFailed, keyAPI)

	eventually(t, "the PR annotation", func() bool {
		_, ok := h.daemon.annotation(keyAPI, "", KeyAttention)
		return ok
	})
	eventually(t, "the events to be acknowledged", func() bool { return h.settled(t) })
	if n := h.daemon.getCalls(keyAPI); n != 2 {
		t.Errorf("PR fetched %d times, want 2: the sweep and one debounced evaluation", n)
	}
	if n := len(h.jev.seen()); n != 1 {
		t.Errorf("got %d Jev requests, want 1", n)
	}
	if info := h.consumer(t); info.Delivered.Stream != 3 {
		t.Errorf("consumer saw %d events, want the 3 trigger events", info.Delivered.Stream)
	}
}

func TestJevFailuresAreRetried(t *testing.T) {
	h := newHarness(t, foreignItem(keyAPI,
		inline("mine", me, "T1", 0, "Use a context here."),
		inline("q1", "alice", "T1", 5, "Which one?")))
	h.jev.setAnswers(map[string]answer{"q1": {IntentQuestionForMe, 0.9}, "q2": {IntentPushback, 0.9}})
	h.jev.failNext(2)

	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })
	if _, ok := h.daemon.annotation(keyAPI, "q1", KeyIntent); !ok {
		t.Fatal("sweep gave up on the PR")
	}

	h.daemon.addComment(keyAPI, inline("q2", "alice", "T1", 6, "I disagree, it is fine."))
	h.jev.failNext(2)
	h.publish(t, model.EventCommentAdded, keyAPI)
	eventually(t, "the redelivered event to be acknowledged", func() bool {
		_, ok := h.daemon.annotation(keyAPI, "q2", KeyIntent)
		return ok && h.settled(t)
	})
	if n := len(h.jev.seen()); n != 6 {
		t.Errorf("got %d Jev requests, want 6: 3 for the sweep and 3 for the event", n)
	}
	if info := h.consumer(t); info.Delivered.Consumer != 3 {
		t.Errorf("event delivered %d times, want 3", info.Delivered.Consumer)
	}
	if !strings.Contains(h.logs.String(), `"msg":"evaluation failed, redelivering"`) {
		t.Error("nak not logged")
	}
}

func TestDurableConsumerResumes(t *testing.T) {
	h := newHarness(t, foreignItem(keyAPI, inline("mine", me, "T1", 0, "Use a context here.")))
	stop := h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })

	info := h.consumer(t)
	if info.Config.Durable != ConsumerName || info.Config.AckPolicy != jetstream.AckExplicitPolicy ||
		!reflect.DeepEqual(info.Config.FilterSubjects, FilterSubjects()) {
		t.Errorf("consumer config %+v", info.Config)
	}
	stop()

	h.daemon.addComment(keyAPI, inline("reply", "alice", "T1", 5, "Which context?"))
	h.jev.setAnswers(map[string]answer{"reply": {IntentQuestionForMe, 0.9}})
	h.publish(t, model.EventCommentAdded, keyAPI)
	h.publish(t, model.EventAnnotated, keyAPI)
	if info := h.consumer(t); info.NumPending != 1 {
		t.Fatalf("consumer has %d pending events while stopped, want the comment only", info.NumPending)
	}

	h.start(t, nil)
	eventually(t, "the missed event to be acknowledged", func() bool { return h.settled(t) })
	if _, ok := h.daemon.annotation(keyAPI, "reply", KeyIntent); !ok {
		t.Error("comment not classified after restart")
	}
}

func TestUndecodableEventIsDropped(t *testing.T) {
	h := newHarness(t)
	h.start(t, nil)
	eventually(t, "the sweep", func() bool { return h.sweeps() == 1 })
	if _, err := h.js.Publish(context.Background(), "devinbox.events.pr.acme.api.1.comment_added", []byte("{")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event to be terminated", func() bool { return h.settled(t) })
	if !strings.Contains(h.logs.String(), `"msg":"dropping undecodable event"`) {
		t.Error("drop not logged")
	}
}

func TestResult(t *testing.T) {
	cands := []model.Comment{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	tests := []struct {
		name    string
		values  map[string]intentValue
		reasons []string
		weights map[string]int
		want    *attentionValue
	}{
		{name: "nothing classified", values: nil, want: nil},
		{
			name:   "each intent counts once",
			values: map[string]intentValue{"a": {Intent: IntentQuestionForMe}, "b": {Intent: IntentQuestionForMe}, "c": {Intent: IntentAck}},
			want:   &attentionValue{Score: 40, Attention: []string{"question for you"}, Badge: "question", Level: "warn"},
		},
		{
			name:   "unclear and suppressing intents add nothing",
			values: map[string]intentValue{"a": {Intent: IntentUnclear}, "b": {Intent: IntentNotForMe}, "c": {Intent: IntentAck}},
			want:   nil,
		},
		{
			name: "intents ordered, comments outside the candidates ignored",
			values: map[string]intentValue{"a": {Intent: IntentRereviewRequest}, "b": {Intent: IntentPushback},
				"x": {Intent: IntentQuestionForMe}},
			want: &attentionValue{Score: 60, Attention: []string{"pushback on your comment", "asks for re-review"},
				Badge: "pushback, re-review", Level: "warn"},
		},
		{
			name:    "core re-review owns the re-review request",
			values:  map[string]intentValue{"a": {Intent: IntentRereviewRequest}, "b": {Intent: IntentAddressed}},
			reasons: []string{derive.ReasonRereview},
			want:    nil,
		},
		{
			name:    "weight 0 disables, other weights add points without a reason",
			values:  map[string]intentValue{"a": {Intent: IntentQuestionForMe}, "b": {Intent: IntentAddressed}, "c": {Intent: IntentAck}},
			weights: map[string]int{IntentQuestionForMe: 0, IntentAddressed: 10, IntentAck: -5},
			want:    &attentionValue{Score: 5, Badge: "addressed, ack", Level: "info"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.weights
			if w == nil {
				w = DefaultWeights
			}
			got := result(cands, tt.values, tt.reasons, w)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFilterSubjects(t *testing.T) {
	want := []string{
		"devinbox.events.pr.*.*.*.comment_added",
		"devinbox.events.pr.*.*.*.review_submitted",
		"devinbox.events.pr.*.*.*.commits_pushed",
		"devinbox.events.pr.*.*.*.review_requested",
		"devinbox.events.pr.*.*.*.discovered",
		"devinbox.events.pr.*.*.*.reopened",
		"devinbox.events.pr.*.*.*.updated",
	}
	if got := FilterSubjects(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
