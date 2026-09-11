package webapi

// `/v1/tree` reports each entry's modification time, and the two
// backends rendered the same instant differently: rx-go wrote
// 2026-09-06T00:53:24.43832265Z and rx-python wrote
// 2026-09-06T00:53:24.438323 — different precision, different trailing
// zeros, and on the Python side no timezone at all, so the value was a
// naive local time that only meant something to a reader who already
// knew the server's zone.
//
// The agreed rendering is RFC 3339, UTC, with a `Z` and exactly six
// fractional digits. Six because Python's datetime cannot hold
// nanoseconds; fixed rather than trimmed because a file whose mtime
// lands on a whole second must not render a different shape from one
// that does not.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
)

// rfc3339Micros is the exact shape both backends emit: a `Z`, and six
// fractional digits whether or not they are zero.
var rfc3339Micros = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`)

// treeFixture builds a directory with two files, one whose mtime lands
// exactly on a whole second — the case that renders differently when
// the fractional part is trimmed rather than fixed.
func treeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	fractional := filepath.Join(dir, "fractional.log")
	if err := os.WriteFile(fractional, []byte("alpha\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	whole := filepath.Join(dir, "whole-second.log")
	if err := os.WriteFile(whole, []byte("beta\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Truncated to the second, so Nanosecond() is 0.
	wholeSecond := time.Now().UTC().Truncate(time.Second)
	if err := os.Chtimes(whole, wholeSecond, wholeSecond); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return dir
}

func TestTree_ModifiedAtIsRFC3339MicrosecondsUTC(t *testing.T) {
	dir := treeFixture(t)
	if err := paths.SetSearchRoots([]string{dir}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/tree?path=" + dir) //nolint:noctx // test server
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		Entries []struct {
			Name       string  `json:"name"`
			ModifiedAt *string `json:"modified_at"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(body.Entries), body.Entries)
	}

	for _, entry := range body.Entries {
		if entry.ModifiedAt == nil {
			t.Errorf("%s: modified_at is null", entry.Name)
			continue
		}
		if !rfc3339Micros.MatchString(*entry.ModifiedAt) {
			t.Errorf("%s: modified_at is %q, want RFC 3339 UTC with six fractional digits",
				entry.Name, *entry.ModifiedAt)
		}
	}
}
