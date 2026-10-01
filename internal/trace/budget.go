package trace

import (
	"context"
	"sync"
	"sync/atomic"
)

// MatchBudget is the shared match allowance for one scan of one file.
//
// Every worker charges the budget as soon as ripgrep has reported a
// match and the lines of its trailing context (trailingWindowGate), not
// when the worker finishes, so the cap can stop a scan in the middle of
// a chunk. The first charge that exhausts the allowance
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

// trailingWindowGate charges a worker's matches against its MatchBudget
// only once ripgrep has reported the lines after each match that its
// window asks for (ContextAfter of them), or its input has ended.
//
// The first charge that spends the budget cancels every worker, this
// one included, and ripgrep writes a match's trailing context after the
// match itself. Charging at the match would kill ripgrep before it wrote
// those lines, and the last match kept would lose the end of its window.
// Charging at the window's end keeps every counted match whole; a match
// still waiting when the cancel lands was never counted, and the worker
// reports it as a line of the windows around it instead (see
// matchAsContext).
//
// Line numbers are ripgrep's own, within one ripgrep run. A gate belongs
// to the goroutine that reads that run's events.
type trailingWindowGate struct {
	budget *MatchBudget
	after  int
	// pending holds the line numbers of the matches not charged yet, in
	// the order ripgrep reported them. They are always the latest
	// matches the worker collected.
	pending []int
}

// newTrailingWindowGate returns a gate for windows of after lines past
// each match, charging budget. A nil budget makes every charge a no-op.
func newTrailingWindowGate(budget *MatchBudget, after int) *trailingWindowGate {
	return &trailingWindowGate{budget: budget, after: after}
}

// matched records a match on line, then charges what that line
// completes. With no trailing window the match is charged at once.
func (g *trailingWindowGate) matched(line int) {
	g.pending = append(g.pending, line)
	g.reached(line)
}

// reached records that ripgrep reported line, and charges every pending
// match whose window ends at or before it.
func (g *trailingWindowGate) reached(line int) {
	done := 0
	for done < len(g.pending) && g.pending[done]+g.after <= line {
		g.budget.Charge()
		done++
	}
	g.pending = g.pending[done:]
}

// inputEnded charges every pending match: ripgrep read all of its
// input, so no window can grow any further.
func (g *trailingWindowGate) inputEnded() {
	for range g.pending {
		g.budget.Charge()
	}
	g.pending = nil
}

// uncounted is how many of the latest matches were never charged
// because the run stopped before their windows were read.
func (g *trailingWindowGate) uncounted() int {
	return len(g.pending)
}
