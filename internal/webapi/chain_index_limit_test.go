package webapi

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainRouteAnswer is one answer of a chain route: its status, body and
// headers.
type chainRouteAnswer struct {
	route  string
	status int
	body   []byte
	header http.Header
}

// askChainRoute sends one request without a body and returns its answer.
func askChainRoute(t *testing.T, method, base, route string, query url.Values) chainRouteAnswer {
	t.Helper()
	req, err := http.NewRequest(method, base+route+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return chainRouteAnswer{route: method + " " + route, status: resp.StatusCode, body: body, header: resp.Header}
}

// unfinishedChainTasksOf reads how many index tasks of log chains hold
// a place among the unfinished ones.
func unfinishedChainTasksOf(c *chainIndexTasks) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unfinished
}

// With as many chain index tasks unfinished as the limit allows (the
// chains' share of the build queue, here 2), one more pending chain gets
// no task: its describe answers pending with no index_build, and
// POST /v1/logs/index and a samples request by global line answer 503
// with a Retry-After of a few seconds. A chain whose task runs is still
// joined at the limit. Once a task ends, a describe of the chain past
// the limit starts its task.
func TestChainIndex_APendingChainPastTheLimitStartsNoTask(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	held := func(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
		<-release
		return samples.BuildIndex(path, progress)
	}
	f := newChainIndexFixture(t, 1, held)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	builds := f.server.samplesIndex
	builds.maxQueued = 4
	limit := builds.maxChainParts()
	handle := func(chain string) string { return filepath.Join(f.root, chain, "app.log") }
	for _, chain := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(f.root, chain), 0o700); err != nil {
			t.Fatal(err)
		}
		writeIndexChain(t, filepath.Join(f.root, chain), []chainPartFile{{name: "app.log.1"}, {name: "app.log"}}, 2, chainStart)
	}
	first := describeChainAt(t, f.base, handle("a")).IndexBuild
	second := describeChainAt(t, f.base, handle("b")).IndexBuild
	if first == nil || second == nil || limit != 2 || unfinishedChainTasksOf(f.server.chainIndex) != limit {
		t.Fatalf("index_build %+v and %+v, limit %d, %d unfinished; want two tasks at the limit of 2",
			first, second, limit, unfinishedChainTasksOf(f.server.chainIndex))
	}

	past := describeChainAt(t, f.base, handle("c"))
	posted := askChainRoute(t, http.MethodPost, f.base, "/v1/logs/index", url.Values{"path": {handle("c")}})
	read := askChainRoute(t, http.MethodGet, f.base, "/v1/logs/samples", url.Values{"path": {handle("c")}, "lines": {"1"}})
	joined := describeChainAt(t, f.base, handle("a")).IndexBuild

	if past.State != rxtypes.ChainStatePending || past.IndexBuild != nil {
		t.Errorf("past the limit: state %s, index_build %+v; want pending with none", past.State, past.IndexBuild)
	}
	for _, answer := range []chainRouteAnswer{posted, read} {
		if answer.status != http.StatusServiceUnavailable || !strings.Contains(string(answer.body), "tasks the server runs at once (2) are running or waiting") {
			t.Errorf("%s past the limit: status %d, %s; want 503 naming the limit", answer.route, answer.status, answer.body)
		}
		if got := answer.header.Get("Retry-After"); got != strconv.Itoa(chainTasksFullRetryAfterSeconds) {
			t.Errorf("%s past the limit: Retry-After %q, want %d", answer.route, got, chainTasksFullRetryAfterSeconds)
		}
	}
	if joined == nil || joined.TaskID != first.TaskID {
		t.Errorf("a describe of a chain whose task runs, at the limit: %+v, want its task %s", joined, first.TaskID)
	}
	if n := chainIndexTasksIn(f.manager); n != limit {
		t.Fatalf("%d chain index tasks in the table, want the %d at the limit", n, limit)
	}

	// Chain a's part build takes the release and ends, and so does a's task.
	release <- struct{}{}
	awaitTaskEnd(t, f.manager, first.TaskID)
	again := describeChainAt(t, f.base, handle("c")).IndexBuild

	if again == nil || again.TaskID == first.TaskID || again.TaskID == second.TaskID {
		t.Fatalf("once a task ended, the chain past the limit got %+v, want a task of its own", again)
	}
}
