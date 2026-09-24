package webapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// askLines asks the fixture's gzip log for lines with one line of
// context and the given headers, and returns the status and the body.
func (f *samplesBuildFixture) askLines(t *testing.T, lines string, headers http.Header) (int, []byte) {
	t.Helper()
	query := url.Values{"path": {f.gzPath}, "lines": {lines}, "context": {"1"}}
	req, err := http.NewRequest(http.MethodGet, f.base+"/v1/samples?"+query.Encode(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header = headers
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get samples: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// decodeSamples checks a 200 answer and decodes it.
func decodeSamples(t *testing.T, status int, body []byte) rxtypes.SamplesResponse {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", status, body)
	}
	var answer rxtypes.SamplesResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode samples: %v; body %s", err, body)
	}
	return answer
}

// A lookup whose lines lie in the head of a file that wants an index is
// answered at once, while the build it starts is still held, and names
// that build in index_build. Once the build ends the same lookup is
// answered from the index, with the same answer and no index_build.
func TestSamples_AnswersFromTheHeadAndBuildsInTheBackground(t *testing.T) {
	f := newSamplesBuildFixture(t, 0)
	t.Setenv("RX_SAMPLES_HEAD_MB", "1")

	status, body := f.askLines(t, "5", nil)
	early := decodeSamples(t, status, body)
	if got := early.Samples["5"]; len(got) != 3 || got[1] != "LINE 5 of the log" {
		t.Fatalf("samples[5] = %q, want lines 4 to 6", got)
	}
	if early.IndexBuild == nil {
		t.Fatalf("index_build is null; body %s", body)
	}
	build := early.IndexBuild
	if build.TaskID == "" || build.Path != f.gzPath || build.StartedAt == nil ||
		(build.Status != string(tasks.StatusQueued) && build.Status != string(tasks.StatusRunning)) {
		t.Fatalf("index_build = %+v, want the running build of %s", *build, f.gzPath)
	}
	f.gate.awaitStart(t)

	// A second lookup in the head while the build is held joins it.
	status, joinedBody := f.askLines(t, "7", nil)
	if again := decodeSamples(t, status, joinedBody); again.IndexBuild == nil || again.IndexBuild.TaskID != build.TaskID {
		t.Fatalf("a second lookup names %+v, want task %s", again.IndexBuild, build.TaskID)
	}

	f.gate.open()
	if finished := f.awaitTask(t, build.TaskID); finished.Status != string(tasks.StatusCompleted) {
		t.Fatalf("task ended %q: %v", finished.Status, finished.Error)
	}
	status, indexedBody := f.askLines(t, "5", nil)
	if indexed := decodeSamples(t, status, indexedBody); indexed.IndexBuild != nil {
		t.Fatalf("index_build = %+v once the index is stored, want null", *indexed.IndexBuild)
	}
	samplesanswer.RequireAgree(t, "head and indexed", json.RawMessage(body), json.RawMessage(indexedBody))
	if calls := f.gate.calls.Load(); calls != 1 {
		t.Fatalf("%d index builds started, want 1", calls)
	}
}

// A lookup the head cannot answer takes the path it took before: with
// Prefer: respond-async it answers 202 with the build's task once the
// wait runs out, and without it, it waits and answers 200 with a null
// index_build, since the build it waited for is over.
func TestSamples_PastTheHeadWaitsForTheBuild(t *testing.T) {
	f := newSamplesBuildFixture(t, 0)
	t.Setenv("RX_SAMPLES_HEAD_MB", "1")

	status, body := f.askLines(t, "-1", http.Header{"Prefer": {"respond-async"}})
	pending := f.requirePending(t, status, body)
	f.gate.awaitStart(t)

	type answer struct {
		status int
		body   []byte
	}
	waited := make(chan answer, 1)
	go func() {
		status, body := f.askLines(t, "-1", nil)
		waited <- answer{status, body}
	}()
	f.gate.open()
	got := <-waited
	answered := decodeSamples(t, got.status, got.body)
	if answered.IndexBuild != nil {
		t.Fatalf("index_build = %+v after a wait, want null", *answered.IndexBuild)
	}
	if lines := answered.Samples["300"]; len(lines) != 2 || lines[1] != "LINE 300 of the log" {
		t.Fatalf("samples[300] = %q, want lines 299 and 300", lines)
	}
	if finished := f.awaitTask(t, pending.TaskID); finished.Status != string(tasks.StatusCompleted) {
		t.Fatalf("task ended %q: %v", finished.Status, finished.Error)
	}
}

// reachLogLineBytes is the length of every line of writeReachLog's
// log, line break included: lines 1 to 1000 fit in a head of 1 MiB,
// and line 1001 runs past it.
const reachLogLineBytes = 1048

// writeReachLog rewrites the fixture's gzip log as 2,000 lines of
// reachLogLineBytes bytes, each reading "LINE <n>".
func (f *samplesBuildFixture) writeReachLog(t *testing.T) {
	t.Helper()
	var text bytes.Buffer
	for n := 1; n <= 2000; n++ {
		line := fmt.Sprintf("LINE %d ", n)
		text.WriteString(line + strings.Repeat(".", reachLogLineBytes-1-len(line)) + "\n")
	}
	if err := os.WriteFile(f.gzPath, compressedcopy.Encode(t, compressedcopy.Gzip, text.Bytes()), 0o600); err != nil {
		t.Fatalf("write gzip: %v", err)
	}
}

// reachOfLog returns the head reach the server keeps for the fixture's
// gzip log as it is now, or nil.
func (f *samplesBuildFixture) reachOfLog(t *testing.T) *samples.HeadReach {
	t.Helper()
	info, err := os.Stat(f.gzPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return f.server.samplesIndex.reachOf(f.gzPath, index.IdentityFromInfo(f.gzPath, info))
}

// A lookup past the head records how far the head reaches for the
// file's identity while its build runs, so a later lookup past the
// reach goes to the build without reading the head again, and one
// inside it is still answered from the head at once. A rewritten file
// is a new identity, and the reach of the old one does not apply to it.
func TestSamples_RemembersHowFarTheHeadReaches(t *testing.T) {
	f := newSamplesBuildFixture(t, 20*time.Millisecond)
	t.Setenv("RX_SAMPLES_HEAD_MB", "1")
	f.writeReachLog(t)
	async := http.Header{"Prefer": {"respond-async"}}

	status, body := f.askLines(t, "1500", async)
	pending := f.requirePending(t, status, body)
	f.gate.awaitStart(t)
	want := samples.HeadReach{Head: 1 << 20, Lines: 1000, End: 1000 * reachLogLineBytes}
	if reach := f.reachOfLog(t); reach == nil || *reach != want {
		t.Fatalf("reach %+v, want %+v", reach, want)
	}

	status, body = f.askLines(t, "1-1000", nil)
	early := decodeSamples(t, status, body)
	if early.IndexBuild == nil || early.IndexBuild.TaskID != pending.TaskID {
		t.Fatalf("index_build = %+v, want task %s", early.IndexBuild, pending.TaskID)
	}
	if got := early.Samples["1-1000"]; len(got) != 1000 || !strings.HasPrefix(got[999], "LINE 1000 ") {
		t.Fatalf("samples[1-1000] holds %d lines, want lines 1 to 1000", len(got))
	}
	status, body = f.askLines(t, "2000", async)
	if again := f.requirePending(t, status, body); again.TaskID != pending.TaskID {
		t.Fatalf("a lookup past the reach names task %s, want %s", again.TaskID, pending.TaskID)
	}

	// The rewritten log has 300 short lines, all in the head: line 1500
	// is past its end, which the head answers.
	f.writeLog(t, "NEW")
	if reach := f.reachOfLog(t); reach != nil {
		t.Fatalf("reach %+v for the rewritten file, want none", *reach)
	}
	status, body = f.askLines(t, "1500", async)
	if answer := decodeSamples(t, status, body); answer.Lines["1500"] != -1 {
		t.Fatalf("lines[1500] = %d for the rewritten file, want -1", answer.Lines["1500"])
	}

	f.gate.open()
	if finished := f.awaitTask(t, pending.TaskID); finished.Status != string(tasks.StatusCompleted) {
		t.Fatalf("task ended %q: %v", finished.Status, finished.Error)
	}
}
