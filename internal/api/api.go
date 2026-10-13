// Package api defines the NATS contract between the daemon and its clients: subjects,
// request/response types and the CloudEvents envelope of published events.
package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	DefaultNATSURL = "nats://127.0.0.1:14222"

	StreamName = "DEVINBOX_EVENTS"
	// EventsSubjectAll matches every event subject, see EventSubject.
	EventsSubjectAll = "devinbox.events.>"

	SubjectList     = "devinbox.api.inbox.list"
	SubjectGet      = "devinbox.api.pr.get"
	SubjectAnnotate = "devinbox.api.annotate"
	SubjectTrack    = "devinbox.api.pr.track"

	// CloudEventTypePrefix prefixes model event types, e.g. "devinbox.pr.merged".
	CloudEventTypePrefix = "devinbox."
)

// EventSubject returns the NATS subject of an event:
// devinbox.events.pr.<owner>.<repo>.<number>.<event>, e.g. devinbox.events.pr.acme-co.frontend.42.merged.
// Owner and repo are lowercased and "." is escaped as "%2E" so each stays a single subject token.
func EventSubject(ev model.Event) string {
	return fmt.Sprintf("devinbox.events.pr.%s.%s.%d.%s",
		subjectToken(ev.PR.Owner), subjectToken(ev.PR.Repo), ev.PR.Number, strings.TrimPrefix(ev.Type, "pr."))
}

func subjectToken(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), ".", "%2E")
}

// CloudEvent is the CloudEvents 1.0 structured-mode JSON envelope of every published event.
type CloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"` // model.EventData
}

const CloudEventContentType = "application/cloudevents+json"

func NewCloudEvent(ev model.Event) CloudEvent {
	return CloudEvent{
		SpecVersion:     "1.0",
		ID:              ev.ID,
		Source:          fmt.Sprintf("https://github.com/%s/%s", ev.PR.Owner, ev.PR.Repo),
		Type:            CloudEventTypePrefix + ev.Type,
		Subject:         fmt.Sprintf("pull/%d", ev.PR.Number),
		Time:            ev.Time,
		DataContentType: "application/json",
		Data:            ev.Data,
	}
}

// InboxItem is a tracked PR with its classification and annotations.
type InboxItem struct {
	PR model.PR `json:"pr"`
	model.Classification
	Annotations []model.Annotation `json:"annotations,omitempty"`
}

type ListRequest struct{}

// ListResponse holds all open tracked PRs, without comments and with PR-level annotations only.
type ListResponse struct {
	Viewer model.Viewer `json:"viewer"`
	Items  []InboxItem  `json:"items"`
	Error  string       `json:"error,omitempty"`
}

type GetRequest struct {
	PR model.PRKey `json:"pr"`
}

// GetResponse holds one PR including comments and all PR- and comment-level annotations.
type GetResponse struct {
	Item  *InboxItem `json:"item,omitempty"`
	Error string     `json:"error,omitempty"`
}

// AnnotateRequest sets an annotation; a JSON null Value deletes it. UpdatedAt is set by the daemon.
type AnnotateRequest struct {
	Annotation model.Annotation `json:"annotation"`
}

type AnnotateResponse struct {
	Error string `json:"error,omitempty"`
}

// TrackRequest asks the daemon to ingest a PR it does not know yet. The daemon queues it and answers at
// once; ingestion emits pr.discovered. A PR the GitHub token cannot read is dropped by the daemon.
type TrackRequest struct {
	PR model.PRKey `json:"pr"`
}

// TrackResponse reports Known if the PR was already in the store, in which case nothing was queued.
type TrackResponse struct {
	Known bool   `json:"known,omitempty"`
	Error string `json:"error,omitempty"`
}
