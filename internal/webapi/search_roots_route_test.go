package webapi

import (
	"net/http"
	"testing"
)

// No rx serves GET /v1/search-roots: the search roots are in /health
// and in GET /v1/tree without a path. The route answers 404, and a
// route of that name would be a deliberate addition to the contract.
func TestSearchRootsRoute_IsNotServed(t *testing.T) {
	ts := newTestServer(t)
	status, _, _ := get(t, ts.URL+"/v1/search-roots", "")
	if status != http.StatusNotFound {
		t.Errorf("GET /v1/search-roots: status %d, want 404", status)
	}
}
