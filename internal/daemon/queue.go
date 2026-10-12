package daemon

import (
	"context"
	"sync"

	"github.com/roehrijn/dev-inbox/internal/model"
)

type item struct {
	key model.PRKey
	// baseline items only emit pr.discovered, so backfill does not flood consumers with history.
	baseline bool
	attempts int
	// done callbacks run once when the item is finished, ingested or given up.
	done []func()
}

func (it item) finish() {
	for _, fn := range it.done {
		fn()
	}
}

// queue is a FIFO of PRs to ingest that holds each PR at most once.
type queue struct {
	mu    sync.Mutex
	order []model.PRKey
	items map[model.PRKey]*item
	ready chan struct{}
}

func newQueue() *queue {
	return &queue{items: map[model.PRKey]*item{}, ready: make(chan struct{}, 1)}
}

// push enqueues it or merges it into the pending item of the same PR. A merged item is only a
// baseline item when both are.
func (q *queue) push(it item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if cur, ok := q.items[it.key]; ok {
		cur.baseline = cur.baseline && it.baseline
		cur.attempts = min(cur.attempts, it.attempts)
		cur.done = append(cur.done, it.done...)
		return
	}
	q.items[it.key] = &it
	q.order = append(q.order, it.key)
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// pop returns the oldest item, waiting for one until ctx ends. It returns nothing once ctx ended,
// even when items are pending.
func (q *queue) pop(ctx context.Context) (item, bool) {
	for {
		if ctx.Err() != nil {
			return item{}, false
		}
		q.mu.Lock()
		if len(q.order) > 0 {
			key := q.order[0]
			q.order = q.order[1:]
			it := *q.items[key]
			delete(q.items, key)
			q.mu.Unlock()
			return it, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return item{}, false
		case <-q.ready:
		}
	}
}
