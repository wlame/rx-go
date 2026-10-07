package logchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainFile is one file of a chain a test writes: its name, its text
// before compression, how it is compressed (a compressedcopy format, ""
// for plain) and its modification time.
type chainFile struct {
	name  string
	text  []byte
	codec string
	mtime time.Time
}

// chainBase is the time the test chains' first line is written at.
var chainBase = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// timedLines is n lines of one part of a test chain, each of the form
// `<timestamp> LINE <global> part=<part> local=<local>`, the first at
// start and each next one step later. A wrong line number is visible in
// the line itself.
func timedLines(start time.Time, step time.Duration, firstGlobal, n int, part string) []byte {
	var buf bytes.Buffer
	for local := 1; local <= n; local++ {
		at := start.Add(time.Duration(local-1) * step)
		fmt.Fprintf(&buf, "%s LINE %d part=%s local=%d\n", at.Format("2006-01-02 15:04:05.000"),
			firstGlobal+local-1, part, local)
	}
	return buf.Bytes()
}

// writeChainFiles writes files into dir, compressed as each says, with
// their modification times, and returns dir. A codec the host cannot
// write skips the test.
func writeChainFiles(t *testing.T, dir string, files []chainFile) string {
	t.Helper()
	for _, f := range files {
		body := f.text
		if f.codec != "" {
			body = compressedcopy.Encode(t, f.codec, f.text)
			if body == nil {
				t.Skipf("no %s encoder on this host", f.codec)
			}
		}
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", f.name, err)
		}
		mtime := f.mtime
		if mtime.IsZero() {
			mtime = chainBase
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", f.name, err)
		}
	}
	return dir
}

// resolveIn resolves the chain named name in dir, without search roots.
func resolveIn(t *testing.T, dir, name string) Candidate {
	t.Helper()
	paths.Reset()
	c, err := Resolve(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return c
}

// describe resolves and describes the chain named name in dir.
func describe(t *testing.T, dir, name string, opts Options) *Description {
	t.Helper()
	d, err := Describe(context.Background(), resolveIn(t, dir, name), opts)
	if err != nil {
		t.Fatalf("describe %s: %v", name, err)
	}
	return d
}

// storeIndexes builds and stores the line index of each named file of
// dir, as `rx index` does.
func storeIndexes(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		built, err := index.Build(filepath.Join(dir, name), index.BuildOptions{})
		if err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		if _, err := index.Save(built); err != nil {
			t.Fatalf("save the index of %s: %v", name, err)
		}
	}
}

// partNamesOf are the names of a description's parts, in its order.
func partNamesOf(d *Description) []string {
	names := make([]string, 0, len(d.Response.Parts))
	for _, p := range d.Response.Parts {
		names = append(names, p.Name)
	}
	return names
}

// reasonCodes are the codes of a description's reasons, in order.
func reasonCodes(d *Description) []string {
	codes := []string{}
	for _, r := range d.Response.Reasons {
		codes = append(codes, r.Code)
	}
	return codes
}

// concatenation writes the texts of files in the order names gives
// into one file of dir, a newline added after a text that lacks one,
// as the chain reads them, and returns its path: the oracle a chain's
// answers are compared with.
func concatenation(t *testing.T, dir string, files map[string]chainFile, names []string) string {
	t.Helper()
	var buf bytes.Buffer
	for _, name := range names {
		text := files[name].text
		buf.Write(text)
		if len(text) > 0 && text[len(text)-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	path := filepath.Join(dir, "concatenation.txt")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write the concatenation: %v", err)
	}
	return path
}

// singleFileAnswer is what rx answers for one file: its line count (a
// built index) and its time range.
func singleFileAnswer(t *testing.T, path string) (int64, *rxtypes.TimeRangeResponse) {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		t.Fatalf("index %s: %v", path, err)
	}
	loader := func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }
	tr, err := samples.TimeRange(context.Background(), samples.Request{Path: path, IndexLoader: loader})
	if err != nil {
		t.Fatalf("time range of %s: %v", path, err)
	}
	return *idx.LineCount, tr
}

// lineCountOf is the number of lines in text as rx numbers them: its
// line breaks, plus one for a last line without one (wc -l, plus that
// line).
func lineCountOf(text []byte) int64 {
	n := int64(bytes.Count(text, []byte{'\n'}))
	if len(text) > 0 && text[len(text)-1] != '\n' {
		n++
	}
	return n
}

// requireSameDescription fails unless a and b describe the chain the
// same way under the accelerator rule: every member equal, except
// cli_command and is_indexed, which say how the answer was made, and a
// count or time that is null in one of them (not read) and filled in
// the other.
func requireSameDescription(t *testing.T, label string, a, b *rxtypes.ChainResponse) {
	t.Helper()
	norm := func(r *rxtypes.ChainResponse, other *rxtypes.ChainResponse) rxtypes.ChainResponse {
		out := *r
		out.CLICommand = ""
		out.Parts = make([]rxtypes.ChainPart, len(r.Parts))
		for i, p := range r.Parts {
			p.IsIndexed = false
			if i < len(other.Parts) {
				q := other.Parts[i]
				if p.LineCount == nil || q.LineCount == nil {
					p.LineCount = nil
				}
				if p.MaxMs == nil || q.MaxMs == nil {
					p.MaxMs, p.MaxIsBound = nil, false
				}
			}
			out.Parts[i] = p
		}
		if r.LineCount == nil || other.LineCount == nil {
			out.LineCount = nil
		}
		return out
	}
	x, y := norm(a, b), norm(b, a)
	if fmt.Sprint(jsonOf(t, x)) != fmt.Sprint(jsonOf(t, y)) {
		t.Fatalf("%s: the descriptions differ\n%s\n%s", label, jsonOf(t, x), jsonOf(t, y))
	}
}

// jsonOf is v as indented JSON, for comparing and showing answers.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.String()
}
