// Package slackplugin is the dev-inbox-slack plugin: it reads review channels on Slack, asks the
// daemon to track PRs that tag the viewer there, directly or through one of the viewer's user groups,
// and annotates those PRs with MentionsKey, using only the public NATS API of the daemon.
package slackplugin

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
	"time"

	"github.com/nats-io/nats.go/micro"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/pluginkit"
	"github.com/roehrijn/dev-inbox/internal/slack"
)

const (
	// ServiceName is the NATS micro service the plugin registers, which the TUI lists as live.
	ServiceName    = "devinbox-slack"
	serviceVersion = "0.1.0"
	// Source marks the annotations the plugin writes.
	Source = "dev-inbox-slack"
	// MentionsKey is the PR-level annotation of a PR tagged in a channel since the viewer's last
	// activity: {"score", "attention", "label", "links"}.
	MentionsKey = "slack.mentions"

	DefaultScore        = 10
	DefaultPollInterval = time.Minute
	DefaultLookback     = 7 * 24 * time.Hour

	mentionsLabel = "Slack"
	// retrackAfter is how long a PR the daemon was asked to track is not asked for again, so a PR the
	// GitHub token cannot read is not requested every poll.
	retrackAfter = time.Hour
	titleMax     = 100
	// detailLayout is absolute, unlike "2h ago", so an unchanged message keeps the annotation unchanged.
	detailLayout = "Jan 2 15:04"
)

// Slack is the subset of *slack.Client the plugin uses.
type Slack interface {
	AuthTest(ctx context.Context) (slack.Identity, error)
	Channel(ctx context.Context, idOrName string) (slack.Channel, error)
	History(ctx context.Context, channel string, oldest time.Time) ([]slack.Message, error)
	Replies(ctx context.Context, channel, threadTS string) ([]slack.Message, error)
	UserName(ctx context.Context, userID string) (string, error)
}

type Config struct {
	NATSURL string
	// Channels are channel IDs or names to read.
	Channels []string
	// Groups are the IDs (S...) of the user groups whose tags count as tagging the viewer.
	Groups []string
	// Score is what a tagged PR gains, once per PR.
	Score        int
	PollInterval time.Duration
	// Lookback is how far back messages count.
	Lookback time.Duration
	Slack    Slack
	Logger   *slog.Logger
	// Now is time.Now when nil.
	Now func() time.Time
}

// daemon is the subset of *pluginkit.Client the plugin uses.
type daemon interface {
	List(ctx context.Context) (api.ListResponse, error)
	Get(ctx context.Context, key model.PRKey) (api.InboxItem, error)
	Annotate(ctx context.Context, a model.Annotation) error
	Track(ctx context.Context, key model.PRKey) (known bool, err error)
}

type plugin struct {
	cfg Config
	log *slog.Logger
	d   daemon

	me       slack.Identity
	channels []slack.Channel
	// history is the last history read per channel ID, used while a channel fails.
	history map[string][]slack.Message
	threads map[threadKey]thread
	// tracked is when the daemon was last asked to track a PR.
	tracked map[model.PRKey]time.Time

	retryBase time.Duration
	retryMax  time.Duration
}

type threadKey struct{ channel, ts string }

type thread struct {
	latestReply string
	replies     []slack.Message
}

// mentionsValue is the value of the MentionsKey annotation.
type mentionsValue struct {
	Score     int      `json:"score"`
	Attention []string `json:"attention"`
	Label     string   `json:"label"`
	Links     []link   `json:"links"`
}

type link struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// Run connects to the daemon and Slack and annotates until ctx ends. It returns nil on shutdown.
func Run(ctx context.Context, cfg Config) error {
	p, err := newPlugin(cfg)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

func newPlugin(cfg Config) (*plugin, error) {
	if cfg.Slack == nil {
		return nil, errors.New("slackplugin: Slack client is required")
	}
	if len(cfg.Channels) == 0 {
		return nil, errors.New("slackplugin: at least one channel is required")
	}
	if cfg.Score < 0 {
		return nil, fmt.Errorf("slackplugin: score %d is negative", cfg.Score)
	}
	if cfg.PollInterval < 0 {
		return nil, fmt.Errorf("slackplugin: poll interval %s is negative", cfg.PollInterval)
	}
	if cfg.Lookback < 0 {
		return nil, fmt.Errorf("slackplugin: lookback %s is negative", cfg.Lookback)
	}
	if cfg.Score == 0 {
		cfg.Score = DefaultScore
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Lookback == 0 {
		cfg.Lookback = DefaultLookback
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
		history:   make(map[string][]slack.Message),
		threads:   make(map[threadKey]thread),
		tracked:   make(map[model.PRKey]time.Time),
		retryBase: 5 * time.Second,
		retryMax:  5 * time.Minute,
	}, nil
}

func (p *plugin) run(ctx context.Context) error {
	c, err := pluginkit.Connect(p.cfg.NATSURL, Source, p.log)
	if err != nil {
		return err
	}
	p.d = c
	defer c.Close()

	svc, err := micro.AddService(c.Conn(), micro.Config{
		Name:        ServiceName,
		Version:     serviceVersion,
		Description: "Annotates PRs that tag the viewer in Slack review channels",
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

	if err := p.connectSlack(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	p.pollLoop(ctx)
	p.log.Info("stopped")
	return nil
}

// connectSlack identifies the viewer and resolves the channels, retrying until it succeeds or ctx ends.
// A channel Slack does not know is an error.
func (p *plugin) connectSlack(ctx context.Context) error {
	err := p.retry(ctx, p.log, "slack auth.test", func() error {
		id, err := p.cfg.Slack.AuthTest(ctx)
		if err != nil {
			return err
		}
		if id.UserID == "" {
			return errors.New("auth.test returned no user ID")
		}
		p.me = id
		return nil
	})
	if err != nil {
		return err
	}
	p.log.Info("connected to slack", "user", p.me.UserID)
	for _, name := range p.cfg.Channels {
		var ch slack.Channel
		err := p.retry(ctx, p.log.With("channel", name), "resolve slack channel", func() error {
			var err error
			ch, err = p.cfg.Slack.Channel(ctx, name)
			if apiErr := (*slack.APIError)(nil); errors.As(err, &apiErr) && apiErr.Code == "channel_not_found" {
				return permanentError{fmt.Errorf("slack channel %s: %w", name, err)}
			}
			return err
		})
		if err != nil {
			return err
		}
		p.channels = append(p.channels, ch)
	}
	return nil
}

// permanentError ends retry at once.
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// retry calls fn until it succeeds, fails with a permanentError or ctx ends, whose error it returns.
func (p *plugin) retry(ctx context.Context, log *slog.Logger, what string, fn func() error) error {
	for attempt := 1; ; attempt++ {
		err := fn()
		var permanent permanentError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &permanent):
			return permanent.error
		case ctx.Err() != nil:
			return ctx.Err()
		}
		d := pluginkit.Backoff(p.retryBase, p.retryMax, attempt)
		log.Warn(what, "err", err, "retry_in_ms", d.Milliseconds())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

func (p *plugin) pollLoop(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.poll(ctx)
	}
}

type pollStats struct {
	prs, written, deleted, tracked, failed int
}

func (p *plugin) poll(ctx context.Context) {
	now := p.cfg.Now()
	ms, complete := p.collect(ctx, now)
	if ctx.Err() != nil {
		return
	}
	st, err := p.apply(ctx, ms, complete)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("poll: skipping annotations", "err", err)
		}
		return
	}
	p.log.Info("poll done", "mentions", len(ms), "prs", st.prs, "written", st.written, "deleted", st.deleted,
		"tracked", st.tracked, "failed", st.failed, "complete", complete)
}

// collect returns the mentions of the viewer in all channels within the lookback window. complete is
// false if a channel or thread could not be read and nothing earlier stands in for it.
func (p *plugin) collect(ctx context.Context, now time.Time) (ms []mention, complete bool) {
	oldest := now.Add(-p.cfg.Lookback)
	complete = true
	for _, ch := range p.channels {
		if ctx.Err() != nil {
			return ms, false
		}
		chMentions, ok := p.collectChannel(ctx, ch, oldest)
		ms = append(ms, chMentions...)
		complete = complete && ok
	}
	return ms, complete
}

func (p *plugin) collectChannel(ctx context.Context, ch slack.Channel, oldest time.Time) ([]mention, bool) {
	log := p.log.With("channel", ch.Name)
	hist, err := p.cfg.Slack.History(ctx, ch.ID, oldest)
	if err != nil {
		prev, ok := p.history[ch.ID]
		if !ok {
			log.Warn("read channel history, skipping the channel", "err", err)
			return nil, false
		}
		log.Warn("read channel history, using the previous read", "err", err)
		hist = slices.DeleteFunc(slices.Clone(prev), func(m slack.Message) bool {
			t, ok := tsTime(m.TS)
			return !ok || t.Before(oldest)
		})
	}
	p.history[ch.ID] = hist

	complete := true
	seen := make(map[string]bool)
	var out []mention
	add := func(m slack.Message, parent *slack.Message) {
		if seen[m.TS] {
			return
		}
		seen[m.TS] = true
		if mn, ok := toMention(ch, p.me.UserID, p.cfg.Groups, m, parent); ok {
			out = append(out, mn)
		}
	}
	parents := make(map[string]*slack.Message)
	for i := range hist {
		m := &hist[i]
		if isReply(*m) {
			continue
		}
		add(*m, nil)
		if m.ReplyCount == 0 {
			continue
		}
		parents[m.TS] = m
		replies, ok := p.replies(ctx, log, ch, *m)
		complete = complete && ok
		for _, r := range replies {
			if r.TS != m.TS {
				add(r, m)
			}
		}
	}
	// Replies also sent to the channel; those of threads read above are already seen.
	for _, m := range hist {
		if isReply(m) {
			add(m, parents[m.ThreadTS])
		}
	}
	for k := range p.threads {
		if k.channel == ch.ID && parents[k.ts] == nil {
			delete(p.threads, k)
		}
	}
	return out, complete
}

// replies returns the replies of thread parent, read again only when its latest reply changed. A thread
// that cannot be read keeps its previous replies; without any, ok is false.
func (p *plugin) replies(ctx context.Context, log *slog.Logger, ch slack.Channel, parent slack.Message) ([]slack.Message, bool) {
	k := threadKey{channel: ch.ID, ts: parent.TS}
	prev, cached := p.threads[k]
	if cached && prev.latestReply == parent.LatestReply {
		return prev.replies, true
	}
	rs, err := p.cfg.Slack.Replies(ctx, ch.ID, parent.TS)
	if err != nil {
		if cached {
			log.Warn("read thread, using the previous read", "thread", parent.TS, "err", err)
			return prev.replies, true
		}
		log.Warn("read thread, skipping it", "thread", parent.TS, "err", err)
		return nil, false
	}
	p.threads[k] = thread{latestReply: parent.LatestReply, replies: rs}
	return rs, true
}

// apply brings the annotations in line with ms. Unless complete, it deletes nothing, since a mention
// missing from ms may only be unreadable.
func (p *plugin) apply(ctx context.Context, ms []mention, complete bool) (pollStats, error) {
	var st pollStats
	list, err := p.d.List(ctx)
	if err != nil {
		return st, err
	}
	now := p.cfg.Now()
	// Links are written by hand, so their owner and repo may differ in case from the stored PR.
	canon := make(map[model.PRKey]model.PRKey, len(list.Items))
	for _, it := range list.Items {
		canon[foldKey(it.PR.Key)] = it.PR.Key
	}
	byPR := make(map[model.PRKey][]mention)
	for _, m := range ms {
		for _, k := range m.prs {
			if c, ok := canon[foldKey(k)]; ok {
				k = c
			}
			if l := byPR[k]; len(l) > 0 && l[len(l)-1].ts == m.ts && l[len(l)-1].channel.ID == m.channel.ID {
				continue
			}
			byPR[k] = append(byPR[k], m)
		}
	}
	keys := make([]model.PRKey, 0, len(byPR))
	for k := range byPR {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b model.PRKey) int {
		return cmp.Or(cmp.Compare(a.Owner, b.Owner), cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number))
	})
	for _, key := range keys {
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		st.prs++
		log := p.log.With("pr", key.String())
		res, err := p.applyPR(ctx, log, now, list.Viewer, key, byPR[key], complete)
		if err != nil {
			log.Warn("annotate PR", "err", err)
			st.failed++
			continue
		}
		switch res {
		case resWritten:
			st.written++
		case resDeleted:
			st.deleted++
		case resTracked:
			st.tracked++
		}
	}
	for k, t := range p.tracked {
		if now.Sub(t) >= retrackAfter {
			delete(p.tracked, k)
		}
	}
	if !complete {
		return st, nil
	}
	for _, it := range list.Items {
		if _, ok := byPR[it.PR.Key]; ok || !slices.ContainsFunc(it.Annotations, own) {
			continue
		}
		log := p.log.With("pr", it.PR.Key.String())
		if err := p.d.Annotate(ctx, model.Annotation{PR: it.PR.Key, Key: MentionsKey, Value: json.RawMessage("null")}); err != nil {
			log.Warn("delete annotation", "err", err)
			st.failed++
			continue
		}
		log.Info("PR no longer tagged in slack")
		st.deleted++
	}
	return st, nil
}

func foldKey(k model.PRKey) model.PRKey {
	return model.PRKey{Owner: strings.ToLower(k.Owner), Repo: strings.ToLower(k.Repo), Number: k.Number}
}

func own(a model.Annotation) bool {
	return a.CommentID == "" && a.Key == MentionsKey && a.Source == Source
}

type result int

const (
	resUnchanged result = iota
	resWritten
	resDeleted
	resTracked
)

func (p *plugin) applyPR(ctx context.Context, log *slog.Logger, now time.Time, viewer model.Viewer, key model.PRKey, ms []mention, complete bool) (result, error) {
	item, err := p.d.Get(ctx, key)
	if err != nil {
		if !strings.Contains(err.Error(), "unknown PR") {
			return resUnchanged, err
		}
		return p.track(ctx, log, now, key)
	}
	delete(p.tracked, key)
	if item.Section == model.SectionNone {
		return resUnchanged, nil
	}
	last := derive.LastActivity(item.PR, viewer)
	ms = slices.DeleteFunc(slices.Clone(ms), func(m mention) bool { return !m.time.After(last) })
	want := p.value(ctx, now.Location(), ms)
	if want == nil && !complete {
		return resUnchanged, nil
	}
	return p.sync(ctx, log, item, last, want)
}

// track asks the daemon for a PR it does not know, at most once per retrackAfter.
func (p *plugin) track(ctx context.Context, log *slog.Logger, now time.Time, key model.PRKey) (result, error) {
	if t, ok := p.tracked[key]; ok && now.Sub(t) < retrackAfter {
		log.Debug("PR unknown to the daemon, tracking already requested")
		return resUnchanged, nil
	}
	known, err := p.d.Track(ctx, key)
	if err != nil {
		return resUnchanged, err
	}
	if known {
		log.Debug("PR became known to the daemon, annotating on the next poll")
		return resUnchanged, nil
	}
	p.tracked[key] = now
	log.Info("asked the daemon to track PR tagged in slack")
	return resTracked, nil
}

// sync writes want as the annotation when it differs from the current one, or deletes the annotation
// when want is nil. An equal annotation is rewritten when the viewer acted after it was written,
// because the core ignores annotations older than the viewer's last activity.
func (p *plugin) sync(ctx context.Context, log *slog.Logger, item api.InboxItem, last time.Time, want *mentionsValue) (result, error) {
	i := slices.IndexFunc(item.Annotations, func(a model.Annotation) bool { return a.CommentID == "" && a.Key == MentionsKey })
	ann := model.Annotation{PR: item.PR.Key, Key: MentionsKey, Value: json.RawMessage("null")}
	if want == nil {
		if i < 0 {
			return resUnchanged, nil
		}
		log.Info("PR no longer tagged in slack since the viewer's last activity")
		return resDeleted, p.d.Annotate(ctx, ann)
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return resUnchanged, err
	}
	if i >= 0 && sameJSON(item.Annotations[i].Value, raw) && item.Annotations[i].UpdatedAt.After(last) {
		return resUnchanged, nil
	}
	ann.Value = raw
	log.Info("PR tagged in slack", "messages", len(want.Links), "attention", want.Attention)
	return resWritten, p.d.Annotate(ctx, ann)
}

func sameJSON(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// value returns the annotation value for the mentions of one PR, or nil without any. Times in link
// details are shown in loc.
func (p *plugin) value(ctx context.Context, loc *time.Location, ms []mention) *mentionsValue {
	if len(ms) == 0 {
		return nil
	}
	ms = slices.Clone(ms)
	slices.SortFunc(ms, func(a, b mention) int {
		return cmp.Or(b.time.Compare(a.time), cmp.Compare(a.channel.ID, b.channel.ID), cmp.Compare(b.ts, a.ts))
	})
	v := &mentionsValue{Score: p.cfg.Score, Label: mentionsLabel, Links: make([]link, 0, len(ms))}
	for _, m := range ms {
		reason := "tagged your team in #" + m.channel.Name
		if m.kind == tagDirect {
			reason = "tagged you in #" + m.channel.Name
		}
		if !slices.Contains(v.Attention, reason) {
			v.Attention = append(v.Attention, reason)
		}
		v.Links = append(v.Links, link{
			ID:     "@" + p.userName(ctx, m.user),
			Title:  simplify(m.text, func(id string) string { return p.userName(ctx, id) }),
			URL:    permalink(p.me.TeamURL, m.channel.ID, m.ts, m.threadTS),
			Detail: "#" + m.channel.Name + " · " + m.time.In(loc).Format(detailLayout),
		})
	}
	return v
}

// userName returns the display name of a user, or the ID if Slack cannot tell.
func (p *plugin) userName(ctx context.Context, id string) string {
	name, err := p.cfg.Slack.UserName(ctx, id)
	if err != nil || name == "" {
		p.log.Debug("look up slack user", "user", id, "err", err)
		return id
	}
	return name
}
