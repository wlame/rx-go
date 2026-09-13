package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/tasks"
)

// TestTaskStatus_ResultIsNullUntilTheTaskCompletes asserts that a task
// that has not finished answers `"result": null`, and that the OpenAPI
// document allows exactly that, so a client generated from it accepts
// the poll answers it gets before the last one.
func TestTaskStatus_ResultIsNullUntilTheTaskCompletes(t *testing.T) {
	manager := tasks.New(tasks.Config{})
	queued, _ := manager.Create("/var/log/app.log", "index")
	srv := NewServer(Config{AppVersion: "unit-test", TaskManager: manager})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/tasks/" + queued.TaskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(body["result"]); got != "null" {
		t.Fatalf("result of a queued task = %s, want null", got)
	}

	schema := srv.API().OpenAPI().Components.Schemas.Map()["TaskStatusResponse"].Properties["result"]
	declared, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal result schema: %v", err)
	}
	if !strings.Contains(string(declared), `"null"`) {
		t.Errorf("TaskStatusResponse.result schema does not allow null: %s", declared)
	}
}
