// Package integration_test contains API-level integration tests that exercise
// the full HTTP stack (router, middleware, handlers, engine). These live outside
// internal/engine/ to avoid import cycles (api imports engine).
package integration_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/api"
	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/models"
)

// testdataDir returns the absolute path to the testdata directory.
func testdataDir(t *testing.T) string {
	t.Helper()
	// integration_test/ is at the project root, so testdata is at ../testdata/
	dir, err := filepath.Abs("../testdata")
	require.NoError(t, err)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skipf("testdata directory not found at %s, skipping API integration test", dir)
	}
	return dir
}

// newTestServer creates a test HTTP server backed by a real API server with isolated config.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_NO_CACHE", "")

	td := testdataDir(t)

	// Configure search roots to include testdata so path validation passes.
	t.Setenv("RX_SEARCH_ROOT", td)

	cfg := config.Load()
	srv := api.NewServer(&cfg)
	ts := httptest.NewServer(srv.Router)
	t.Cleanup(ts.Close)

	return ts
}

func TestAPI_TraceEndpoint(t *testing.T) {
	ts := newTestServer(t)
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	url := ts.URL + "/v1/trace?path=" + path + "&regexp=ERROR"
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var traceResp models.TraceResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&traceResp))

	assert.GreaterOrEqual(t, len(traceResp.Matches), 5)
	assert.Len(t, traceResp.Patterns, 1)
	assert.Equal(t, "ERROR", traceResp.Patterns["p1"])
}

func TestAPI_SamplesEndpoint(t *testing.T) {
	ts := newTestServer(t)
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	url := ts.URL + "/v1/samples?path=" + path + "&byte_offset=0"
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var samplesResp models.SamplesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&samplesResp))

	assert.Equal(t, path, samplesResp.Path)
	assert.Contains(t, samplesResp.Samples, "0", "should have samples for offset 0")
	assert.GreaterOrEqual(t, len(samplesResp.Samples["0"]), 1,
		"should return at least 1 context line")
}

func TestAPI_HealthEndpoint(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var health map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&health))

	// Verify key fields are present.
	assert.Equal(t, "ok", health["status"])
	assert.Contains(t, health, "uptime_seconds")
	assert.Contains(t, health, "rg_version")
	assert.Contains(t, health, "go_version")
	assert.Contains(t, health, "constants")
	assert.Contains(t, health, "os_info")
}

func TestAPI_ComplexityStub(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/complexity?regex=ERROR")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var complexity models.ComplexityResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&complexity))

	assert.Equal(t, "ERROR", complexity.Regex)
	assert.Equal(t, "not_implemented", complexity.RiskLevel)
	assert.Equal(t, float64(0), complexity.Score)
	assert.Empty(t, complexity.Issues)
	assert.Empty(t, complexity.Recommendations)
}

func TestAPI_DetectorsEndpoint(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/detectors")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var detectors models.DetectorsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&detectors))

	assert.Empty(t, detectors.Detectors, "detectors list should be empty")
	assert.GreaterOrEqual(t, len(detectors.SeverityScale), 4,
		"severity scale should have at least 4 levels")
}

func TestAPI_IndexTaskLifecycle(t *testing.T) {
	ts := newTestServer(t)
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	// POST to start an indexing task.
	body := strings.NewReader(`{"path":"` + path + `"}`)
	resp, err := http.Post(ts.URL+"/v1/index", "application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var taskResp models.TaskResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&taskResp))

	assert.NotEmpty(t, taskResp.TaskID, "task ID should not be empty")
	assert.Contains(t, []string{"pending", "running"}, taskResp.Status)

	// GET the task status.
	statusResp, err := http.Get(ts.URL + "/v1/tasks/" + taskResp.TaskID)
	require.NoError(t, err)
	defer statusResp.Body.Close()

	assert.Equal(t, http.StatusOK, statusResp.StatusCode)

	var taskStatus models.TaskStatusResponse
	require.NoError(t, json.NewDecoder(statusResp.Body).Decode(&taskStatus))

	assert.Equal(t, taskResp.TaskID, taskStatus.TaskID)
	assert.Contains(t, []string{"pending", "running", "completed"}, taskStatus.Status)
}

func TestAPI_TaskNotFound(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/tasks/nonexistent-id")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAPI_TraceMissingParams(t *testing.T) {
	ts := newTestServer(t)

	// Missing path.
	resp, err := http.Get(ts.URL + "/v1/trace?regexp=ERROR")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Missing regexp.
	resp2, err := http.Get(ts.URL + "/v1/trace?path=/tmp")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp2.StatusCode)
}
