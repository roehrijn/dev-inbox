// Package linearplugin is the dev-inbox-linear plugin: it links PRs to Linear issues and annotates
// PRs whose issues, projects or milestones are due soon with derive.UrgencyKey and lists all linked
// issues in IssuesKey, using only the public NATS API of the daemon.
//
// Only the URL, title and head branch name of a PR are sent to Linear.
package linearplugin

import (
	"cmp"
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
	"github.com/roehrijn/dev-inbox/internal/linear"
	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/pluginkit"
)

const (
	// ServiceName is the NATS micro service the plugin registers, which the TUI lists as live.
	ServiceName    = "devinbox-linear"
	serviceVersion = "0.1.0"
	// ConsumerName is the durable JetStream consumer of the plugin.
	ConsumerName = "linear"
	// Source marks the annotations the plugin writes.
	Source = "dev-inbox-linear"
	// IssuesKey is the PR-level annotation listing all linked Linear issues. Its value carries no
	// score, attention or badge, so it never affects the ranking.
	IssuesKey   = "linear.issues"
	issuesLabel = "Linear issues"

	DefaultHorizonDays   = 7
	DefaultSweepInterval = time.Hour

	ackWait    = time.Minute
	maxDeliver = 10
	dateLayout = "2006-01-02"
)

// triggers are the event types that can change which Linear issues a PR links to. pr.annotated is
// not among them, so the plugin's own annotations never trigger it.
var triggers = []string{model.EventDiscovered, model.EventReopened, model.EventUpdated}

// FilterSubjects are the subjects of the trigger events, see api.EventSubject.
func FilterSubjects() []string { return pluginkit.FilterSubjects(triggers) }

// Linear is the subset of *linear.Client the plugin uses.
type Linear interface {
	IssuesForPR(ctx context.Context, pr linear.PRRef) ([]linear.Issue, error)
}

type Config struct {
	NATSURL string
	// HorizonDays is how many days ahead a due date makes a PR urgent; overdue and due today always do.
	HorizonDays int
	// SweepInterval is how often all open PRs are re-evaluated, since due dates change outside GitHub.
	SweepInterval time.Duration
	Linear        Linear
	Logger        *slog.Logger
	// Now is time.Now when nil.
	Now func() time.Time
}

type plugin struct {
	cfg Config
	log *slog.Logger
	c   *pluginkit.Client

	// evalMu serializes evaluations, so one PR is never annotated concurrently.
	evalMu sync.Mutex
	// evaluated is what was sent to Linear at each PR's last successful evaluation.
	evaluated map[model.PRKey]linear.PRRef // guarded by evalMu
	running   sync.WaitGroup

	retryBase time.Duration
	retryMax  time.Duration
}

// urgencyValue is the value of the derive.UrgencyKey annotation.
type urgencyValue struct {
	Badge   string   `json:"badge"`
	Level   string   `json:"level"`
	DueDate string   `json:"dueDate"`
	Reason  string   `json:"reason"`
	Issues  []string `json:"issues"`
}

// linksValue is the value of the IssuesKey annotation.
type linksValue struct {
	Label string `json:"label"`
	Links []link `json:"links"`
}

type link struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// Run connects to the daemon and annotates until ctx ends. It returns nil on shutdown.
func Run(ctx context.Context, cfg Config) error {
	p, err := newPlugin(cfg)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

func newPlugin(cfg Config) (*plugin, error) {
	if cfg.Linear == nil {
		return nil, errors.New("linearplugin: Linear client is required")
	}
	if cfg.HorizonDays < 0 {
		return nil, fmt.Errorf("linearplugin: horizon of %d days is negative", cfg.HorizonDays)
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = DefaultSweepInterval
	}
	if cfg.SweepInterval < 0 {
		return nil, fmt.Errorf("linearplugin: sweep interval %s is negative", cfg.SweepInterval)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &plugin{
		cfg:       cfg,
		log:       cfg.Logger,
		evaluated: make(map[model.PRKey]linear.PRRef),
		retryBase: 5 * time.Second,
		retryMax:  5 * time.Minute,
	}, nil
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
		Description: "Annotates PRs whose Linear issues are due soon",
	})
	if err != nil {
		return fmt.Errorf("register service %s: %w", ServiceName, err)
	}
	defer func() { _ = svc.Stop() }()

	list, ok := c.AwaitDaemon(ctx)
	if !ok {
		return nil
	}
	p.log.Info("connected to daemon", "viewer", list.Viewer.Login, "prs", len(list.Items))

	cc, err := p.consume(ctx)
	if err != nil {
		return err
	}
	var sweeps sync.WaitGroup
	sweeps.Go(func() { p.sweepLoop(ctx, list.Items) })

	<-ctx.Done()
	cc.Stop()
	<-cc.Closed()
	sweeps.Wait()
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

func (p *plugin) receive(ctx context.Context, msg jetstream.Msg) {
	p.running.Add(1)
	defer p.running.Done()
	if ctx.Err() != nil {
		_ = msg.Nak()
		return
	}
	key, typ, ref, err := decodeEvent(msg.Data())
	if err != nil {
		p.log.Error("dropping undecodable event", "subject", msg.Subject(), "err", err)
		if err := msg.TermWithReason("undecodable event"); err != nil {
			p.log.Warn("terminate event", "err", err)
		}
		return
	}
	log := p.log.With("pr", key.String())
	if typ == model.EventUpdated && p.unchanged(key, ref) {
		log.Debug("skipping update that does not change what Linear sees")
		if err := msg.Ack(); err != nil {
			log.Warn("ack event", "err", err)
		}
		return
	}
	_, err = p.evaluate(ctx, key)
	if err != nil && ctx.Err() != nil {
		// Interrupted by shutdown: the next run should get the event without a backoff delay.
		_ = msg.Nak()
		return
	}
	pluginkit.Settle(log, msg, err, p.retryDelay, maxDeliver)
}

func decodeEvent(data []byte) (model.PRKey, string, linear.PRRef, error) {
	key, err := pluginkit.EventKey(data)
	if err != nil {
		return model.PRKey{}, "", linear.PRRef{}, err
	}
	var ce api.CloudEvent
	if err := json.Unmarshal(data, &ce); err != nil {
		return model.PRKey{}, "", linear.PRRef{}, err
	}
	var ed model.EventData
	if err := json.Unmarshal(ce.Data, &ed); err != nil {
		return model.PRKey{}, "", linear.PRRef{}, err
	}
	return key, strings.TrimPrefix(ce.Type, api.CloudEventTypePrefix), prRef(ed.PR), nil
}

func prRef(pr model.PR) linear.PRRef {
	return linear.PRRef{URL: pr.URL, Title: pr.Title, Branch: pr.HeadRef}
}

// unchanged reports whether ref is what Linear saw at key's last successful evaluation.
func (p *plugin) unchanged(key model.PRKey, ref linear.PRRef) bool {
	p.evalMu.Lock()
	defer p.evalMu.Unlock()
	last, ok := p.evaluated[key]
	return ok && last == ref
}

func (p *plugin) retryDelay(attempt int) time.Duration {
	return pluginkit.Backoff(p.retryBase, p.retryMax, attempt)
}

// sweepLoop evaluates items, then all listed PRs every SweepInterval. Failed PRs wait for the next
// sweep.
func (p *plugin) sweepLoop(ctx context.Context, items []api.InboxItem) {
	p.sweep(ctx, items)
	t := time.NewTicker(p.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := p.c.List(ctx)
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("sweep: list inbox", "err", err)
			}
			continue
		}
		p.sweep(ctx, list.Items)
	}
}

func (p *plugin) sweep(ctx context.Context, items []api.InboxItem) {
	var evaluated, urgent, failed int
	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		if it.Section == model.SectionNone {
			continue
		}
		u, err := p.evaluate(ctx, it.PR.Key)
		switch {
		case err != nil:
			failed++
		case u:
			urgent++
		}
		evaluated++
	}
	p.log.Info("sweep done", "prs", evaluated, "urgent", urgent, "failed", failed)
}

// evaluate updates the urgency and issues annotations of key and reports whether the PR is urgent.
func (p *plugin) evaluate(ctx context.Context, key model.PRKey) (bool, error) {
	p.evalMu.Lock()
	defer p.evalMu.Unlock()
	log := p.log.With("pr", key.String())
	urgent, err := p.evaluateLocked(ctx, log, key)
	if err != nil {
		log.Warn("evaluate PR", "err", err)
	}
	return urgent, err
}

func (p *plugin) evaluateLocked(ctx context.Context, log *slog.Logger, key model.PRKey) (bool, error) {
	item, err := p.c.Get(ctx, key)
	if err != nil {
		return false, err
	}
	if item.Section == model.SectionNone {
		log.Debug("skipping closed PR")
		return false, nil
	}
	ref := prRef(item.PR)
	issues, err := p.cfg.Linear.IssuesForPR(ctx, ref)
	if err != nil {
		return false, fmt.Errorf("linear: %w", err)
	}
	want := urgency(log, issues, p.cfg.Now(), p.cfg.HorizonDays)
	if err := p.update(ctx, log, item, want, linkedIssues(issues)); err != nil {
		return false, err
	}
	p.evaluated[key] = ref
	return want != nil, nil
}

// update brings the PR-level urgency and issues annotations in line with want and links.
func (p *plugin) update(ctx context.Context, log *slog.Logger, item api.InboxItem, want *urgencyValue, links *linksValue) error {
	have, changed, err := syncAnnotation(ctx, p.c, item, derive.UrgencyKey, want)
	if err != nil {
		return err
	}
	switch {
	case !changed:
	case want == nil:
		var old struct {
			Badge string `json:"badge"`
		}
		_ = json.Unmarshal(have, &old)
		log.Info("PR no longer urgent", "badge", old.Badge)
	default:
		log.Info("PR urgent", "badge", want.Badge, "reason", want.Reason)
	}
	_, _, err = syncAnnotation(ctx, p.c, item, IssuesKey, links)
	return err
}

// syncAnnotation writes want as the PR-level annotation key when it differs from the current one, or
// deletes the annotation when want is nil. have is the current value, nil without one.
func syncAnnotation[T any](ctx context.Context, c *pluginkit.Client, item api.InboxItem, key string, want *T) (have json.RawMessage, changed bool, err error) {
	i := slices.IndexFunc(item.Annotations, func(a model.Annotation) bool {
		return a.CommentID == "" && a.Key == key
	})
	if i >= 0 {
		have = item.Annotations[i].Value
	}
	ann := model.Annotation{PR: item.PR.Key, Key: key, Value: json.RawMessage("null")}
	if want == nil {
		if i < 0 {
			return nil, false, nil
		}
		return have, true, c.Annotate(ctx, ann)
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return have, false, err
	}
	if i >= 0 && sameJSON(have, raw) {
		return have, false, nil
	}
	ann.Value = raw
	return have, true, c.Annotate(ctx, ann)
}

func sameJSON(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

const (
	kindIssue = iota
	kindMilestone
	kindProject
)

type candidate struct {
	due    time.Time
	kind   int
	issue  string
	reason string
}

// urgency returns the annotation value for issues as of now, or nil when none is due within
// horizonDays.
func urgency(log *slog.Logger, issues []linear.Issue, now time.Time, horizonDays int) *urgencyValue {
	var open []string
	var cands []candidate
	add := func(raw string, kind int, issue, reason string) {
		if raw == "" {
			return
		}
		due, err := time.Parse(dateLayout, raw)
		if err != nil {
			log.Warn("ignoring invalid Linear date", "issue", issue, "reason", reason, "date", raw, "err", err)
			return
		}
		cands = append(cands, candidate{due: due, kind: kind, issue: issue, reason: reason})
	}
	for _, is := range issues {
		if is.Done {
			continue
		}
		open = append(open, is.Identifier)
		add(is.DueDate, kindIssue, is.Identifier, "issue "+is.Identifier)
		if is.Milestone != nil {
			add(is.Milestone.TargetDate, kindMilestone, is.Identifier, "milestone "+is.Milestone.Name)
		}
		if is.Project != nil && !is.Project.Done {
			add(is.Project.TargetDate, kindProject, is.Identifier, "project "+is.Project.Name)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	first := slices.MinFunc(cands, func(a, b candidate) int {
		return cmp.Or(a.due.Compare(b.due), cmp.Compare(a.kind, b.kind), cmp.Compare(a.issue, b.issue))
	})
	// Both are midnight UTC, so the difference is a whole number of days.
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	days := int(first.due.Sub(today).Hours() / 24)
	if days > horizonDays {
		return nil
	}
	slices.Sort(open)
	return &urgencyValue{
		Badge:   badge(days),
		Level:   level(days),
		DueDate: first.due.Format(dateLayout),
		Reason:  first.reason,
		Issues:  slices.Compact(open),
	}
}

// linkedIssues returns the annotation value listing issues, or nil without any.
func linkedIssues(issues []linear.Issue) *linksValue {
	if len(issues) == 0 {
		return nil
	}
	v := &linksValue{Label: issuesLabel, Links: make([]link, 0, len(issues))}
	for _, is := range issues {
		v.Links = append(v.Links, link{ID: is.Identifier, Title: is.Title, URL: is.URL, Detail: detail(is)})
	}
	return v
}

func detail(is linear.Issue) string {
	var parts []string
	if is.State != "" {
		parts = append(parts, is.State)
	}
	if is.DueDate != "" {
		parts = append(parts, "due "+is.DueDate)
	}
	if is.Milestone != nil && is.Milestone.Name != "" {
		parts = append(parts, "milestone "+is.Milestone.Name)
	}
	if is.Project != nil && is.Project.Name != "" {
		parts = append(parts, "project "+is.Project.Name)
	}
	return strings.Join(parts, " · ")
}

func badge(days int) string {
	switch {
	case days < 0:
		return fmt.Sprintf("overdue %dd", -days)
	case days == 0:
		return "due today"
	case days == 1:
		return "due tomorrow"
	default:
		return fmt.Sprintf("due in %dd", days)
	}
}

func level(days int) string {
	switch {
	case days <= 0:
		return "crit"
	case days <= 2:
		return "warn"
	default:
		return "info"
	}
}
