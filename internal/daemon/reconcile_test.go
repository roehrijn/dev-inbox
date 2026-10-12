package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
)

// idleDaemon returns a daemon whose loops do not run, so that a test drives reconcile itself.
func idleDaemon(t *testing.T, dir string, gh GitHub) *daemon {
	t.Helper()
	d, err := start(testConfig(dir), gh)
	if err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(d.close)
	d.setViewer(testViewer)
	return d
}

// drainQueue empties q and returns the keys it held, oldest first.
func drainQueue(q *queue) []model.PRKey {
	q.mu.Lock()
	defer q.mu.Unlock()
	keys := q.order
	q.order = nil
	clear(q.items)
	return keys
}

// updatedAt returns pr with its update time set to baseTime plus offset.
func updatedAt(pr model.PR, offset time.Duration) model.PR {
	pr.UpdatedAt = baseTime.Add(offset)
	return pr
}

func TestReconcileRefetchesNeedsMePRs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	needsMe, own := requestedPR(1), openPR(2, testViewer.Login)
	seed(t, dir, true, needsMe, own)

	gh := newFake(needsMe, own)
	cfg := testConfig(dir)
	cfg.ReconcileInterval = 20 * time.Millisecond
	startDaemon(t, cfg, gh)

	eventually(t, "needs-me PR refetched on several rounds", func() bool { return gh.fetches(needsMe.Key) >= 3 })
	n := len(gh.callsOf("FetchStatuses"))
	eventually(t, "more reconcile rounds", func() bool { return len(gh.callsOf("FetchStatuses")) >= n+3 })
	if got := gh.fetches(own.Key); got != 0 {
		t.Errorf("FetchPR(PR without attention) calls = %d, want 0", got)
	}
}

func TestReconcileQueuesNeedsMePRsEveryRound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	needsMe, own := requestedPR(1), openPR(2, testViewer.Login)
	seed(t, dir, true, needsMe, own)
	d := idleDaemon(t, dir, newFake(needsMe, own))

	for round := 1; round <= 3; round++ {
		if err := d.reconcile(t.Context()); err != nil {
			t.Fatalf("reconcile round %d: %v", round, err)
		}
		if got, want := drainQueue(d.queue), []model.PRKey{needsMe.Key}; !slices.Equal(got, want) {
			t.Errorf("round %d queued %v, want %v", round, got, want)
		}
	}
}

func TestReconcileKeepsTheMostRecentNeedsMePRsOverTheCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const total = needsMeRefetchCap + 5
	var prs []model.PR
	for i := range total {
		prs = append(prs, updatedAt(requestedPR(i+1), time.Duration(i)*time.Minute))
	}
	// PRs without attention neither count against the cap nor get queued, however fresh they are.
	for i := range 3 {
		prs = append(prs, updatedAt(openPR(total+i+1, testViewer.Login), time.Duration(total+i)*time.Minute))
	}
	seed(t, dir, true, prs...)
	d := idleDaemon(t, dir, newFake(prs...))

	if err := d.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var want []model.PRKey
	for i := total; i > total-needsMeRefetchCap; i-- {
		want = append(want, prKey(i))
	}
	got := drainQueue(d.queue)
	if !slices.Equal(got, want) {
		t.Errorf("queued %v, want the %d most recently updated needs-me PRs %v", got, needsMeRefetchCap, want)
	}
}

func TestReconcileDoesNotCountStatusChangesAgainstTheCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const total = needsMeRefetchCap + 1
	var prs []model.PR
	for i := range total {
		prs = append(prs, updatedAt(requestedPR(i+1), time.Duration(i)*time.Minute))
	}
	changed := prs[total-1]
	// A PR that needs no attention and does not change must stay out of the queue.
	quiet := updatedAt(openPR(total+1, testViewer.Login), 0)
	unchanged := append(slices.Clone(prs), quiet)
	seed(t, dir, true, unchanged...)

	// The freshest needs-me PR changes its status, so the status path queues it and the others fit the cap.
	changed.Checks = model.ChecksFailure
	changed.UpdatedAt = changed.UpdatedAt.Add(time.Minute)
	gh := newFake(unchanged...)
	gh.prs[changed.Key] = changed
	d := idleDaemon(t, dir, gh)

	if err := d.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := drainQueue(d.queue)
	if len(got) != total {
		t.Errorf("queued %d PRs, want all %d needs-me PRs", len(got), total)
	}
	if !slices.Contains(got, changed.Key) {
		t.Errorf("status-changed needs-me PR %v not queued", changed.Key)
	}
	if slices.Contains(got, quiet.Key) {
		t.Errorf("queued the PR without attention and without a status change")
	}
}
