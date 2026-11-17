package pluginkit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

func TestBackoff(t *testing.T) {
	t.Parallel()
	for attempt, want := range map[int]time.Duration{0: 5, 1: 5, 2: 10, 3: 20, 4: 40, 5: 50, 30: 50} {
		if got := Backoff(5, 50, attempt); got != want {
			t.Errorf("Backoff(5, 50, %d) = %d, want %d", attempt, got, want)
		}
	}
}

func TestFilterSubjects(t *testing.T) {
	t.Parallel()
	got := FilterSubjects([]string{model.EventDiscovered, model.EventUpdated})
	want := []string{"devinbox.events.pr.*.*.*.discovered", "devinbox.events.pr.*.*.*.updated"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilterSubjects = %q, want %q", got, want)
	}
}

func TestEventKey(t *testing.T) {
	t.Parallel()
	key := model.PRKey{Owner: "acme", Repo: "api", Number: 7}
	ev, err := model.NewEvent(model.EventUpdated, key, model.EventData{PR: model.PR{Key: key}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(api.NewCloudEvent(ev))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := EventKey(data); err != nil || got != key {
		t.Errorf("EventKey = %v, %v, want %v", got, err, key)
	}

	for name, data := range map[string]string{
		"not json":  `{`,
		"no PR key": `{"id":"x","data":{"pr":{"key":{"owner":"acme"}}}}`,
	} {
		if _, err := EventKey([]byte(data)); err == nil {
			t.Errorf("%s: EventKey succeeded", name)
		}
	}
}

func TestConfigPath(t *testing.T) {
	xdg := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for name, tt := range map[string]struct{ xdg, want string }{
		"absolute XDG_CONFIG_HOME": {xdg, filepath.Join(xdg, "dev-inbox", "jev.yaml")},
		"unset":                    {"", filepath.Join(home, ".config", "dev-inbox", "jev.yaml")},
		"relative":                 {"rel/config", filepath.Join(home, ".config", "dev-inbox", "jev.yaml")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			if got := ConfigPath("jev"); got != tt.want {
				t.Errorf("ConfigPath = %q, want %q", got, tt.want)
			}
		})
	}
}

// A stopped consumer leaves a pull request on the server until the server processes the unsubscribe.
// Close must not return before that, or the next event goes to the stopped plugin and stays
// unacknowledged until AckWait.
func TestCloseReleasesPullRequests(t *testing.T) {
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: server.RANDOM_PORT, JetStream: true,
		StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
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
	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: api.StreamName, Subjects: []string{api.EventsSubjectAll}, Storage: jetstream.MemoryStorage,
	}); err != nil {
		t.Fatal(err)
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, api.StreamName, jetstream.ConsumerConfig{
		Durable: "test", AckPolicy: jetstream.AckExplicitPolicy, AckWait: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)

	// The race is narrow, so repeat it.
	for i := range 50 {
		c, err := Connect(ns.ClientURL(), "test", log)
		if err != nil {
			t.Fatal(err)
		}
		pjs, err := jetstream.New(c.Conn())
		if err != nil {
			t.Fatal(err)
		}
		pcons, err := pjs.Consumer(ctx, api.StreamName, "test")
		if err != nil {
			t.Fatal(err)
		}
		cc, err := pcons.Consume(func(msg jetstream.Msg) { _ = msg.Ack() })
		if err != nil {
			t.Fatal(err)
		}
		cc.Stop()
		<-cc.Closed()
		c.Close()

		if _, err := js.Publish(ctx, fmt.Sprintf("devinbox.events.pr.acme.api.%d.updated", i+1), []byte("{}")); err != nil {
			t.Fatal(err)
		}
		info, err := cons.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.NumPending != 1 || info.NumAckPending != 0 {
			t.Fatalf("round %d: %d pending, %d unacknowledged after Close, want 1 pending", i, info.NumPending,
				info.NumAckPending)
		}
		msgs, err := cons.Fetch(1)
		if err != nil {
			t.Fatal(err)
		}
		for msg := range msgs.Messages() {
			if err := msg.Ack(); err != nil {
				t.Fatal(err)
			}
		}
		if err := msgs.Error(); err != nil {
			t.Fatal(err)
		}
	}
}
