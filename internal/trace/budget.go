package trace

import (
	"context"
	"sync"
	"sync/atomic"
)

// MatchBudget is the shared match allowance for one scan of one file.
//
// Every worker charges the budget as soon as ripgrep reports a match,
// not when the worker finishes, so the cap can stop a scan in the
// middle of a chunk. The first charge that exhausts the allowance
// cancels the context every sibling worker runs under; ripgrep sees the
// cancellation through exec.CommandContext and exits within
// milliseconds.
//
// A nil *MatchBudget means "no cap" and every method is a no-op, so
// callers do not need a branch around the charge.
//
// Overshoot is expected and harmless. A worker charges once per
// ripgrep match event, while one event can become several matches when
// several patterns hit the same line, so the counter runs at or below
// the true match count. Undercounting cancels a little late, never too
// early, and the engine truncates to the exact cap after sorting.
type MatchBudget struct {
	limit  int64
	spent  atomic.Int64
	cancel context.CancelFunc
	once   sync.Once
}

// NewMatchBudget returns a budget of limit matches that calls cancel
// once the limit is reached. A limit of zero or less means unlimited
// and yields a nil budget, which every method tolerates.
func NewMatchBudget(limit int, cancel context.CancelFunc) *MatchBudget {
	if limit <= 0 {
		return nil
	}
	return &MatchBudget{limit: int64(limit), cancel: cancel}
}

// Charge records one match against the budget and reports whether the
// budget is now exhausted. The cancel function fires exactly once, on
// the charge that crosses the limit.
func (b *MatchBudget) Charge() bool {
	if b == nil {
		return false
	}
	if b.spent.Add(1) < b.limit {
		return false
	}
	b.once.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
	})
	return true
}

// Exhausted reports whether the budget has been spent. Callers use it
// to tell a cancellation they asked for from one imposed on them.
func (b *MatchBudget) Exhausted() bool {
	return b != nil && b.spent.Load() >= b.limit
}
