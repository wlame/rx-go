package logchain

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// builtChain is a chain a test wrote: its directory, its files by name,
// their order in the chain (oldest first, the active file last when it
// exists) and the concatenation of their texts in that order, the
// oracle every answer is compared with.
type builtChain struct {
	dir    string
	name   string
	files  map[string]chainFile
	order  []string
	concat string
	// zone, when it names one, is the file zone every answer reads the
	// timestamps in (file_tz), the chain's and the concatenation's.
	zone config.Zone
}

// buildChain writes files, in the chain's order, into a new directory,
// and their concatenation beside them (named so it is no part).
func buildChain(t *testing.T, name string, files []chainFile) builtChain {
	t.Helper()
	dir := writeChainFiles(t, t.TempDir(), files)
	c := builtChain{dir: dir, name: name, files: map[string]chainFile{}}
	for _, f := range files {
		c.files[f.name] = f
		c.order = append(c.order, f.name)
	}
	c.concat = concatenation(t, dir, c.files, c.order)
	return c
}

// resolveReader reads a part with samples.Resolve, as `rx logs samples`
// does.
func resolveReader(ctx context.Context, _ Part, req samples.Request) (*rxtypes.SamplesResponse, error) {
	return samples.Resolve(ctx, req)
}

// chainSamples describes c (with a scan when cold, from the stored
// indexes otherwise) and answers req on it with resolveReader.
func chainSamples(t *testing.T, c builtChain, req SamplesRequest, scan bool) *rxtypes.ChainSamplesResponse {
	t.Helper()
	d := describe(t, c.dir, c.name, Options{Scan: scan, FileZone: c.zone})
	if !scan && d.Response.State != rxtypes.ChainStateReady {
		t.Fatalf("the indexed chain is %s", d.Response.State)
	}
	if req.IndexLoader == nil {
		req.IndexLoader = samples.StoredIndex
	}
	resp, err := Samples(context.Background(), d, req, resolveReader)
	if err != nil {
		t.Fatalf("chain samples %+v: %v", req, err)
	}
	return resp
}

// concatSamples answers req on the concatenation, read as one file
// without an index.
func concatSamples(t *testing.T, c builtChain, req SamplesRequest) *rxtypes.SamplesResponse {
	t.Helper()
	resp, err := samples.Resolve(context.Background(), samples.Request{
		Path: c.concat, Lines: req.Lines, Timestamps: req.Timestamps, FileZone: c.zone,
		BeforeContext: req.BeforeContext, AfterContext: req.AfterContext, IndexLoader: samples.NoIndex,
	})
	if err != nil {
		t.Fatalf("concatenation samples %+v: %v", req, err)
	}
	return resp
}

// requireLikeConcatenation answers every request on c cold (no stored
// index, the chain described by a scan) and then with every part
// indexed, and requires each answer to equal the concatenation's: the
// same keys, the same lines and line_timestamps for each key, a target
// where the concatenation has the line, and pieces whose global numbers
// follow the parts' starts.
func requireLikeConcatenation(t *testing.T, c builtChain, reqs ...SamplesRequest) {
	t.Helper()
	oracle := make([]*rxtypes.SamplesResponse, len(reqs))
	for i, req := range reqs {
		oracle[i] = concatSamples(t, c, req)
	}
	cold := make([]*rxtypes.ChainSamplesResponse, len(reqs))
	for i, req := range reqs {
		cold[i] = chainSamples(t, c, req, true)
		requireAnswerLike(t, fmt.Sprintf("cold %+v", req), cold[i], oracle[i], len(req.Timestamps) > 0)
	}
	storeIndexes(t, c.dir, c.order...)
	for i, req := range reqs {
		indexed := chainSamples(t, c, req, false)
		requireAnswerLike(t, fmt.Sprintf("indexed %+v", req), indexed, oracle[i], len(req.Timestamps) > 0)
		if a, b := jsonOf(t, cold[i].Samples), jsonOf(t, indexed.Samples); a != b {
			t.Fatalf("cold and indexed pieces differ for %+v:\n%s\n%s", req, a, b)
		}
	}
}

// requireAnswerLike fails unless the chain's answer equals the
// concatenation's, key by key, and its pieces are numbered by the
// parts' global starts.
func requireAnswerLike(t *testing.T, label string, chain *rxtypes.ChainSamplesResponse, single *rxtypes.SamplesResponse, timeMode bool) {
	t.Helper()
	if len(chain.Samples) != len(single.Samples) {
		t.Fatalf("%s: keys %v, the concatenation has %v", label, keysOf(chain.Samples), keysOf(single.Samples))
	}
	for key, want := range single.Samples {
		pieces, ok := chain.Samples[key]
		if !ok {
			t.Fatalf("%s: no key %q (keys %v)", label, key, keysOf(chain.Samples))
		}
		lines, stamps := flatten(pieces)
		if (want == nil) != (pieces == nil) || !slices.Equal(lines, want) {
			t.Fatalf("%s: key %q lines %q (pieces %s), the concatenation %q", label, key, lines, jsonOf(t, pieces), want)
		}
		var wantStamps []*int64
		if single.LineTimestamps != nil {
			wantStamps = single.LineTimestamps[key]
		}
		if got, want := stampsText(stamps, len(lines)), stampsText(wantStamps, len(lines)); got != want {
			t.Fatalf("%s: key %q line_timestamps %s, the concatenation %s", label, key, got, want)
		}
		requireTarget(t, label, key, chain, single, timeMode)
		requirePieceNumbers(t, label, key, chain)
	}
}

// requireTarget checks a key's target against the concatenation: the
// line a time found, or for a single line, the line itself exactly when
// the concatenation has it.
func requireTarget(t *testing.T, label, key string, chain *rxtypes.ChainSamplesResponse, single *rxtypes.SamplesResponse, timeMode bool) {
	t.Helper()
	if timeMode {
		if got, want := chain.Timestamps[key], single.Timestamps[key]; got != want {
			t.Fatalf("%s: timestamps[%q] = %d, the concatenation %d", label, key, got, want)
		}
		return
	}
	n, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return // a range: the concatenation answers -1 for every range
	}
	want := int64(-1)
	if single.Lines[key] >= 0 {
		want = n
	}
	if got := chain.Lines[key]; got != want {
		t.Fatalf("%s: lines[%q] = %d, want %d", label, key, got, want)
	}
}

// requirePieceNumbers checks each piece of key against the parts: its
// global line is its part's start plus its local line, the pieces follow
// each other without a hole, and part_start / part_end say where the
// part begins and ends.
func requirePieceNumbers(t *testing.T, label, key string, chain *rxtypes.ChainSamplesResponse) {
	t.Helper()
	starts := map[string]rxtypes.ChainPart{}
	for _, p := range chain.Parts {
		starts[p.Name] = p
	}
	var next int64
	for i, piece := range chain.Samples[key] {
		part := starts[piece.Part]
		if part.GlobalStart == nil || piece.FirstGlobalLine != *part.GlobalStart+piece.FirstLocalLine-1 {
			t.Fatalf("%s: key %q piece %d of %s: global %d, local %d, part start %v", label, key, i, piece.Part,
				piece.FirstGlobalLine, piece.FirstLocalLine, part.GlobalStart)
		}
		if i > 0 && piece.FirstGlobalLine != next {
			t.Fatalf("%s: key %q piece %d starts at %d, the one before ends before %d", label, key, i, piece.FirstGlobalLine, next)
		}
		next = piece.FirstGlobalLine + int64(len(piece.Lines))
		if piece.PartStart != (piece.FirstLocalLine == 1) {
			t.Fatalf("%s: key %q piece %d part_start %v at local %d", label, key, i, piece.PartStart, piece.FirstLocalLine)
		}
		if part.LineCount != nil && !part.IsActive {
			if end := piece.FirstLocalLine + int64(len(piece.Lines)) - 1; piece.PartEnd != (end == *part.LineCount) {
				t.Fatalf("%s: key %q piece %d part_end %v, ends at %d of %d", label, key, i, piece.PartEnd, end, *part.LineCount)
			}
		}
	}
}

// flatten joins a key's pieces into its lines and their timestamps, as
// one file's answer holds them.
func flatten(pieces []rxtypes.ChainPiece) ([]string, []*int64) {
	if pieces == nil {
		return nil, nil
	}
	lines := []string{}
	var stamps []*int64
	for _, p := range pieces {
		lines = append(lines, p.Lines...)
		if p.LineTimestamps == nil {
			stamps = append(stamps, make([]*int64, len(p.Lines))...)
			continue
		}
		stamps = append(stamps, p.LineTimestamps...)
	}
	return lines, stamps
}

// stampsText writes n line timestamps for a failure message and for
// comparing: a missing list is n nulls.
func stampsText(stamps []*int64, n int) string {
	words := make([]string, n)
	for i := range words {
		words[i] = "null"
		if i < len(stamps) && stamps[i] != nil {
			words[i] = strconv.FormatInt(*stamps[i], 10)
		}
	}
	return "[" + strings.Join(words, " ") + "]"
}

// keysOf lists a map's keys, sorted.
func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// lines parses a lines spec for a request.
func lines(t *testing.T, spec string) []samples.OffsetOrRange {
	t.Helper()
	parsed, err := samples.ParseCSV(spec)
	if err != nil {
		t.Fatalf("parse %q: %v", spec, err)
	}
	return parsed
}
