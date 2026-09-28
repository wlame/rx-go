package webapi

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainPartJoinsOf reads how many times the index tasks of log chains
// have asked builds for a part's build.
func chainPartJoinsOf(builds *samplesIndexBuilds) int {
	builds.mu.Lock()
	defer builds.mu.Unlock()
	return builds.chainPartJoins
}

// awaitChainPartJoins waits until the index tasks of log chains have
// asked for a part's build n times in all.
func awaitChainPartJoins(t *testing.T, builds *samplesIndexBuilds, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for chainPartJoinsOf(builds) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the chains' tasks asked for a part's build %d times, want %d", chainPartJoinsOf(builds), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Eight pending chains wait for room in a build queue that other files'
// builds fill (RX_MAX_INDEX_BUILDS at 1, every build held). The running
// build's end frees one place, and wakes one chain's task to take it:
// the waiting tasks ask for a part's build once in all, not once each.
func TestChainIndex_ABuildsEndWakesOneWaitingChain(t *testing.T) {
	const waiting = 8
	release := make(chan struct{})
	var once sync.Once
	held := func(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
		<-release
		return samples.BuildIndex(path, progress)
	}
	f := newChainIndexFixture(t, 1, held)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	builds := f.server.samplesIndex
	builds.maxQueued = 2 * waiting
	// One build running and a full queue behind it.
	for i := 0; i <= builds.maxQueued; i++ {
		path := filepath.Join(f.root, fmt.Sprintf("other%02d.log.gz", i))
		writeGzipLog(t, path, "LINE")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if builds.start(path, info) == nil {
			t.Fatalf("no build of %s", path)
		}
	}
	for c := 0; c < waiting; c++ {
		dir := filepath.Join(f.root, fmt.Sprintf("d%d", c))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeIndexChain(t, dir, []chainPartFile{{name: "app.log.1"}, {name: "app.log"}}, 2, chainStart)
		describeChainAt(t, f.base, filepath.Join(dir, "app.log"))
	}
	awaitChainPartJoins(t, builds, waiting)
	// Each task has been refused once; give each the time to start waiting.
	time.Sleep(50 * time.Millisecond)
	before := chainPartJoinsOf(builds)

	// The running build takes the release and ends.
	release <- struct{}{}
	awaitChainPartJoins(t, builds, before+1)
	// A task woken with the first would ask within this time.
	time.Sleep(100 * time.Millisecond)

	if asked := chainPartJoinsOf(builds) - before; asked != 1 {
		t.Fatalf("one build's end made the waiting chains ask for a part's build %d times, want once", asked)
	}
}

// chainWaitersOf reads how many index tasks of log chains wait in line
// for room.
func chainWaitersOf(builds *samplesIndexBuilds) int {
	builds.mu.Lock()
	defer builds.mu.Unlock()
	return len(builds.chainWaiters)
}

// awaitChainWaiters waits until n index tasks of log chains wait in line.
func awaitChainWaiters(t *testing.T, builds *samplesIndexBuilds, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for chainWaitersOf(builds) != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d chains wait in line, want %d", chainWaitersOf(builds), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A chain's task woken for room its part does not need (a compression
// holds the part by then) passes the wake-up on: the next chain in line
// builds its part, although no other build is left to end and wake it.
func TestChainIndex_AWakeUpThePartDoesNotNeedGoesToTheNextChain(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	started := make(chan string, 8)
	held := func(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
		started <- filepath.Base(filepath.Dir(path))
		<-release
		return samples.BuildIndex(path, progress)
	}
	f := newChainIndexFixture(t, 1, held)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	builds := f.server.samplesIndex
	// The chains' part builds take at most one place.
	builds.maxQueued = 2
	handle := func(chain string) string { return filepath.Join(f.root, chain, "app.log") }
	for _, chain := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(f.root, chain), 0o700); err != nil {
			t.Fatal(err)
		}
		writeIndexChain(t, filepath.Join(f.root, chain), []chainPartFile{{name: "app.log.1"}, {name: "app.log"}}, 2, chainStart)
	}
	describeChainAt(t, f.base, handle("a"))
	if got := <-started; got != "a" {
		t.Fatalf("the first build is chain %s's, want a's", got)
	}
	describeChainAt(t, f.base, handle("b"))
	awaitChainWaiters(t, builds, 1)
	describeChainAt(t, f.base, handle("c"))
	awaitChainWaiters(t, builds, 2)
	compression, _, _ := f.manager.CreateHolding("compress", filepath.Join(f.root, "b", "app.log.1"))
	t.Cleanup(func() { f.manager.Fail(compression.TaskID, "released by the test") })

	// Chain a's build ends: b is woken first and joins the compression.
	release <- struct{}{}

	select {
	case got := <-started:
		if got != "c" {
			t.Fatalf("the next build is chain %s's, want c's", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("chain c's part was never built; %d chains still wait in line", chainWaitersOf(builds))
	}
}
