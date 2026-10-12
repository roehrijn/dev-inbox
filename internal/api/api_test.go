package api

import (
	"testing"

	"github.com/roehrijn/dev-inbox/internal/model"
)

func TestEventSubject(t *testing.T) {
	ev := model.Event{Type: model.EventMerged, PR: model.PRKey{Owner: "Acme-CO", Repo: "foo.github.io", Number: 7}}
	if got, want := EventSubject(ev), "devinbox.events.pr.acme-co.foo%2Egithub%2Eio.7.merged"; got != want {
		t.Fatalf("EventSubject = %q, want %q", got, want)
	}
}
