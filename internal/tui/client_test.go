package tui

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

func startNATS(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: server.RANDOM_PORT, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// serveFixtures answers the daemon API like the daemon would, from fixtureList and fixtureDetail.
func serveFixtures(t *testing.T, url string, list api.ListResponse) {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	reply := func(m *nats.Msg, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Error(err)
			return
		}
		if err := m.Respond(data); err != nil {
			t.Error(err)
		}
	}
	if _, err := nc.Subscribe(api.SubjectList, func(m *nats.Msg) {
		var req api.ListRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			reply(m, api.ListResponse{Error: "bad request: " + err.Error()})
			return
		}
		reply(m, list)
	}); err != nil {
		t.Fatal(err)
	}
	detail := fixtureDetail()
	if _, err := nc.Subscribe(api.SubjectGet, func(m *nats.Msg) {
		var req api.GetRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			reply(m, api.GetResponse{Error: "bad request: " + err.Error()})
			return
		}
		if req.PR != detail.PR.Key {
			reply(m, api.GetResponse{Error: "pr " + req.PR.String() + " is not tracked"})
			return
		}
		reply(m, api.GetResponse{Item: &detail})
	}); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

func dialTest(t *testing.T, url string) (*client, <-chan bool) {
	t.Helper()
	states := make(chan bool, 16)
	c, err := dial(url, func(connected bool) { states <- connected })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, states
}

func waitState(t *testing.T, states <-chan bool, want bool) {
	t.Helper()
	select {
	case got := <-states:
		if got != want {
			t.Fatalf("connection state %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no connection state %v reported", want)
	}
}

func TestClientAgainstEmbeddedNATS(t *testing.T) {
	s := startNATS(t)
	list := fixtureList()
	serveFixtures(t, s.ClientURL(), list)
	c, states := dialTest(t, s.ClientURL())
	waitState(t, states, true)
	ctx := context.Background()

	gotList, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip(t, list), gotList) {
		t.Fatalf("List decoded\n%+v\nwant\n%+v", gotList, list)
	}

	want := fixtureDetail()
	got, err := c.Get(ctx, want.PR.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Item == nil || !reflect.DeepEqual(roundTrip(t, want), *got.Item) {
		t.Fatalf("Get decoded\n%+v\nwant\n%+v", got.Item, want)
	}
	if len(got.Item.PR.Comments) != 2 || got.Item.PR.Comments[1].Path != "internal/x.go" {
		t.Fatalf("comments not decoded: %+v", got.Item.PR.Comments)
	}

	_, err = c.Get(ctx, prKey("nope", "nope", 1))
	if err == nil || !strings.Contains(err.Error(), "pr nope/nope#1 is not tracked") {
		t.Fatalf("Get error = %v, want the daemon's error", err)
	}
}

func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClientSurfacesListError(t *testing.T) {
	s := startNATS(t)
	serveFixtures(t, s.ClientURL(), api.ListResponse{Error: "store: database is locked"})
	c, states := dialTest(t, s.ClientURL())
	waitState(t, states, true)
	if _, err := c.List(context.Background()); err == nil || err.Error() != "store: database is locked" {
		t.Fatalf("List error = %v", err)
	}
}

func TestClientNoResponders(t *testing.T) {
	s := startNATS(t)
	c, states := dialTest(t, s.ClientURL())
	waitState(t, states, true)
	_, err := c.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dev-inbox serve") {
		t.Fatalf("List error = %v, want a hint to start the daemon", err)
	}
}

func TestClientEvents(t *testing.T) {
	s := startNATS(t)
	c, states := dialTest(t, s.ClientURL())
	waitState(t, states, true)
	keys := make(chan *model.PRKey, 4)
	if _, err := c.subscribeEvents(func(k *model.PRKey) { keys <- k }); err != nil {
		t.Fatal(err)
	}
	if err := c.nc.Flush(); err != nil {
		t.Fatal(err)
	}

	pub, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	k := model.PRKey{Owner: "Acme-CO", Repo: "foo.github.io", Number: 7}
	ev, err := model.NewEvent(model.EventCommentAdded, k, model.EventData{PR: model.PR{Key: k}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(api.NewCloudEvent(ev))
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(api.EventSubject(ev), data); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish("devinbox.events.pr.x.y.1.updated", []byte("not json")); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-keys:
		if got == nil || *got != k {
			t.Fatalf("event key = %v, want %v", got, k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not received")
	}
	select {
	case got := <-keys:
		if got != nil {
			t.Fatalf("undecodable event key = %v, want nil", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("undecodable event not received")
	}
}

func TestClientReconnects(t *testing.T) {
	s := startNATS(t)
	port := s.Addr().(*net.TCPAddr).Port
	c, states := dialTest(t, s.ClientURL())
	waitState(t, states, true)

	s.Shutdown()
	waitState(t, states, false)
	if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "daemon not reachable") {
		t.Fatalf("List while disconnected = %v", err)
	}

	s2, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s2.Start()
	t.Cleanup(s2.Shutdown)
	waitState(t, states, true)
}

func TestClientDaemonDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "nats://" + l.Addr().String()
	_ = l.Close()

	c, states := dialTest(t, url)
	start := time.Now()
	_, err = c.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "daemon not reachable at "+url) {
		t.Fatalf("List error = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("List did not fail fast while disconnected")
	}
	select {
	case got := <-states:
		t.Fatalf("unexpected connection state %v", got)
	default:
	}
}

// TestAppAgainstEmbeddedNATS drives the model with the real client end to end.
func TestAppAgainstEmbeddedNATS(t *testing.T) {
	s := startNATS(t)
	serveFixtures(t, s.ClientURL(), fixtureList())
	c, states := dialTest(t, s.ClientURL())
	waitState(t, states, true)

	a := newTestApp(c)
	update(t, a, tea.WindowSizeMsg{Width: 140, Height: 70})
	drain(t, a, update(t, a, connMsg{connected: true}))
	drain(t, a, update(t, a, keyPress("d")))

	assertContains(t, screen(a), "▾ Needs action 1", "@roehrijn", "5 PRs", "Comments 2", "Please fix this.")
}
