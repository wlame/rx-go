package findings

import (
	"context"
	"slices"
	"sync"
)

// maxScansLimit bounds SetMaxScans, as RX_MAX_SIGNATURE_SCANS is
// bounded: each scan runs its own workers over a whole file.
const maxScansLimit = 64

// slotLimiter lets at most limit scans run at once; the others wait in
// a queue, first come first served. The limit can change while scans
// run or wait.
//
// Waiting is done on a channel per waiter, not on a sync.Mutex or a
// sync.Cond, so a waiter can stop when its context is canceled (select
// on both); a waiter that leaves takes its channel out of the queue.
type slotLimiter struct {
	mu      sync.Mutex
	limit   int
	running int
	// waiters are the waiting callers' channels, oldest first. A waiter
	// is let in by closing its channel (it then counts as running).
	waiters []chan struct{}
	// onWait, when set, is called each time a caller starts to wait
	// (tests). It runs outside the mutex.
	onWait func()
}

// newSlotLimiter returns a limiter of n slots (clamped to 1–64).
func newSlotLimiter(n int) *slotLimiter {
	return &slotLimiter{limit: clampScans(n)}
}

// clampScans keeps a scan limit within 1 and maxScansLimit.
func clampScans(n int) int {
	return min(max(n, 1), maxScansLimit)
}

// acquire waits for a slot and returns the function that gives it back,
// which the caller must call exactly once. It returns ctx.Err() and no
// slot when ctx is canceled first.
func (l *slotLimiter) acquire(ctx context.Context) (func(), error) {
	l.mu.Lock()
	if l.running < l.limit && len(l.waiters) == 0 {
		l.running++
		l.mu.Unlock()
		return l.release, nil
	}
	ch := make(chan struct{})
	l.waiters = append(l.waiters, ch)
	onWait := l.onWait
	l.mu.Unlock()
	if onWait != nil {
		onWait()
	}
	// Go note: select waits for whichever happens first: the slot (its
	// channel closed by release or setLimit) or the cancel.
	select {
	case <-ch:
		return l.release, nil
	case <-ctx.Done():
		l.mu.Lock()
		defer l.mu.Unlock()
		if i := slices.Index(l.waiters, ch); i >= 0 {
			l.waiters = slices.Delete(l.waiters, i, i+1)
			return nil, ctx.Err()
		}
		// The slot was given in the same instant as the cancel: give it
		// back so the next waiter gets it.
		l.releaseLocked()
		return nil, ctx.Err()
	}
}

// release gives a slot back and lets the oldest waiters in.
func (l *slotLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseLocked()
}

// releaseLocked is release with l.mu held.
func (l *slotLimiter) releaseLocked() {
	l.running--
	l.admitLocked()
}

// admitLocked lets waiters in while slots are free, oldest first.
func (l *slotLimiter) admitLocked() {
	for l.running < l.limit && len(l.waiters) > 0 {
		ch := l.waiters[0]
		l.waiters = l.waiters[1:]
		l.running++
		close(ch)
	}
}

// setLimit changes the number of slots (clamped to 1–64). A larger
// limit lets waiters in at once; a smaller one lets running scans end
// and admits no one until fewer run than the new limit.
func (l *slotLimiter) setLimit(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = clampScans(n)
	l.admitLocked()
}

// limitNow returns the current number of slots.
func (l *slotLimiter) limitNow() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// setOnWait sets the hook called when a caller starts to wait (tests).
func (l *slotLimiter) setOnWait(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onWait = f
}

// fileLocks holds one lock per cache directory, so one scheduler runs
// one scan of a file at a time. The table keeps an entry only while a
// caller holds or waits for it, so it holds at most one entry per
// concurrent caller.
type fileLocks struct {
	mu   sync.Mutex
	held map[string]*fileLock
	// onWait, when set, is called each time a caller finds the lock
	// taken and starts to wait (tests). It runs outside the mutex.
	onWait func()
}

// fileLock is one file's lock. Its channel has room for one token: the
// holder is the caller whose send went in, and taking the token out
// releases it. A channel, unlike a sync.Mutex, lets a waiter give up
// when its context is canceled.
type fileLock struct {
	token chan struct{}
	// refs counts the callers holding or waiting for the lock; the entry
	// leaves the table when it drops to 0.
	refs int
}

// lock waits for the lock of key and returns the function that releases
// it, or ctx.Err() when ctx is canceled first.
func (f *fileLocks) lock(ctx context.Context, key string) (func(), error) {
	f.mu.Lock()
	if f.held == nil {
		f.held = map[string]*fileLock{}
	}
	fl := f.held[key]
	if fl == nil {
		fl = &fileLock{token: make(chan struct{}, 1)}
		f.held[key] = fl
	}
	fl.refs++
	onWait := f.onWait
	f.mu.Unlock()

	// Go note: a send on a channel with free room never blocks, so an
	// unheld lock is taken at once; select with a default tries that
	// first, and only a taken lock reaches the waiting select below.
	select {
	case fl.token <- struct{}{}:
		return func() { f.unlock(key, fl) }, nil
	default:
	}
	if onWait != nil {
		onWait()
	}
	select {
	case fl.token <- struct{}{}:
		return func() { f.unlock(key, fl) }, nil
	case <-ctx.Done():
		f.drop(key, fl)
		return nil, ctx.Err()
	}
}

// unlock releases a held lock.
func (f *fileLocks) unlock(key string, fl *fileLock) {
	<-fl.token
	f.drop(key, fl)
}

// drop forgets one caller of fl, and the entry once no caller is left.
func (f *fileLocks) drop(key string, fl *fileLock) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl.refs--
	if fl.refs == 0 {
		delete(f.held, key)
	}
}
