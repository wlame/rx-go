package samples

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// The loader looks the index up by path, and the path can lead to
// another file at that moment than the one Resolve pinned and reads (a
// link retargeted and put back). An index whose recorded inode is not
// the pinned file's is then ignored: the answer is the one the file
// itself gives, as without an index.
func TestResolve_IgnoresAnIndexBuiltFromAnotherFile(t *testing.T) {
	pinned := writeFixture(t)
	other := filepath.Join(t.TempDir(), "other.log")
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&sb, "line %03d of another file, longer\n", i)
	}
	if err := os.WriteFile(other, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	otherIndex, err := index.Build(other, index.BuildOptions{StepBytes: 64})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(otherIndex.LineIndex) < 2 {
		t.Fatalf("the other file's index has %d checkpoints, want several", len(otherIndex.LineIndex))
	}
	loader := func(string) (*rxtypes.UnifiedFileIndex, error) { return otherIndex, nil }

	cases := []struct {
		name string
		req  Request
	}{
		{"lines", Request{Path: pinned, Lines: []OffsetOrRange{{Start: 17}}, BeforeContext: 1, AfterContext: 1}},
		{"offsets", Request{Path: pinned, Offsets: []OffsetOrRange{{Start: 150}}, BeforeContext: 1, AfterContext: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withIndex, noIndex := tc.req, tc.req
			withIndex.IndexLoader = loader
			noIndex.IndexLoader = NoIndex

			got, err := Resolve(withIndex)
			if err != nil {
				t.Fatalf("Resolve with the other index: %v", err)
			}
			want, err := Resolve(noIndex)
			if err != nil {
				t.Fatalf("Resolve without an index: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("with another file's index:\n  %+v\nwithout an index:\n  %+v", got, want)
			}
		})
	}
}
