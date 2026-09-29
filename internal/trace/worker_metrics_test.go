package trace

import (
	"context"
	"testing"

	"github.com/wlame/rx-go/internal/prometheus"
)

// counterValue reads an unlabeled counter family from the registry.
func counterValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) == 1 {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// A max_results cap cancels the chunks still running once it is met.
// They stopped because they were told to, not because they failed, so
// they count as neither completed nor failed.
func TestCappedTraceCountsNoFailedWorkerTask(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)
	path, _ := writeChunkedFixture(t, "capped.log", 16<<20)

	failedBefore := counterValue(t, "rx_worker_tasks_failed_total")
	completedBefore := counterValue(t, "rx_worker_tasks_completed_total")
	limit := 1
	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"line"},
		Options{MaxResults: &limit, NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.Matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(resp.Matches))
	}

	if failed := counterValue(t, "rx_worker_tasks_failed_total") - failedBefore; failed != 0 {
		t.Errorf("rx_worker_tasks_failed_total rose by %v for a capped trace", failed)
	}
	completed := counterValue(t, "rx_worker_tasks_completed_total") - completedBefore
	if completed >= float64(resp.FileChunks["f1"]) {
		t.Errorf("%v of %d chunks counted as completed; the cap canceled none, so the test proves nothing",
			completed, resp.FileChunks["f1"])
	}
}
