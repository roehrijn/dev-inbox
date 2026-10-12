package tui

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// addService registers a NATS micro service called name on its own connection and returns a
// function that stops it.
func addService(t *testing.T, url, name string) (stop func()) {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	svc, err := micro.AddService(nc, micro.Config{Name: name, Version: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	// The flushes make the server see the subscriptions, or their removal, before the next ping.
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = svc.Stop()
		_ = nc.Flush()
	}
}

func connectedClient(t *testing.T, url string) *client {
	t.Helper()
	c, states := dialTest(t, url)
	waitState(t, states, true)
	return c
}

func TestClientPlugins(t *testing.T) {
	s := startNATS(t)
	c := connectedClient(t, s.ClientURL())
	ctx := context.Background()

	if got := c.Plugins(ctx); len(got) != 0 {
		t.Fatalf("Plugins without services = %v", got)
	}

	stopJev := addService(t, s.ClientURL(), "devinbox-jev")
	if got, want := c.Plugins(ctx), []string{"devinbox-jev"}; !slices.Equal(got, want) {
		t.Fatalf("Plugins with one service = %v, want %v", got, want)
	}

	addService(t, s.ClientURL(), "devinbox-attn")
	if got, want := c.Plugins(ctx), []string{"devinbox-attn", "devinbox-jev"}; !slices.Equal(got, want) {
		t.Fatalf("Plugins with two services = %v, want %v", got, want)
	}

	addService(t, s.ClientURL(), "devinbox-jev")
	if got, want := c.Plugins(ctx), []string{"devinbox-attn", "devinbox-jev"}; !slices.Equal(got, want) {
		t.Fatalf("Plugins with two instances of one service = %v, want %v", got, want)
	}

	stopJev()
	if got, want := c.Plugins(ctx), []string{"devinbox-attn", "devinbox-jev"}; !slices.Equal(got, want) {
		t.Fatalf("Plugins while a second instance of a stopped service runs = %v, want %v", got, want)
	}
}

func TestClientPluginsForgetsStoppedServices(t *testing.T) {
	s := startNATS(t)
	c := connectedClient(t, s.ClientURL())
	stop := addService(t, s.ClientURL(), "devinbox-jev")
	if got := c.Plugins(context.Background()); len(got) != 1 {
		t.Fatalf("Plugins = %v, want the service", got)
	}
	stop()
	if got := c.Plugins(context.Background()); len(got) != 0 {
		t.Fatalf("Plugins after stopping the service = %v", got)
	}
}

func TestClientPluginsIgnoresOtherReplies(t *testing.T) {
	s := startNATS(t)
	c := connectedClient(t, s.ClientURL())
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	for _, reply := range []string{"not json", `{"type":"io.nats.micro.v1.ping_response"}`, `{"name":"\u001b[31mevil"}`, `{"name":"two words"}`} {
		if _, err := nc.Subscribe(pingSubject, func(m *nats.Msg) { _ = m.Respond([]byte(reply)) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	addService(t, s.ClientURL(), "devinbox-jev")
	if got, want := c.Plugins(context.Background()), []string{"devinbox-jev"}; !slices.Equal(got, want) {
		t.Fatalf("Plugins = %v, want %v", got, want)
	}
}

func TestClientPluginsCollectsRepliesForTheWindow(t *testing.T) {
	s := startNATS(t)
	c := connectedClient(t, s.ClientURL())
	addService(t, s.ClientURL(), "devinbox-jev")

	start := time.Now()
	c.Plugins(context.Background())
	if d := time.Since(start); d < pluginPingWindow || d > 2*time.Second {
		t.Errorf("Plugins took %v, want about %v", d, pluginPingWindow)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	c.Plugins(ctx)
	if d := time.Since(start); d > pluginPingWindow/2 {
		t.Errorf("Plugins with a canceled context took %v", d)
	}
}

func TestClientPluginsWithoutServicesReturnsEarly(t *testing.T) {
	s := startNATS(t)
	c := connectedClient(t, s.ClientURL())
	start := time.Now()
	if got := c.Plugins(context.Background()); len(got) != 0 {
		t.Errorf("Plugins = %v, want none", got)
	}
	if d := time.Since(start); d > pluginPingWindow {
		t.Errorf("Plugins took %v without any service, want the server's no-responders answer to end the wait", d)
	}
}

func TestClientPluginsWhileDisconnected(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "nats://" + l.Addr().String()
	_ = l.Close()

	c, _ := dialTest(t, url)
	start := time.Now()
	if got := c.Plugins(context.Background()); got != nil {
		t.Errorf("Plugins = %v, want none", got)
	}
	if d := time.Since(start); d > pluginPingWindow/2 {
		t.Errorf("Plugins took %v while disconnected, want to fail fast", d)
	}
}

// statusLine is the last line of the screen.
func statusLine(a *app) string {
	lines := strings.Split(screen(a), "\n")
	return lines[len(lines)-1]
}

func TestStatusBarListsPlugins(t *testing.T) {
	tests := []struct {
		name    string
		plugins []string
		want    []string
	}{
		{"none", nil, nil},
		{"one", []string{"devinbox-jev"}, []string{"devinbox-jev ●"}},
		{"two", []string{"devinbox-attn", "devinbox-jev"}, []string{"devinbox-attn ●", "devinbox-jev ●"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fb := &fakeBackend{list: fixtureList(), plugins: tt.plugins}
			a := newTestApp(fb)
			update(t, a, tea.WindowSizeMsg{Width: 140, Height: 30})
			drain(t, a, update(t, a, connMsg{connected: true}))

			status := statusLine(a)
			assertFits(t, screen(a), 140, 30)
			assertContains(t, status, "connected", "5 PRs")
			if len(tt.want) == 0 {
				if strings.Contains(status, "devinbox") || strings.Count(status, "●") != 1 {
					t.Errorf("status bar mentions plugins although none answer: %q", status)
				}
				return
			}
			assertOrder(t, status, append([]string{"refreshed"}, tt.want...)...)
			if !strings.Contains(a.View().Content, a.st.success.Render("●")) {
				t.Error("plugin dot is not rendered in the success color")
			}
		})
	}
}

func TestStatusBarDropsPluginsFirstWhenNarrow(t *testing.T) {
	fb := &fakeBackend{list: fixtureList(), plugins: []string{"devinbox-jev"}}
	a := newTestApp(fb)
	update(t, a, tea.WindowSizeMsg{Width: 50, Height: 30})
	drain(t, a, update(t, a, connMsg{connected: true}))
	status := statusLine(a)
	assertFits(t, screen(a), 50, 30)
	assertContains(t, status, "connected")
	if strings.Contains(status, "devinbox-jev") {
		t.Errorf("plugins kept over PR count and viewer in a narrow status bar: %q", status)
	}
}

func TestPluginsFollowTickAndConnection(t *testing.T) {
	fb := &fakeBackend{list: fixtureList(), plugins: []string{"devinbox-jev"}}
	a := newTestApp(fb)
	update(t, a, tea.WindowSizeMsg{Width: 140, Height: 30})
	if n := fb.pingCount(); n != 0 {
		t.Fatalf("pinged %d times before the connection came up", n)
	}
	drain(t, a, update(t, a, connMsg{connected: true}))
	if n := fb.pingCount(); n != 1 {
		t.Fatalf("pinged %d times on connect, want 1", n)
	}
	assertContains(t, statusLine(a), "devinbox-jev ●")

	// Services come and go between ticks.
	fb.mu.Lock()
	fb.plugins = []string{"devinbox-attn", "devinbox-jev"}
	fb.mu.Unlock()
	drain(t, a, update(t, a, tickMsg{}))
	if n := fb.pingCount(); n != 2 {
		t.Fatalf("pinged %d times after a tick, want 2", n)
	}
	assertContains(t, statusLine(a), "devinbox-attn ●", "devinbox-jev ●")

	fb.mu.Lock()
	fb.plugins = nil
	fb.mu.Unlock()
	drain(t, a, update(t, a, tickMsg{}))
	if strings.Contains(statusLine(a), "devinbox") {
		t.Errorf("plugins remain after none answered: %q", statusLine(a))
	}

	// Without a connection there is nothing to ask, and nothing to show.
	fb.mu.Lock()
	fb.plugins = []string{"devinbox-jev"}
	fb.mu.Unlock()
	drain(t, a, update(t, a, tickMsg{}))
	assertContains(t, statusLine(a), "devinbox-jev ●")
	drain(t, a, update(t, a, connMsg{connected: false}))
	pings := fb.pingCount()
	drain(t, a, update(t, a, tickMsg{}))
	if fb.pingCount() != pings {
		t.Error("pinged while disconnected")
	}
	if strings.Contains(statusLine(a), "devinbox-jev") {
		t.Errorf("plugins shown while disconnected: %q", statusLine(a))
	}
}

// TestPluginPingRunsInACommand checks that Update only schedules the ping; waiting for the
// answers happens off the UI goroutine.
func TestPluginPingRunsInACommand(t *testing.T) {
	fb := &fakeBackend{list: fixtureList(), plugins: []string{"devinbox-jev"}}
	a := newTestApp(fb)
	update(t, a, tea.WindowSizeMsg{Width: 140, Height: 30})

	connCmd := update(t, a, connMsg{connected: true})
	tickCmd := update(t, a, tickMsg{})
	if n := fb.pingCount(); n != 0 {
		t.Fatalf("Update pinged %d times itself", n)
	}
	drain(t, a, tea.Batch(connCmd, tickCmd))
	if n := fb.pingCount(); n != 2 {
		t.Errorf("commands pinged %d times, want 2", n)
	}
}

func TestAppShowsPluginsFromEmbeddedNATS(t *testing.T) {
	s := startNATS(t)
	serveFixtures(t, s.ClientURL(), fixtureList())
	addService(t, s.ClientURL(), "devinbox-jev")
	addService(t, s.ClientURL(), "devinbox-attn")
	c := connectedClient(t, s.ClientURL())

	a := newTestApp(c)
	update(t, a, tea.WindowSizeMsg{Width: 140, Height: 30})
	drain(t, a, update(t, a, connMsg{connected: true}))

	assertContains(t, statusLine(a), "devinbox-attn ●", "devinbox-jev ●", "5 PRs")
}
