package pluginkit

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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
