package webapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/tasks"
)

// familiesAllowedToStayStill names the rx_* families that one pass of
// every operation cannot be seen to move, each with the reason. Every
// other registered rx_* family must change during the pass: a family
// nothing updates is a dashboard panel that lies.
var familiesAllowedToStayStill = map[string]string{
	"rx_active_workers": "a live gauge that returns to 0 once the scans " +
		"end, so a snapshot taken after them equals the one taken before",
	"rx_large_file_threshold_mb": "set once by Server.Start, which an " +
		"httptest server does not call",
}

// metricSnapshot maps a family name to its series, keyed by their label
// pairs, with one number per series: a counter's or a gauge's value, or
// a histogram's sample count.
type metricSnapshot map[string]map[string]float64

// snapshotMetrics gathers the rx registry into a metricSnapshot.
func snapshotMetrics(t *testing.T) metricSnapshot {
	t.Helper()
	families, err := prometheus.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	snap := metricSnapshot{}
	for _, family := range families {
		series := map[string]float64{}
		for _, m := range family.GetMetric() {
			series[seriesKey(m)] = seriesValue(m)
		}
		snap[family.GetName()] = series
	}
	return snap
}

// seriesKey renders a series' labels as name=value pairs in a fixed order.
func seriesKey(m *dto.Metric) string {
	pairs := make([]string, 0, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		pairs = append(pairs, l.GetName()+"="+l.GetValue())
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// seriesValue reduces a series to the one number that moves when the
// series is updated.
func seriesValue(m *dto.Metric) float64 {
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	}
	return 0
}

// moved reports whether any series of family is new in after or has a
// different value than in before.
func (before metricSnapshot) moved(after metricSnapshot, family string) bool {
	for key, value := range after[family] {
		if old, ok := before[family][key]; !ok || old != value {
			return true
		}
	}
	return false
}

// metricsFixture is a sandbox with the files the operations below need:
// a plain log above the 1 MB large-file threshold the test sets, so it
// is chunked, cached and indexed; a small log; a directory holding a
// binary file, which a trace of the directory skips; and a .gz file
// whose data is damaged.
type metricsFixture struct {
	root, bigLog, smallLog, mixedDir, brokenGz string
}

func newMetricsFixture(t *testing.T) metricsFixture {
	t.Helper()
	root := t.TempDir()
	f := metricsFixture{
		root:     root,
		bigLog:   filepath.Join(root, "big.log"),
		smallLog: filepath.Join(root, "small.log"),
		mixedDir: filepath.Join(root, "mixed"),
		brokenGz: filepath.Join(root, "broken.log.gz"),
	}
	var big bytes.Buffer
	for i := 1; big.Len() < 2*1024*1024; i++ {
		level := "INFO"
		if i%100 == 0 {
			level = "ERROR"
		}
		fmt.Fprintf(&big, "LINE %d %s request handled\n", i, level)
	}
	writeFixture(t, f.bigLog, big.Bytes())
	writeFixture(t, f.smallLog, []byte("LINE 1 ERROR one\nLINE 2 INFO two\n"))
	// A gzip header over data that is not deflate: its scan fails in
	// the worker.
	writeFixture(t, f.brokenGz, append([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3}, "LINE 1 ERROR one\n"...))
	if err := os.Mkdir(f.mixedDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFixture(t, filepath.Join(f.mixedDir, "text.log"), []byte("LINE 1 ERROR one\n"))
	writeFixture(t, filepath.Join(f.mixedDir, "blob.bin"), []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 1})
	return f
}

func writeFixture(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// metricsServer starts a server wired the way `rx serve` wires it, with
// a hook dispatcher. drain delivers every queued webhook before return.
func metricsServer(t *testing.T) (ts *httptest.Server, drain func()) {
	t.Helper()
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Skipf("ripgrep not installed: %v", err)
	}
	dispatcher := hooks.NewDispatcher(hooks.DispatcherConfig{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	drained := false
	drain = func() {
		if !drained {
			drained = true
			dispatcher.Close()
			dispatcher.Wait()
		}
	}
	t.Cleanup(drain)
	ts = httptest.NewServer(NewServer(Config{
		AppVersion:  "metrics-test",
		RipgrepPath: rgPath,
		TaskManager: tasks.New(tasks.Config{}),
		Hooks:       dispatcher,
	}))
	t.Cleanup(ts.Close)
	return ts, drain
}

// mustGet issues a GET and returns the status code.
func mustGet(t *testing.T, rawURL string) int {
	t.Helper()
	resp, err := http.Get(rawURL)
	if err != nil {
		t.Fatalf("get %s: %v", rawURL, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestMetrics_EveryFamilyMovesWhenEveryOperationRunsOnce drives each
// operation `serve` offers once — traces that miss, write and hit the
// trace cache, a capped trace with a webhook, a trace that skips a
// binary file, an invalid pattern, a file whose scan fails, an
// analyzing index build, an index
// read and a samples request — and checks that every registered rx_*
// family changed, except the ones familiesAllowedToStayStill excuses.
func TestMetrics_EveryFamilyMovesWhenEveryOperationRunsOnce(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true")
	for _, name := range []string{"RX_HOOK_ON_FILE_URL", "RX_HOOK_ON_MATCH_URL", "RX_HOOK_ON_COMPLETE_URL", "RX_NO_INDEX"} {
		t.Setenv(name, "")
	}
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)

	f := newMetricsFixture(t)
	if err := paths.SetSearchRoots([]string{f.root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts, drain := metricsServer(t)
	receiver := newWebhookReceiver(t)

	before := snapshotMetrics(t)

	traceURL := func(path, pattern string, extra url.Values) string {
		q := url.Values{"path": {path}, "regexp": {pattern}}
		for k, vs := range extra {
			q[k] = vs
		}
		return ts.URL + "/v1/trace?" + q.Encode()
	}
	steps := []struct {
		name string
		url  string
		want int
	}{
		{"trace that writes the cache", traceURL(f.bigLog, "ERROR", nil), http.StatusOK},
		{"trace answered from the cache", traceURL(f.bigLog, "ERROR", nil), http.StatusOK},
		{"capped trace with a webhook", traceURL(f.bigLog, "ERROR", url.Values{
			"max_results": {"1"}, "hook_on_complete": {receiver.server.URL},
		}), http.StatusOK},
		{"trace that skips a binary file", traceURL(f.mixedDir, "ERROR", nil), http.StatusOK},
		{"trace of an invalid pattern", traceURL(f.smallLog, "(unclosed", nil), http.StatusBadRequest},
		{"trace of a file whose scan fails", traceURL(f.brokenGz, "ERROR", nil), http.StatusOK},
	}
	for _, step := range steps {
		if got := mustGet(t, step.url); got != step.want {
			t.Fatalf("%s: status %d, want %d", step.name, got, step.want)
		}
	}

	body, _ := json.Marshal(map[string]any{"path": f.bigLog, "analyze": true})
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post /v1/index: %v", err)
	}
	var task struct {
		TaskID string `json:"task_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&task)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || task.TaskID == "" {
		t.Fatalf("post /v1/index: status %d, task %q", resp.StatusCode, task.TaskID)
	}
	waitForTaskCompletion(t, ts.URL, task.TaskID)

	if got := mustGet(t, ts.URL+"/v1/index?"+url.Values{"path": {f.bigLog}}.Encode()); got != http.StatusOK {
		t.Fatalf("get /v1/index: status %d", got)
	}
	samplesQuery := url.Values{"path": {f.bigLog}, "lines": {"10,20"}, "context": {"2"}}
	if got := mustGet(t, ts.URL+"/v1/samples?"+samplesQuery.Encode()); got != http.StatusOK {
		t.Fatalf("get /v1/samples: status %d", got)
	}
	drain()

	// FamilyNames rather than the snapshot: Gather leaves out a labeled
	// family until its first series exists, so a family nothing ever
	// updates would be missing from both snapshots and pass unseen.
	after := snapshotMetrics(t)
	for _, family := range prometheus.FamilyNames() {
		reason, excused := familiesAllowedToStayStill[family]
		moved := before.moved(after, family)
		switch {
		case !moved && !excused:
			t.Errorf("%s did not change although every operation ran once", family)
		case moved && excused:
			t.Errorf("%s changed, so its allow-list entry (%q) is stale", family, reason)
		}
	}
	declared := map[string]bool{}
	for _, family := range prometheus.FamilyNames() {
		declared[family] = true
	}
	for family := range familiesAllowedToStayStill {
		if !declared[family] {
			t.Errorf("allow-listed family %s is not declared", family)
		}
	}
}

// A directory listing reports whether each file has an index without
// using one, so it must not move the index cache counters: listing a
// directory of a thousand files is not a thousand cache misses.
func TestMetrics_TreeListingIsNotAnIndexLookup(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	prometheus.Enable()
	t.Cleanup(prometheus.Disable)
	f := newMetricsFixture(t)
	if err := paths.SetSearchRoots([]string{f.root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	before := snapshotMetrics(t)
	if got := mustGet(t, ts.URL+"/v1/tree?"+url.Values{"path": {f.root}}.Encode()); got != http.StatusOK {
		t.Fatalf("get /v1/tree: status %d", got)
	}
	after := snapshotMetrics(t)
	for _, family := range []string{"rx_index_cache_hits_total", "rx_index_cache_misses_total"} {
		if before.moved(after, family) {
			t.Errorf("%s changed on a directory listing", family)
		}
	}
}
