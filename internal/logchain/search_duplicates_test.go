package logchain

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Another encoding of a part (app.log.1.gz beside app.log.1) named with
// its chain, in either order and beside the part's own path: the chain
// searches the part it described, the other encoding is skipped once
// with duplicate_part and never searched, so each line comes once, in
// the chain, with its chain line.
func TestSearch_AnotherEncodingNamedWithItsChainIsSkippedWhicheverComesFirst(t *testing.T) {
	dir := t.TempDir()
	first := timedLines(chainBase.Add(time.Hour), time.Second, 4, 3, "1")
	writeChainFiles(t, dir, []chainFile{
		{name: "app.log.2.gz", text: timedLines(chainBase, time.Second, 1, 3, "2"), codec: compressedcopy.Gzip},
		{name: "app.log.1", text: first},
		{name: "app.log.1.gz", text: first, codec: compressedcopy.Gzip},
		{name: "app.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 7, 3, "active")},
	})
	storeIndexes(t, dir, "app.log.2.gz", "app.log.1")
	handle, part, other := filepath.Join(dir, "app.log"), filepath.Join(dir, "app.log.1"), filepath.Join(dir, "app.log.1.gz")
	for _, tc := range []struct {
		label string
		paths []string
	}{
		{"the handle, then the other encoding", []string{handle, other}},
		{"the other encoding, then the handle", []string{other, handle}},
		{"the directory, then the other encoding", []string{dir, other}},
		{"the other encoding, then the directory", []string{other, dir}},
		{"the other encoding, the part and the handle", []string{other, part, handle}},
		{"the part, the other encoding and the handle", []string{part, other, handle}},
	} {
		answer := searchFor(t, SearchRequest{Paths: tc.paths, Patterns: []string{"LINE"}}).Answer
		ref, ok := answer.Chains["c1"]
		if len(answer.Chains) != 1 || !ok || ref.State != rxtypes.ChainStateReady || len(ref.Parts) != 3 {
			t.Fatalf("%s: chains %+v", tc.label, answer.Chains)
		}
		if !slices.Equal(answer.SkippedFiles, []string{other}) || len(answer.SkipReasons) != 1 ||
			!strings.HasPrefix(answer.SkipReasons[0].Reason, ReasonDuplicatePart+":") {
			t.Fatalf("%s: skipped %v (%+v), want %s once as %s", tc.label, answer.SkippedFiles, answer.SkipReasons, other, ReasonDuplicatePart)
		}
		for id, path := range answer.Files {
			if slices.Contains(answer.SkippedFiles, path) {
				t.Fatalf("%s: %s is searched as %s and skipped", tc.label, path, id)
			}
		}
		seen := map[string]bool{}
		for _, m := range answer.Matches {
			text := *m.LineText
			named, _ := strconv.ParseInt(globalOf.FindStringSubmatch(text)[1], 10, 64)
			if seen[text] || m.Chain == nil || *m.Chain != "c1" || m.ChainLine != named {
				t.Fatalf("%s: match %q (twice: %v) in chain %v at chain_line %d", tc.label, text, seen[text], m.Chain, m.ChainLine)
			}
			seen[text] = true
		}
		if len(seen) != 9 {
			t.Fatalf("%s: %d lines, want the chain's 9", tc.label, len(seen))
		}
	}
}
