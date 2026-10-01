package sigscan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/testutil/isolatedcache"
	"github.com/wlame/rx-go/internal/trace"
)

// TestMain points RX_CACHE_DIR at a temporary directory for every test
// in this package, so no test can reach the user's real rx cache.
func TestMain(m *testing.M) {
	isolatedcache.Main(m)
}

// loadSigs loads the signatures of a YAML file body and returns them by
// id. Any load error fails the test. The built-in signatures are left
// out unless named.
func loadSigs(t testing.TB, body string, ids ...string) []*signatures.Signature {
	t.Helper()
	path := filepath.Join(t.TempDir(), "team.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{path}})
	if errs := cat.Errors(); len(errs) > 0 {
		t.Fatalf("load errors: %v", errs)
	}
	var out []*signatures.Signature
	for _, id := range ids {
		sig, ok := cat.Lookup(id)
		if !ok {
			t.Fatalf("no signature %q", id)
		}
		out = append(out, sig)
	}
	return out
}

// sigFile wraps signature definitions into a file body.
func sigFile(defs ...string) string {
	return "signatures:\n" + strings.Join(defs, "")
}

// writeFile writes data to a file in a temporary directory and returns
// it pinned and classified, as a scan receives it.
func writeFile(t testing.TB, name string, data []byte) trace.SearchFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return classify(t, path)
}

// classify pins and classifies path.
func classify(t testing.TB, path string) trace.SearchFile {
	t.Helper()
	pin, err := paths.Pin(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := trace.ClassifyForSearch(pin)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// scanOne scans text written as a plain file with default tuning and
// one worker.
func scanOne(t testing.TB, text string, sigs []*signatures.Signature, opts Options) *Result {
	t.Helper()
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	res, err := Scan(context.Background(), writeFile(t, "app.log", []byte(text)), sigs, opts)
	if err != nil {
		t.Fatal(err)
	}
	requireValidExcerpts(t, res)
	return res
}

// lineSpan is a finding's lines, for compact expectations.
type lineSpan struct{ start, end int64 }

// spans returns the line spans of a signature's findings.
func spans(res *Result, id string) []lineSpan {
	var out []lineSpan
	for _, f := range res.BySignature[id].Findings {
		out = append(out, lineSpan{f.StartLine, f.EndLine})
	}
	return out
}

// lines joins lines with "\n" and ends the text with one.
func lines(ls ...string) string {
	return strings.Join(ls, "\n") + "\n"
}
