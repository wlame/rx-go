package webapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// askLinesOf asks the gzip log at path for lines with one line of
// context and returns the 200 answer.
func (f *samplesBuildFixture) askLinesOf(t *testing.T, path, lines string) rxtypes.SamplesResponse {
	t.Helper()
	query := url.Values{"path": {path}, "lines": {lines}, "context": {"1"}}
	resp, err := http.Get(f.base + "/v1/samples?" + query.Encode())
	if err != nil {
		t.Fatalf("get samples: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return decodeSamples(t, resp.StatusCode, body)
}

// Builds that samples lookups start beyond RX_MAX_INDEX_BUILDS wait in
// a queue: their tasks stay queued, lookups in the head of their files
// are still answered at once and name them, and each starts, in the
// order it was queued, when a running build ends.
func TestSamples_LimitsHowManyIndexBuildsRunAtOnce(t *testing.T) {
	f := newLimitedBuildFixture(t, 20*time.Millisecond, 2)
	t.Setenv("RX_SAMPLES_HEAD_MB", "1")
	logs := make([]string, 5)
	taskIDs := make([]string, len(logs))
	for i := range logs {
		logs[i] = filepath.Join(f.root, fmt.Sprintf("app%d.log.gz", i))
		writeGzipLog(t, logs[i], "LINE")
		answer := f.askLinesOf(t, logs[i], "5")
		if answer.IndexBuild == nil {
			t.Fatalf("%s: index_build is null, want the build's task", logs[i])
		}
		taskIDs[i] = answer.IndexBuild.TaskID
		if i >= 2 && answer.IndexBuild.Status != string(tasks.StatusQueued) {
			t.Fatalf("%s: index_build status %q, want queued", logs[i], answer.IndexBuild.Status)
		}
	}
	f.gate.awaitStart(t)
	f.gate.awaitStart(t)
	for i, id := range taskIDs {
		want := string(tasks.StatusRunning)
		if i >= 2 {
			want = string(tasks.StatusQueued)
		}
		if got := f.taskStatus(t, id).Status; got != want {
			t.Fatalf("task of %s is %q, want %q", logs[i], got, want)
		}
	}
	if again := f.askLinesOf(t, logs[4], "7"); again.IndexBuild == nil || again.IndexBuild.TaskID != taskIDs[4] ||
		again.IndexBuild.Status != string(tasks.StatusQueued) {
		t.Fatalf("a second lookup in a queued file names %+v, want queued task %s", again.IndexBuild, taskIDs[4])
	}

	// Each build that ends lets exactly the next queued one start.
	for i := 2; i < len(logs); i++ {
		f.gate.releaseOne(t)
		f.gate.awaitStart(t)
		if started := f.gate.startedPaths(); started[len(started)-1] != logs[i] {
			t.Fatalf("builds started for %q, want %s next", started, logs[i])
		}
	}
	f.gate.open()
	for i, id := range taskIDs {
		if finished := f.awaitTask(t, id); finished.Status != string(tasks.StatusCompleted) {
			t.Fatalf("task of %s ended %q: %v", logs[i], finished.Status, finished.Error)
		}
	}
	if most := f.gate.mostAtOnce.Load(); most > 2 {
		t.Fatalf("%d builds ran at once, want at most 2", most)
	}
}

// The queue of builds waiting for a slot has a cap: past it a lookup
// starts no build and creates no task (an answer from the head names
// none), and once every build has ended, nothing of them is kept, the
// head reaches included.
func TestSamplesIndexBuilds_QueueAndRegistryStayBounded(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	gate := newBuildGate(t)
	manager := tasks.New(tasks.Config{})
	builds := newSamplesIndexBuilds(manager, nil, gate.build, 1)
	builds.maxQueued = 2

	dir := t.TempDir()
	var started []*rxtypes.SamplesIndexBuild
	for i := range 4 {
		path := filepath.Join(dir, fmt.Sprintf("app%d.log.gz", i))
		writeGzipLog(t, path, "LINE")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		started = append(started, builds.start(path, info))
	}
	for i, build := range started[:3] {
		if build == nil {
			t.Fatalf("file %d: no build, want one running or queued", i)
		}
	}
	if started[3] != nil {
		t.Fatalf("file 3: build %+v past the queue's cap, want none", *started[3])
	}
	if size := manager.Size(); size != 3 {
		t.Fatalf("%d tasks, want 3", size)
	}

	gate.open()
	for _, build := range started[:3] {
		done, _ := manager.Done(build.TaskID)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("task %s did not end", build.TaskID)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		builds.mu.Lock()
		running, queued, active := len(builds.running), len(builds.queue), builds.active
		builds.mu.Unlock()
		if running == 0 && queued == 0 && active == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("after every build: %d running entries, %d queued, %d slots taken; want none", running, queued, active)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
