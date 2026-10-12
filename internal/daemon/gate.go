package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/roehrijn/dev-inbox/internal/github"
)

const (
	// defaultRateLimitPause applies when GitHub does not say when the limit resets.
	defaultRateLimitPause = time.Minute
	// minRateLimitPause keeps clock skew from turning a reset in the past into a retry storm.
	minRateLimitPause = time.Second
)

// gate pauses all GitHub calls of the daemon while a rate limit is exhausted.
type gate struct {
	log *slog.Logger

	mu    sync.Mutex
	until time.Time
}

func (g *gate) pause(reset time.Time) {
	now := time.Now()
	until := reset
	if until.IsZero() {
		until = now.Add(defaultRateLimitPause)
	}
	if earliest := now.Add(minRateLimitPause); until.Before(earliest) {
		until = earliest
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if until.After(g.until) {
		g.until = until
		g.log.Warn("github rate limit exceeded, pausing", "until", until.Format(time.RFC3339))
	}
}

// wait blocks until the gate is open or ctx ends.
func (g *gate) wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		d := time.Until(g.until)
		g.mu.Unlock()
		if d <= 0 {
			return nil
		}
		if !sleep(ctx, d) {
			return ctx.Err()
		}
	}
}

// call runs fn once the gate is open and retries it after a rate limit error has paused the gate.
func call[T any](ctx context.Context, g *gate, fn func() (T, error)) (T, error) {
	for {
		if err := g.wait(ctx); err != nil {
			var zero T
			return zero, err
		}
		v, err := fn()
		var rl *github.RateLimitError
		if !errors.As(err, &rl) {
			return v, err
		}
		g.pause(rl.Reset)
	}
}
