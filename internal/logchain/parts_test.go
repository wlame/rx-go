package logchain

import (
	"slices"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// namesOfParts are the names of parts, in their order.
func namesOfParts(parts []Part) []string {
	names := []string{}
	for _, p := range parts {
		names = append(names, p.Name)
	}
	return names
}

// A description names its parts in the chain's order, those without a
// current index (empty and active ones too), and those the chain waits
// for: the frozen parts with lines and no index. Once these are indexed
// the chain is ready and waits for none.
func TestDescription_PartsUnindexedAndWaiting(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := writeChainFiles(t, t.TempDir(), []chainFile{
		{name: "app.log.3", text: nil},
		{name: "app.log.2.gz", text: timedLines(chainBase, time.Second, 1, 5, "2"), codec: compressedcopy.Gzip},
		{name: "app.log.1", text: timedLines(chainBase.Add(time.Hour), time.Second, 6, 5, "1")},
		{name: "app.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 11, 5, "active")},
	})
	all := []string{"app.log.3", "app.log.2.gz", "app.log.1", "app.log"}

	pending := describe(t, dir, "app.log", Options{})
	checks := []struct {
		label string
		got   []Part
		want  []string
	}{
		{"parts", pending.Parts(), all},
		{"unindexed", pending.UnindexedParts(), all},
		{"waiting", pending.WaitingParts(), []string{"app.log.2.gz", "app.log.1"}},
	}
	for _, c := range checks {
		if got := namesOfParts(c.got); !slices.Equal(got, c.want) {
			t.Errorf("pending %s: %q, want %q", c.label, got, c.want)
		}
	}

	storeIndexes(t, dir, "app.log.2.gz", "app.log.1")
	ready := describe(t, dir, "app.log", Options{})
	if ready.Response.State != "ready" {
		t.Fatalf("state %s after the frozen parts were indexed", ready.Response.State)
	}
	if got := namesOfParts(ready.UnindexedParts()); !slices.Equal(got, []string{"app.log.3", "app.log"}) {
		t.Errorf("ready unindexed: %q, want the empty part and the active file", got)
	}
	if got := ready.WaitingParts(); len(got) != 0 {
		t.Errorf("ready waiting: %q, want none", namesOfParts(got))
	}
}

// An active file compressed as a stream (gzip) has no first timestamp
// without its index, so the chain waits for the active file's index as
// well as for the frozen parts'.
func TestDescription_WaitsForACompressedActiveFile(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := writeChainFiles(t, t.TempDir(), []chainFile{
		{name: "app.log.1", text: timedLines(chainBase, time.Second, 1, 5, "1")},
		{name: "app.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 6, 5, "active"), codec: compressedcopy.Gzip},
	})
	storeIndexes(t, dir, "app.log.1")

	d := describe(t, dir, "app.log", Options{})
	if d.Response.State != "pending" {
		t.Fatalf("state %s, want pending until the active file is indexed", d.Response.State)
	}
	if got := namesOfParts(d.WaitingParts()); !slices.Equal(got, []string{"app.log"}) {
		t.Fatalf("waiting: %q, want the active file", got)
	}
	storeIndexes(t, dir, "app.log")
	if after := describe(t, dir, "app.log", Options{}); after.Response.State != "ready" || len(after.WaitingParts()) != 0 {
		t.Fatalf("after the active file was indexed: %s, waiting %q", after.Response.State, namesOfParts(after.WaitingParts()))
	}
}
