package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/model"
)

func (d *daemon) serveAPI() error {
	for subject, fn := range map[string]func(context.Context, []byte) (any, error){
		api.SubjectList:     d.handleList,
		api.SubjectGet:      d.handleGet,
		api.SubjectAnnotate: d.handleAnnotate,
		api.SubjectTrack:    d.handleTrack,
	} {
		if err := d.bus.Serve(subject, fn); err != nil {
			return fmt.Errorf("serve %s: %w", subject, err)
		}
	}
	return nil
}

func (d *daemon) handleList(ctx context.Context, _ []byte) (any, error) {
	prs, err := d.store.ListPRs(ctx, true)
	if err != nil {
		return nil, err
	}
	annotations, err := d.store.AnnotationsByPR(ctx)
	if err != nil {
		return nil, err
	}
	in := d.inputs(nil)
	items := make([]api.InboxItem, 0, len(prs))
	for _, pr := range prs {
		in.Annotations = annotations[pr.Key]
		// Evaluate needs the comments to detect new ones.
		c := derive.Evaluate(pr, in)
		pr.Comments = nil
		items = append(items, api.InboxItem{PR: pr, Classification: c, Annotations: prLevelAnnotations(in.Annotations)})
	}
	return api.ListResponse{Viewer: in.Viewer, Items: items}, nil
}

// prLevelAnnotations returns the PR-level annotations (CommentID empty) of annotations, or nil if there are none.
func prLevelAnnotations(annotations []model.Annotation) []model.Annotation {
	var out []model.Annotation
	for _, a := range annotations {
		if a.CommentID == "" {
			out = append(out, a)
		}
	}
	return out
}

func (d *daemon) handleGet(ctx context.Context, req []byte) (any, error) {
	var r api.GetRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	pr, err := d.knownPR(ctx, r.PR)
	if err != nil {
		return nil, err
	}
	annotations, err := d.store.Annotations(ctx, pr.Key)
	if err != nil {
		return nil, err
	}
	item := api.InboxItem{PR: pr, Classification: derive.Evaluate(pr, d.inputs(annotations)), Annotations: annotations}
	return api.GetResponse{Item: &item}, nil
}

func (d *daemon) handleAnnotate(ctx context.Context, req []byte) (any, error) {
	var r api.AnnotateRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	a := r.Annotation
	if a.Key == "" {
		return nil, errors.New("annotation key is required")
	}
	pr, err := d.knownPR(ctx, a.PR)
	if err != nil {
		return nil, err
	}
	a.UpdatedAt = time.Now().UTC()
	existing, err := d.store.Annotations(ctx, pr.Key)
	if err != nil {
		return nil, err
	}
	ev, err := model.NewEvent(model.EventAnnotated, pr.Key, model.EventData{
		PR:             pr,
		Classification: derive.Evaluate(pr, d.inputs(withAnnotation(existing, a))),
		Change:         a,
	})
	if err != nil {
		return nil, err
	}
	if err := d.store.SetAnnotation(ctx, a, ev); err != nil {
		return nil, err
	}
	d.bus.Kick()
	return api.AnnotateResponse{}, nil
}

// withAnnotation returns the annotations after storing a, which replaces its predecessor or, with an
// empty or null value, deletes it.
func withAnnotation(annotations []model.Annotation, a model.Annotation) []model.Annotation {
	out := slices.DeleteFunc(slices.Clone(annotations), func(o model.Annotation) bool {
		return o.CommentID == a.CommentID && o.Key == a.Key
	})
	if v := bytes.TrimSpace(a.Value); len(v) > 0 && string(v) != "null" {
		out = append(out, a)
	}
	return out
}

// handleTrack queues a PR the store does not know yet. Ingestion emits pr.discovered as for any new PR.
func (d *daemon) handleTrack(ctx context.Context, req []byte) (any, error) {
	var r api.TrackRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	if r.PR.Owner == "" || r.PR.Repo == "" || r.PR.Number <= 0 {
		return nil, fmt.Errorf("invalid PR %s", r.PR)
	}
	_, known, err := d.store.GetPR(ctx, r.PR)
	if err != nil {
		return nil, err
	}
	if !known {
		d.queue.push(item{key: r.PR})
	}
	return api.TrackResponse{Known: known}, nil
}

func (d *daemon) knownPR(ctx context.Context, key model.PRKey) (model.PR, error) {
	pr, ok, err := d.store.GetPR(ctx, key)
	if err != nil {
		return model.PR{}, err
	}
	if !ok {
		return model.PR{}, fmt.Errorf("unknown PR %s", key)
	}
	return pr, nil
}
