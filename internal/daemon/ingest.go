package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/github"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	// itemTimeout bounds one ingestion, including the one still running at shutdown.
	itemTimeout = 2 * time.Minute
	maxAttempts = 3
	retryDelay  = 30 * time.Second
)

var backfillQueries = []string{"involves:@me", "review-requested:@me", "reviewed-by:@me"}

// worker ingests queued PRs one at a time until ctx ends.
func (d *daemon) worker(ctx context.Context) {
	for {
		if err := d.gate.wait(ctx); err != nil {
			return
		}
		it, ok := d.queue.pop(ctx)
		if !ok {
			return
		}
		ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), itemTimeout)
		err := d.ingest(ictx, it)
		cancel()
		d.settle(it, err)
	}
}

func (d *daemon) settle(it item, err error) {
	var rl *github.RateLimitError
	switch {
	case err == nil:
		it.finish()
	case errors.As(err, &rl):
		d.gate.pause(rl.Reset)
		d.queue.push(it)
	case errors.Is(err, github.ErrNotFound):
		d.log.Info("skipping inaccessible PR", "pr", it.key.String(), "err", err)
		it.finish()
	case it.attempts+1 < maxAttempts:
		it.attempts++
		delay := retryDelay * time.Duration(it.attempts)
		d.log.Warn("ingest failed, retrying", "pr", it.key.String(), "err", err, "in", delay)
		time.AfterFunc(delay, func() { d.queue.push(it) })
	default:
		d.log.Error("ingest failed, giving up", "pr", it.key.String(), "err", err)
		it.finish()
	}
}

// ingest fetches a PR, derives events against the stored snapshot and stores both.
func (d *daemon) ingest(ctx context.Context, it item) error {
	cur, err := d.gh.FetchPR(ctx, it.key)
	if err != nil {
		return err
	}
	old, ok, err := d.store.GetPR(ctx, cur.Key)
	if err != nil {
		return fmt.Errorf("load stored PR: %w", err)
	}
	var prev *model.PR
	if ok {
		prev = &old
	}
	annotations, err := d.store.Annotations(ctx, cur.Key)
	if err != nil {
		return fmt.Errorf("load annotations: %w", err)
	}
	events, err := derive.Diff(prev, cur, d.inputs(annotations))
	if err != nil {
		return fmt.Errorf("derive events: %w", err)
	}
	if it.baseline {
		events = slices.DeleteFunc(events, func(ev model.Event) bool { return ev.Type != model.EventDiscovered })
	}
	if err := d.store.ApplySnapshot(ctx, cur, events); err != nil {
		return fmt.Errorf("store snapshot: %w", err)
	}
	if len(events) > 0 {
		d.bus.Kick()
	}
	d.log.Debug("ingested PR", "pr", cur.Key.String(), "events", len(events))
	return nil
}

// backfill discovers the open PRs involving the viewer once per state directory. The meta marker is
// only set after every found PR was ingested, so an interrupted backfill reruns on the next start.
func (d *daemon) backfill(ctx context.Context) {
	_, done, err := d.store.Meta(ctx, metaBackfillDone)
	if err != nil {
		d.log.Error("read backfill marker", "err", err)
		return
	}
	if done {
		return
	}
	for {
		keys, err := d.searchBackfill(ctx)
		if err == nil {
			d.runBackfill(ctx, keys)
			return
		}
		if ctx.Err() != nil {
			return
		}
		d.log.Error("backfill search failed, retrying", "err", err, "in", retryInterval)
		if !sleep(ctx, retryInterval) {
			return
		}
	}
}

func (d *daemon) searchBackfill(ctx context.Context) ([]model.PRKey, error) {
	seen := map[model.PRKey]bool{}
	var keys []model.PRKey
	for _, q := range backfillQueries {
		found, err := call(ctx, d.gate, func() ([]model.PRKey, error) { return d.gh.SearchOpenPRs(ctx, q) })
		if err != nil {
			return nil, err
		}
		for _, k := range found {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	return keys, nil
}

func (d *daemon) runBackfill(ctx context.Context, keys []model.PRKey) {
	d.log.Info("backfill started", "prs", len(keys))
	var wg sync.WaitGroup
	wg.Add(len(keys))
	for _, k := range keys {
		d.queue.push(item{key: k, baseline: true, done: []func(){wg.Done}})
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-ctx.Done():
		return
	case <-finished:
	}
	if err := d.store.SetMeta(ctx, metaBackfillDone, time.Now().UTC().Format(time.RFC3339)); err != nil {
		d.log.Error("write backfill marker", "err", err)
		return
	}
	d.log.Info("backfill done", "prs", len(keys))
}

// notificationsLoop polls notifications with the interval GitHub asks for, but never more often than
// the configured one.
func (d *daemon) notificationsLoop(ctx context.Context) {
	for {
		interval := d.cfg.PollInterval
		res, err := d.pollNotifications(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.log.Error("poll notifications", "err", err)
		}
		interval = max(interval, res.PollInterval)
		if !sleep(ctx, interval) {
			return
		}
	}
}

func (d *daemon) pollNotifications(ctx context.Context) (github.NotificationsResult, error) {
	since, err := d.notificationsSince(ctx)
	if err != nil {
		return github.NotificationsResult{}, err
	}
	lastModified, _, err := d.store.Meta(ctx, metaLastModified)
	if err != nil {
		return github.NotificationsResult{}, err
	}
	res, err := call(ctx, d.gate, func() (github.NotificationsResult, error) {
		return d.gh.Notifications(ctx, since, lastModified)
	})
	if err != nil || res.NotModified {
		return res, err
	}

	// The next poll starts at the newest notification seen, using GitHub's clock rather than ours.
	// That notification is returned once more, which only costs a fetch without events.
	next := since
	for _, n := range res.Notifications {
		if n.UpdatedAt.After(next) {
			next = n.UpdatedAt
		}
		if err := d.handleNotification(ctx, n); err != nil {
			return res, err
		}
	}
	if err := d.store.SetMeta(ctx, metaNotificationsSince, next.UTC().Format(time.RFC3339Nano)); err != nil {
		return res, err
	}
	if res.LastModified != "" {
		if err := d.store.SetMeta(ctx, metaLastModified, res.LastModified); err != nil {
			return res, err
		}
	}
	return res, nil
}

// notificationsSince returns the persisted poll cursor. The first poll starts now, since the
// backfill covers everything before.
func (d *daemon) notificationsSince(ctx context.Context) (time.Time, error) {
	raw, ok, err := d.store.Meta(ctx, metaNotificationsSince)
	if err != nil {
		return time.Time{}, err
	}
	if ok {
		return time.Parse(time.RFC3339Nano, raw)
	}
	now := time.Now().UTC()
	return now, d.store.SetMeta(ctx, metaNotificationsSince, now.Format(time.RFC3339Nano))
}

// handleNotification enqueues the PR of n. Notifications of watched repos (reason subscribed) only
// refresh PRs that are already tracked. Closed tracked PRs are refreshed too, to detect reopening.
func (d *daemon) handleNotification(ctx context.Context, n github.Notification) error {
	if n.Reason == "subscribed" {
		_, tracked, err := d.store.GetPR(ctx, n.PR)
		if err != nil {
			return err
		}
		if !tracked {
			return nil
		}
	}
	d.queue.push(item{key: n.PR})
	return nil
}

func (d *daemon) reconcileLoop(ctx context.Context) {
	for {
		if err := d.reconcile(ctx); err != nil && ctx.Err() == nil {
			d.log.Error("reconcile", "err", err)
		}
		if !sleep(ctx, d.cfg.ReconcileInterval) {
			return
		}
	}
}

// reconcile enqueues the open PRs whose cheap status differs from the stored snapshot, to catch
// changes that come without a notification (CI, mergeability, merge queue).
func (d *daemon) reconcile(ctx context.Context) error {
	prs, err := d.store.ListPRs(ctx, true)
	if err != nil || len(prs) == 0 {
		return err
	}
	keys := make([]model.PRKey, len(prs))
	for i, pr := range prs {
		keys[i] = pr.Key
	}
	statuses, err := call(ctx, d.gate, func() (map[model.PRKey]model.PRStatus, error) {
		return d.gh.FetchStatuses(ctx, keys)
	})
	if err != nil {
		return err
	}
	changed := 0
	for _, pr := range prs {
		if st, ok := statuses[pr.Key]; ok && !sameStatus(st, pr.PRStatus) {
			d.queue.push(item{key: pr.Key})
			changed++
		}
	}
	d.log.Debug("reconciled", "prs", len(prs), "changed", changed)
	return nil
}

// sameStatus compares statuses by instant rather than by time.Time representation, which differs
// between a stored and a freshly decoded value.
func sameStatus(a, b model.PRStatus) bool {
	a.UpdatedAt = a.UpdatedAt.UTC().Round(0)
	b.UpdatedAt = b.UpdatedAt.UTC().Round(0)
	return a == b
}
