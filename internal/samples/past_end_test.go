package samples

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// A position past the end of the file is asked-but-unknown, which the
// line-numbering contract already spells `-1` in the number map and
// `null` in `samples`. The two coordinates answer the same way: a line
// number past the last line and a byte offset past the last byte are the
// same question asked twice.
//
// The valid positions in the same request are still answered. One bad
// number must not throw away the good ones — that is the difference
// between a batch a caller can use and a batch it has to retry one at a
// time.

func pastEndFixture(t *testing.T) (path string, size int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "three.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return path, info.Size()
}

func TestPastEnd_LineNumberIsUnknownRatherThanClamped(t *testing.T) {
	path, _ := pastEndFixture(t)

	resp, err := Resolve(t.Context(), Request{
		Path:        path,
		Lines:       []OffsetOrRange{{Start: 99}},
		IndexLoader: NoIndex,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got := resp.Lines["99"]; got != -1 {
		t.Errorf("lines[99]: got %d, want -1", got)
	}
	if got, ok := resp.Samples["99"]; !ok || got != nil {
		t.Errorf("samples[99]: got %v, want nil", got)
	}
}

func TestPastEnd_ByteOffsetIsUnknownRatherThanClamped(t *testing.T) {
	path, size := pastEndFixture(t)
	past := size + 100
	key := "106"
	if past != 106 {
		t.Fatalf("fixture is %d bytes; the key below assumes 6", size)
	}

	resp, err := Resolve(t.Context(), Request{
		Path:        path,
		Offsets:     []OffsetOrRange{{Start: past}},
		IndexLoader: NoIndex,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// It used to report the file's last line, which is a number counted
	// from the wrong place — exactly what the contract forbids.
	if got := resp.Offsets[key]; got != -1 {
		t.Errorf("offsets[%s]: got %d, want -1", key, got)
	}
	if got, ok := resp.Samples[key]; !ok || got != nil {
		t.Errorf("samples[%s]: got %v, want nil", key, got)
	}
}

// One bad number does not throw away the good ones.
func TestPastEnd_ValidPositionsInTheSameRequestSurvive(t *testing.T) {
	path, size := pastEndFixture(t)

	lines, err := Resolve(t.Context(), Request{
		Path:          path,
		Lines:         []OffsetOrRange{{Start: 2}, {Start: 99}},
		BeforeContext: 0,
		AfterContext:  0,
		IndexLoader:   NoIndex,
	})
	if err != nil {
		t.Fatalf("Resolve lines: %v", err)
	}
	if lines.Lines["2"] != 2 {
		t.Errorf("lines[2]: got %d, want 2 (its byte offset)", lines.Lines["2"])
	}
	if got := lines.Samples["2"]; len(got) != 1 || got[0] != "b" {
		t.Errorf("samples[2]: got %v, want [b]", got)
	}
	if lines.Lines["99"] != -1 {
		t.Errorf("lines[99]: got %d, want -1", lines.Lines["99"])
	}

	offsets, err := Resolve(t.Context(), Request{
		Path:          path,
		Offsets:       []OffsetOrRange{{Start: 2}, {Start: size + 100}},
		BeforeContext: 0,
		AfterContext:  0,
		IndexLoader:   NoIndex,
	})
	if err != nil {
		t.Fatalf("Resolve offsets: %v", err)
	}
	if offsets.Offsets["2"] != 2 {
		t.Errorf("offsets[2]: got %d, want line 2", offsets.Offsets["2"])
	}
	if offsets.Offsets["106"] != -1 {
		t.Errorf("offsets[106]: got %d, want -1", offsets.Offsets["106"])
	}
}

// The last real position is not "past the end" and must still answer.
func TestPastEnd_TheBoundaryItselfIsAnswered(t *testing.T) {
	path, size := pastEndFixture(t)

	lines, err := Resolve(t.Context(), Request{
		Path: path, Lines: []OffsetOrRange{{Start: 3}}, IndexLoader: NoIndex,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lines.Lines["3"] == -1 {
		t.Error("the last line reported as past the end")
	}

	offsets, err := Resolve(t.Context(), Request{
		Path: path, Offsets: []OffsetOrRange{{Start: size - 1}}, IndexLoader: NoIndex,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if offsets.Offsets["5"] != 3 {
		t.Errorf("the last byte resolved to line %d, want 3", offsets.Offsets["5"])
	}
}

// The line after the last of a file that ends with a line break is not
// a line: asked with context, its sample is the context it reaches and
// its offset is -1, on the plain file and on its gzip copy alike.
func TestPastEnd_TheLineAfterTheLastHasNoOffset(t *testing.T) {
	path, _ := pastEndFixture(t)
	gz := path + ".gz"
	writeFile(t, gz, compressedcopy.Encode(t, compressedcopy.Gzip, []byte("a\nb\nc\n")))
	for _, p := range []string{path, gz} {
		resp, err := Resolve(t.Context(), Request{Path: p, Lines: []OffsetOrRange{{Start: 4}}, BeforeContext: 1, IndexLoader: NoIndex})
		if err != nil {
			t.Fatalf("%s: Resolve: %v", p, err)
		}
		if got := resp.Lines["4"]; got != -1 {
			t.Errorf("%s: lines[4] = %d, want -1", filepath.Base(p), got)
		}
		if got := resp.Samples["4"]; len(got) != 1 || got[0] != "c" {
			t.Errorf("%s: samples[4] = %q, want the context line c", filepath.Base(p), got)
		}
	}
}
