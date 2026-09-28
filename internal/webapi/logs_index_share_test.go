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
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// With RX_MAX_INDEX_BUILDS at 1, 258 pending chains each start their
// index task, and every build is held. The part builds of all chains
// together take half of the build queue and no more, however many
// chains wait: a samples lookup of another large file still finds room
// and joins the queue.
func TestChainIndex_ManyChainsLeaveHalfTheQueueToOtherFiles(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	held := func(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
		<-release
		return samples.BuildIndex(path, progress)
	}
	f := newChainIndexFixture(t, 1, held)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	builds := f.server.samplesIndex
	share := builds.maxQueued / 2
	const chains = 258
	for c := 0; c < chains; c++ {
		dir := filepath.Join(f.root, fmt.Sprintf("d%03d", c))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeIndexChain(t, dir, []chainPartFile{{name: "app.log.1"}, {name: "app.log"}}, 2, chainStart)
		describeChainAt(t, f.base, filepath.Join(dir, "app.log"))
	}

	deadline := time.Now().Add(30 * time.Second)
	for unfinishedIndexBuilds(f.manager) < share && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give chain tasks that would take more of the queue the time to do it.
	time.Sleep(100 * time.Millisecond)
	if submitted := unfinishedIndexBuilds(f.manager); submitted != share {
		t.Fatalf("%d part builds submitted by %d chains, want their share of %d", submitted, chains, share)
	}
	other := filepath.Join(f.root, "big.log")
	if err := os.WriteFile(other, []byte("LINE 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	taskID, found := builds.join(other, index.IdentityFromInfo(other, info), info.Size())

	if !found {
		t.Fatalf("a samples build of another file found no room (task %q)", taskID)
	}
	if task, known := f.manager.Get(taskID); !known || task.Status != tasks.StatusQueued || task.Path != other {
		t.Fatalf("the samples build: %+v known=%v, want queued for %s", task, known, other)
	}
}

// More chains pending at once than the task table keeps finished tasks
// (257, every build held): a task another client follows is still known
// after it ends and one more task starts. Unfinished chain tasks are not
// counted against the cap of finished tasks.
func TestChainIndex_ManyPendingChainsLeaveAFollowedTaskInTheTable(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	held := func(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
		<-release
		return samples.BuildIndex(path, progress)
	}
	f := newChainIndexFixture(t, 1, held)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	const chains = tasks.DefaultMaxTasks + 1
	for c := 0; c < chains; c++ {
		dir := filepath.Join(f.root, fmt.Sprintf("d%03d", c))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeIndexChain(t, dir, []chainPartFile{{name: "app.log.1"}, {name: "app.log"}}, 2, chainStart)
		describeChainAt(t, f.base, filepath.Join(dir, "app.log"))
	}
	followed, _ := f.manager.Create(filepath.Join(f.root, "followed.log"), "compress")
	f.manager.Complete(followed.TaskID, nil)

	other, _ := f.manager.Create(filepath.Join(f.root, "other.log"), "compress")
	f.manager.Complete(other.TaskID, nil)

	if _, known := f.manager.Get(followed.TaskID); !known {
		t.Fatalf("after %d pending chains, one more task dropped the finished task a client follows (table of %d)",
			chains, f.manager.Size())
	}
}
