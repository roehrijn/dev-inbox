// Package jevplugin is the dev-inbox-jev plugin: it classifies new comments on others' PRs with
// TypeSafe Jev and annotates PRs that need the viewer, using only the public NATS API of the daemon.
// See docs/proposals/attention.md.
package jevplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nats.go/micro"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/jev"
	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/pluginkit"
)

const (
	// ServiceName is the NATS micro service the plugin registers, which the TUI lists as live.
	ServiceName    = "devinbox-jev"
	serviceVersion = "0.1.0"
	// ConsumerName is the durable JetStream consumer of the plugin.
	ConsumerName = "jev"
	// Source marks the annotations the plugin writes.
	Source = "dev-inbox-jev"

	DefaultThreshold = 0.6
	DefaultDebounce  = 2 * time.Second

	// ackWait covers the debounce, waiting for other evaluations and a Jev call with retries;
	// evaluation also signals progress when it starts.
	ackWait    = 2 * time.Minute
	maxDeliver = 10
)

// DefaultWeights are the points each intent adds to the PR score.
var DefaultWeights = map[string]int{
	IntentQuestionForMe:   40,
	IntentPushback:        30,
	IntentRereviewRequest: 30,
	IntentAddressed:       0,
	IntentAck:             0,
	IntentNotForMe:        0,
}

// triggers are the event types that can change which comments are candidates; pr.updated covers the
// viewer's reactions, which acknowledge comments. pr.annotated is not among them, so the plugin's own
// annotations never trigger it.
var triggers = []string{
	model.EventCommentAdded,
	model.EventReviewSubmitted,
	model.EventCommitsPushed,
	model.EventReviewRequested,
	model.EventDiscovered,
	model.EventReopened,
	model.EventUpdated,
}

// FilterSubjects are the subjects of the trigger events, see api.EventSubject.
func FilterSubjects() []string { return pluginkit.FilterSubjects(triggers) }

type Config struct {
	NATSURL string
	// Threshold is the probability below which a classification is unclear.
	Threshold float64
	// Weights are the points per intent, keyed by the Intent* constants.
	Weights map[string]int
	// Repos limits which PRs are sent to Jev: entries are "org" or "org/repo"; empty allows all.
	Repos    []string
	Debounce time.Duration
	Jev      *jev.Client
	Logger   *slog.Logger
}

type plugin struct {
	cfg    Config
	log    *slog.Logger
	c      *pluginkit.Client
	viewer model.Viewer

	// evalMu serializes evaluations, so one PR is never annotated concurrently.
	evalMu sync.Mutex

	mu      sync.Mutex
	pending map[model.PRKey]*batch // guarded by mu
	stopped bool                   // guarded by mu
	running sync.WaitGroup

	retryBase time.Duration
	retryMax  time.Duration
}

// batch holds the unacknowledged events of one PR until its debounce timer fires.
type batch struct {
	msgs  []jetstream.Msg
	timer *time.Timer
	due   time.Time
}

// Run connects to the daemon and classifies until ctx ends. It returns nil on shutdown.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Jev == nil {
		return errors.New("jevplugin: Jev client is required")
	}
	return newPlugin(cfg).run(ctx)
}

func newPlugin(cfg Config) *plugin {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &plugin{
		cfg:       cfg,
		log:       cfg.Logger,
		pending:   make(map[model.PRKey]*batch),
		retryBase: 5 * time.Second,
		retryMax:  5 * time.Minute,
	}
}

func (p *plugin) run(ctx context.Context) error {
	c, err := pluginkit.Connect(p.cfg.NATSURL, Source, p.log)
	if err != nil {
		return err
	}
	p.c = c
	defer c.Close()

	svc, err := micro.AddService(c.Conn(), micro.Config{
		Name:        ServiceName,
		Version:     serviceVersion,
		Description: "Classifies comments on others' PRs with TypeSafe Jev",
	})
	if err != nil {
		return fmt.Errorf("register service %s: %w", ServiceName, err)
	}
	defer func() { _ = svc.Stop() }()

	list, ok := c.AwaitDaemon(ctx)
	if !ok {
		return nil
	}
	p.viewer = list.Viewer
	p.log.Info("connected to daemon", "viewer", p.viewer.Login, "prs", len(list.Items))

	cc, err := p.consume(ctx)
	if err != nil {
		return err
	}
	var sweep sync.WaitGroup
	sweep.Go(func() { p.sweep(ctx, list.Items) })

	<-ctx.Done()
	cc.Stop()
	<-cc.Closed()
	p.stop()
	sweep.Wait()
	p.running.Wait()
	p.log.Info("stopped")
	return nil
}

func (p *plugin) consume(ctx context.Context) (jetstream.ConsumeContext, error) {
	js, err := jetstream.New(p.c.Conn())
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, pluginkit.RequestTimeout)
	defer cancel()
	// Update, not just create, so a changed FilterSubjects reaches the consumer of an earlier run.
	cons, err := js.CreateOrUpdateConsumer(cctx, api.StreamName, jetstream.ConsumerConfig{
		Durable:        ConsumerName,
		FilterSubjects: FilterSubjects(),
		DeliverPolicy:  jetstream.DeliverNewPolicy,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        ackWait,
		MaxDeliver:     -1,
	})
	if err != nil {
		return nil, fmt.Errorf("create consumer %s: %w", ConsumerName, err)
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) { p.receive(ctx, msg) },
		jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
			if ctx.Err() == nil {
				p.log.Warn("consume events", "err", err)
			}
		}))
	if err != nil {
		return nil, fmt.Errorf("consume %s: %w", ConsumerName, err)
	}
	return cc, nil
}

// receive adds an event to its PR's batch and (re)starts the PR's debounce timer.
func (p *plugin) receive(ctx context.Context, msg jetstream.Msg) {
	key, err := pluginkit.EventKey(msg.Data())
	if err != nil {
		p.log.Error("dropping undecodable event", "subject", msg.Subject(), "err", err)
		if err := msg.TermWithReason("undecodable event"); err != nil {
			p.log.Warn("terminate event", "err", err)
		}
		return
	}
	if !p.allowed(key) {
		if err := msg.Ack(); err != nil {
			p.log.Warn("ack event", "pr", key.String(), "err", err)
		}
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		_ = msg.Nak()
		return
	}
	// due is set before the timer starts, so the timer never fires before due.
	due := time.Now().Add(p.cfg.Debounce)
	b := p.pending[key]
	if b == nil {
		b = &batch{due: due}
		p.pending[key] = b
		b.timer = time.AfterFunc(p.cfg.Debounce, func() { p.fire(ctx, key) })
	} else {
		b.due = due
		b.timer.Reset(p.cfg.Debounce)
	}
	b.msgs = append(b.msgs, msg)
}

// fire evaluates key once its batch is due and acknowledges the batch's events on success.
func (p *plugin) fire(ctx context.Context, key model.PRKey) {
	p.mu.Lock()
	b := p.pending[key]
	if p.stopped || b == nil {
		p.mu.Unlock()
		return
	}
	// The timer was reset after it fired; this call is stale.
	if wait := time.Until(b.due); wait > 0 {
		b.timer.Reset(wait)
		p.mu.Unlock()
		return
	}
	delete(p.pending, key)
	p.running.Add(1)
	p.mu.Unlock()
	defer p.running.Done()

	for _, m := range b.msgs {
		_ = m.InProgress()
	}
	err := p.evaluate(ctx, key)
	for _, m := range b.msgs {
		if err != nil && ctx.Err() != nil {
			// Interrupted by shutdown: the next run should get the event without a backoff delay.
			_ = m.Nak()
			continue
		}
		pluginkit.Settle(p.log.With("pr", key.String()), m, err, p.retryDelay, maxDeliver)
	}
}

// retryDelay doubles retryBase per previous attempt, capped at retryMax.
func (p *plugin) retryDelay(attempt int) time.Duration {
	return pluginkit.Backoff(p.retryBase, p.retryMax, attempt)
}

// stop discards pending batches; their events are redelivered to the next run.
func (p *plugin) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	for key, b := range p.pending {
		b.timer.Stop()
		for _, m := range b.msgs {
			_ = m.Nak()
		}
		delete(p.pending, key)
	}
}

// sweep evaluates the foreign open PRs of items, retrying failed ones with backoff up to maxDeliver
// attempts, like events.
func (p *plugin) sweep(ctx context.Context, items []api.InboxItem) {
	var keys []model.PRKey
	for _, it := range items {
		if foreign(it) && p.allowed(it.PR.Key) {
			keys = append(keys, it.PR.Key)
		}
	}
	for attempt := 1; len(keys) > 0; attempt++ {
		var failed []model.PRKey
		for _, key := range keys {
			if ctx.Err() != nil {
				return
			}
			if err := p.evaluate(ctx, key); err != nil {
				failed = append(failed, key)
			}
		}
		keys = failed
		if len(keys) == 0 {
			break
		}
		if attempt >= maxDeliver {
			p.log.Error("sweep gave up", "prs", len(keys), "attempts", attempt)
			return
		}
		delay := p.retryDelay(attempt)
		p.log.Warn("sweep incomplete, retrying", "prs", len(keys), "retry_in_ms", delay.Milliseconds())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
	p.log.Info("sweep done")
}

// foreign reports whether it is someone else's open PR.
func foreign(it api.InboxItem) bool {
	return it.Section == model.SectionReviewRequested || it.Section == model.SectionInvolved
}

func (p *plugin) allowed(key model.PRKey) bool {
	if len(p.cfg.Repos) == 0 {
		return true
	}
	return slices.ContainsFunc(p.cfg.Repos, func(r string) bool {
		owner, repo, hasRepo := strings.Cut(r, "/")
		return strings.EqualFold(owner, key.Owner) && (!hasRepo || strings.EqualFold(repo, key.Repo))
	})
}

// evaluate classifies the new candidate comments of key and updates its PR-level annotation.
func (p *plugin) evaluate(ctx context.Context, key model.PRKey) error {
	p.evalMu.Lock()
	defer p.evalMu.Unlock()
	log := p.log.With("pr", key.String())
	err := p.evaluateLocked(ctx, log, key)
	if err != nil {
		log.Warn("evaluate PR", "err", err)
	}
	return err
}

func (p *plugin) evaluateLocked(ctx context.Context, log *slog.Logger, key model.PRKey) error {
	item, err := p.c.Get(ctx, key)
	if err != nil {
		return err
	}
	if !foreign(item) {
		log.Debug("skipping PR that is not someone else's open PR", "section", item.Section)
		return nil
	}

	values := make(map[string]intentValue)
	for _, a := range item.Annotations {
		if a.CommentID == "" || a.Key != KeyIntent {
			continue
		}
		var v intentValue
		if err := json.Unmarshal(a.Value, &v); err != nil {
			log.Warn("ignoring undecodable annotation", "comment_id", a.CommentID, "key", a.Key, "err", err)
			continue
		}
		values[a.CommentID] = v
	}
	cands := candidates(item.PR, p.viewer)
	var todo []model.Comment
	for _, c := range cands {
		if _, ok := values[c.ID]; !ok {
			todo = append(todo, c)
		}
	}
	if len(todo) > 0 {
		classified, err := p.classify(ctx, item.PR, todo)
		if err != nil {
			return err
		}
		for _, c := range todo {
			v := classified[c.ID]
			raw, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if err := p.c.Annotate(ctx, model.Annotation{PR: key, CommentID: c.ID, Key: KeyIntent, Value: raw}); err != nil {
				return err
			}
			values[c.ID] = v
			log.Info("classified comment", "comment_id", c.ID, "intent", v.Intent, "probability", v.Probability)
		}
	}
	return p.updateAttention(ctx, log, item, result(cands, values, item.Reasons, p.cfg.Weights))
}

func (p *plugin) classify(ctx context.Context, pr model.PR, todo []model.Comment) (map[string]intentValue, error) {
	ids := make([]string, len(todo))
	for i, c := range todo {
		ids[i] = c.ID
	}
	resp, err := p.cfg.Jev.SystemOne(ctx, request(pr, p.viewer, todo),
		slog.String("pr", pr.Key.String()), slog.Any("comment_ids", ids))
	if err != nil {
		return nil, err
	}
	out := make(map[string]intentValue, len(todo))
	for _, c := range todo {
		a, ok := resp.Answers[c.ID]
		if !ok {
			return nil, fmt.Errorf("jev: no answer for comment %s", c.ID)
		}
		v, err := classification(a, p.cfg.Threshold)
		if err != nil {
			return nil, fmt.Errorf("jev: comment %s: %w", c.ID, err)
		}
		out[c.ID] = v
	}
	return out, nil
}

// updateAttention writes want as the PR-level annotation, or deletes it when want is nil. An equal
// annotation is rewritten only when the viewer acted after it was written, because the core ignores
// annotations older than the viewer's last activity.
func (p *plugin) updateAttention(ctx context.Context, log *slog.Logger, item api.InboxItem, want *attentionValue) error {
	i := slices.IndexFunc(item.Annotations, func(a model.Annotation) bool { return a.CommentID == "" && a.Key == KeyAttention })
	ann := model.Annotation{PR: item.PR.Key, Key: KeyAttention, Value: json.RawMessage("null")}
	if want == nil {
		if i < 0 {
			return nil
		}
		log.Info("PR needs nothing", "key", KeyAttention)
		return p.c.Annotate(ctx, ann)
	}
	if i >= 0 {
		var have attentionValue
		existing := item.Annotations[i]
		if json.Unmarshal(existing.Value, &have) == nil && reflect.DeepEqual(have, *want) &&
			existing.UpdatedAt.After(derive.LastActivity(item.PR, p.viewer)) {
			return nil
		}
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return err
	}
	ann.Value = raw
	log.Info("PR needs attention", "score", want.Score, "reasons", want.Attention)
	return p.c.Annotate(ctx, ann)
}
