package findings

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// With one scan at a time, a scan of another file waits until the
// running one ends; a scan answered from the cache does not wait; a
// scan canceled while it waits gives its place back.
func TestScan_QueueMakesASecondFileWait(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	dir := t.TempDir()
	fileA := classify(t, writeLog(t, dir, "a.log", sampleLog()))
	fileB := classify(t, writeLog(t, dir, "b.log", sampleLog()))
	opts := testOptions(t)
	s, rec := newRecordedScheduler(1)
	mustScan(t, s, fileB, cat, pick(t, cat, "blank-lines"), opts)

	aStarted, releaseA := make(chan struct{}), make(chan struct{})
	var aReleased atomic.Bool
	rec.hold = func(ids []string) {
		switch ids[0] {
		case "err-line":
			close(aStarted)
			<-releaseA
		case "warn-line":
			if !aReleased.Load() {
				t.Error("the scan of b.log ran while the scan of a.log held the only place")
			}
		}
	}
	doneA := make(chan error, 1)
	go func() {
		_, err := s.scan(context.Background(), fileA, cat, pick(t, cat, "err-line"), opts)
		doneA <- err
	}()
	<-aStarted

	// A scan answered from the cache needs no place: it must not wait.
	ctx, cancel := context.WithCancel(context.Background())
	s.slots.setOnWait(func() {
		t.Error("a scan answered from the cache waited in the queue")
		cancel()
	})
	if _, err := s.scan(ctx, fileB, cat, pick(t, cat, "blank-lines"), opts); err != nil {
		t.Fatal(err)
	}
	cancel()

	// A scan canceled while it waits gives up its turn.
	waitCtx, cancelWait := context.WithCancel(context.Background())
	s.slots.setOnWait(cancelWait)
	if _, err := s.scan(waitCtx, fileB, cat, pick(t, cat, "py-trace"), opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled wait gave %v", err)
	}

	waiting := make(chan struct{})
	s.slots.setOnWait(func() { close(waiting) })
	doneB := make(chan error, 1)
	go func() {
		_, err := s.scan(context.Background(), fileB, cat, pick(t, cat, "warn-line"), opts)
		doneB <- err
	}()
	<-waiting
	aReleased.Store(true)
	close(releaseA)
	if err := errors.Join(<-doneA, <-doneB); err != nil {
		t.Fatal(err)
	}
	read, err := s.read(fileB, cat, pick(t, cat, "py-trace"), opts)
	if err != nil || len(read.Checked) != 0 {
		t.Errorf("the canceled scan wrote: %+v (%v)", read, err)
	}
}

// The limit can grow while scans wait: the waiters it lets in start.
func TestSlotLimiter_GrowingTheLimitLetsWaitersIn(t *testing.T) {
	l := newSlotLimiter(1)
	release, err := l.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	l.setOnWait(func() { close(waiting) })
	got := make(chan func(), 1)
	go func() {
		r, err := l.acquire(context.Background())
		if err != nil {
			t.Error(err)
		}
		got <- r
	}()
	<-waiting
	l.setLimit(2)
	second := <-got
	second()
	release()
	if l.running != 0 || len(l.waiters) != 0 {
		t.Errorf("running %d, waiters %d", l.running, len(l.waiters))
	}
}

// SetMaxScans keeps the limit within 1 to 64.
func TestSetMaxScans_Bounds(t *testing.T) {
	t.Cleanup(func() { SetMaxScans(1) })
	for _, tc := range []struct{ give, want int }{{0, 1}, {-3, 1}, {5, 5}, {64, 64}, {1000, 64}} {
		SetMaxScans(tc.give)
		if got := defaultScheduler.slots.limitNow(); got != tc.want {
			t.Errorf("SetMaxScans(%d): limit %d, want %d", tc.give, got, tc.want)
		}
	}
}
