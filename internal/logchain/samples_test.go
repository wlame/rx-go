package logchain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// windowChain is a chain whose parts make every edge a window can
// cross: two large parts (plain, gzip), a part of two lines, an empty
// part, a gzip part of three lines and an active file of five, every
// line `<time> LINE <global> part=<name> local=<n>`, a second apart.
//
//	app.log.5      lines 1-100
//	app.log.4.gz   lines 101-300
//	app.log.3      lines 301-302
//	app.log.2      empty
//	app.log.1.gz   lines 303-305
//	app.log        lines 306-310
func windowChain(t *testing.T, withActive bool) builtChain {
	t.Helper()
	type spec struct {
		name, codec string
		count       int
	}
	specs := []spec{
		{"app.log.5", "", 100}, {"app.log.4.gz", compressedcopy.Gzip, 200}, {"app.log.3", "", 2},
		{"app.log.2", "", 0}, {"app.log.1.gz", compressedcopy.Gzip, 3},
	}
	if withActive {
		specs = append(specs, spec{"app.log", "", 5})
	}
	var files []chainFile
	global, at := 1, chainBase
	for _, s := range specs {
		text := timedLines(at, time.Second, global, s.count, s.name)
		files = append(files, chainFile{name: s.name, text: text, codec: s.codec, mtime: at.Add(time.Duration(s.count) * time.Second)})
		global += s.count
		at = at.Add(time.Duration(s.count) * time.Second)
	}
	return buildChain(t, "app.log", files)
}

// Windows inside one part, across two parts (the last 20 lines of one
// and the first 80 of the next), across three small parts and an empty
// one, with context and as ranges, answer what the parts read as one
// file answer, cold and with every part indexed.
func TestSamples_WindowsAcrossParts(t *testing.T) {
	c := windowChain(t, true)
	requireLikeConcatenation(t, c,
		SamplesRequest{Lines: lines(t, "50"), BeforeContext: 3, AfterContext: 3},
		SamplesRequest{Lines: lines(t, "120-130")},
		SamplesRequest{Lines: lines(t, "81-180")},
		SamplesRequest{Lines: lines(t, "299-306")},
		SamplesRequest{Lines: lines(t, "302"), BeforeContext: 3, AfterContext: 3},
		SamplesRequest{Lines: lines(t, "300,301,305"), BeforeContext: 1, AfterContext: 2},
		SamplesRequest{Lines: lines(t, "0,0-0,9999,305-400,400-500"), BeforeContext: 2, AfterContext: 2},
	)
}

// Positions counted back from the end: -1, -50, a range that ends at
// the last line, -1 with more context than the active part has lines,
// and the same on a chain without an active part.
func TestSamples_FromTheEnd(t *testing.T) {
	requireLikeConcatenation(t, windowChain(t, true),
		SamplesRequest{Lines: lines(t, "-1")},
		SamplesRequest{Lines: lines(t, "-50"), BeforeContext: 2, AfterContext: 2},
		SamplesRequest{Lines: lines(t, "300-310")},
		SamplesRequest{Lines: lines(t, "-1"), BeforeContext: 8, AfterContext: 3},
		SamplesRequest{Lines: lines(t, "-310,-311")},
	)
	requireLikeConcatenation(t, windowChain(t, false),
		SamplesRequest{Lines: lines(t, "-1"), BeforeContext: 4, AfterContext: 4},
		SamplesRequest{Lines: lines(t, "-9"), BeforeContext: 2, AfterContext: 2},
	)
}

// Lines addressed by a part and its own numbers: in a ready chain their
// context crosses the part's edges and their keys stay the part's
// numbers, with the global line as target; a range stays in the part;
// a line past the part's end names none.
func TestSamples_PartAddressedInAReadyChain(t *testing.T) {
	c := windowChain(t, true)
	resp := chainSamples(t, c, SamplesRequest{
		Part: "app.log.4.gz", Lines: lines(t, "1,-1,150-999,201"), BeforeContext: 3, AfterContext: 3,
	}, true)
	want := map[string]struct {
		target      int64
		first, last int64
	}{
		"1":       {101, 98, 104},
		"200":     {300, 297, 303},
		"150-999": {250, 250, 300},
		"201":     {-1, 298, 300},
	}
	if keys := keysOf(resp.Samples); !slices.Equal(keys, []string{"1", "150-999", "200", "201"}) {
		t.Fatalf("keys %v", keys)
	}
	for key, w := range want {
		if got := resp.Lines[key]; got != w.target {
			t.Fatalf("lines[%q] = %d, want %d", key, got, w.target)
		}
		got, _ := flatten(resp.Samples[key])
		var expect []string
		for g := w.first; g <= w.last; g++ {
			expect = append(expect, globalLine(c, g))
		}
		if !slices.Equal(got, expect) {
			t.Fatalf("key %q: %q, want global lines %d-%d", key, got, w.first, w.last)
		}
		requirePieceNumbers(t, "part addressed", key, resp)
	}
}

// A part's own numbers up to the largest a request can name give the
// lines the same positions give in the chain's numbers: a range to
// 9223372036854775807 runs to the part's end (the active part's too),
// and a line or a range that starts there names none. The sums that
// turn a part's numbers into the chain's do not wrap, cold and indexed.
func TestSamples_PartNumbersUpToTheLargest(t *testing.T) {
	const largest = "9223372036854775807"
	c := windowChain(t, true)
	cases := []struct {
		part, lines string
		// global is the same lines in the chain's numbers.
		global string
	}{
		{"app.log.4.gz", "5-" + largest, "105-300"},
		{"app.log", "2-" + largest, "307-" + largest},
		{"app.log.4.gz", largest, largest},
		{"app.log.4.gz", largest + "-" + largest, largest + "-" + largest},
		{"app.log", largest, largest},
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			storeIndexes(t, c.dir, c.order...)
		}
		for _, tc := range cases {
			label := fmt.Sprintf("indexed %v, part %s lines %s", indexed, tc.part, tc.lines)
			byPart := chainSamples(t, c, SamplesRequest{Part: tc.part, Lines: lines(t, tc.lines)}, !indexed)
			global := chainSamples(t, c, SamplesRequest{Lines: lines(t, tc.global)}, !indexed)
			got, _ := flatten(byPart.Samples[tc.lines])
			want, _ := flatten(global.Samples[tc.global])
			if (byPart.Samples[tc.lines] == nil) != (global.Samples[tc.global] == nil) || !slices.Equal(got, want) {
				t.Fatalf("%s: %d lines (null %v), the chain's %s gives %d (null %v)", label,
					len(got), byPart.Samples[tc.lines] == nil, tc.global, len(want), global.Samples[tc.global] == nil)
			}
			if byPart.Lines[tc.lines] != global.Lines[tc.global] {
				t.Fatalf("%s: target %d, the chain's %s gives %d", label, byPart.Lines[tc.lines], tc.global, global.Lines[tc.global])
			}
			requirePieceNumbers(t, label, tc.lines, byPart)
		}
	}
}

// Before the chain is ready, a part answers on its own: its context
// stops at its edges and the pieces say so, global numbers are -1, and a
// request by global line is refused until the chain is ready.
func TestSamples_PartAddressedInAPendingChain(t *testing.T) {
	c := windowChain(t, true)
	d := describe(t, c.dir, c.name, Options{})
	if d.Response.State != rxtypes.ChainStatePending {
		t.Fatalf("state %s, want pending", d.Response.State)
	}
	resp, err := Samples(context.Background(), d, SamplesRequest{
		Part: "app.log.4.gz", Lines: lines(t, "1,200,-1"), BeforeContext: 3, AfterContext: 3,
		IndexLoader: samples.StoredIndex,
	}, resolveReader)
	if err != nil {
		t.Fatalf("pending part samples: %v", err)
	}
	cases := map[string]struct {
		first, count    int64
		partStart, ends bool
	}{
		"1":   {1, 4, true, false},
		"200": {197, 4, false, true},
	}
	if keys := keysOf(resp.Samples); !slices.Equal(keys, []string{"1", "200"}) {
		t.Fatalf("keys %v", keys)
	}
	for key, w := range cases {
		pieces := resp.Samples[key]
		if len(pieces) != 1 {
			t.Fatalf("key %q: %d pieces", key, len(pieces))
		}
		p := pieces[0]
		if p.Part != "app.log.4.gz" || p.FirstLocalLine != w.first || int64(len(p.Lines)) != w.count ||
			p.PartStart != w.partStart || p.PartEnd != w.ends || p.FirstGlobalLine != -1 || resp.Lines[key] != -1 {
			t.Fatalf("key %q: %s (lines[key] %d)", key, jsonOf(t, p), resp.Lines[key])
		}
		for i, line := range p.Lines {
			if want := globalLine(c, 100+w.first+int64(i)); line != want {
				t.Fatalf("key %q line %d: %q, want %q", key, i, line, want)
			}
		}
	}
	_, err = Samples(context.Background(), d, SamplesRequest{Lines: lines(t, "5"), IndexLoader: samples.StoredIndex}, resolveReader)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("a global line on a pending chain: %v, want ErrNotReady", err)
	}
	_, err = Samples(context.Background(), d, SamplesRequest{Timestamps: []string{"2026-10-01"}, IndexLoader: samples.StoredIndex}, resolveReader)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("a time on a pending chain: %v, want ErrNotReady", err)
	}
}

// A part that is not a member, times with a part, both or neither kind
// of position, and an invalid chain are refused before anything is
// read.
func TestSamples_Refusals(t *testing.T) {
	c := windowChain(t, true)
	d := describe(t, c.dir, c.name, Options{Scan: true})
	never := func(context.Context, Part, samples.Request) (*rxtypes.SamplesResponse, error) {
		t.Fatalf("a refused request read a part")
		return nil, nil
	}
	cases := []struct {
		name string
		req  SamplesRequest
		want error
	}{
		{"not a member", SamplesRequest{Part: "app.log.9", Lines: lines(t, "1")}, ErrNotAPart},
		{"a path, not a name", SamplesRequest{Part: filepath.Join(c.dir, "app.log.3"), Lines: lines(t, "1")}, ErrNotAPart},
		{"times with a part", SamplesRequest{Part: "app.log.3", Timestamps: []string{"2026-10-01"}}, ErrSamplesRequest},
		{"both", SamplesRequest{Lines: lines(t, "1"), Timestamps: []string{"2026-10-01"}}, ErrSamplesRequest},
		{"neither", SamplesRequest{}, ErrSamplesRequest},
		{"negative context", SamplesRequest{Lines: lines(t, "1"), BeforeContext: -1}, ErrSamplesRequest},
	}
	for _, tc := range cases {
		if _, err := Samples(context.Background(), d, tc.req, never); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}

	invalid := buildChain(t, "app.log", []chainFile{
		{name: "app.log.1", text: []byte("no timestamp here\n")},
		{name: "app.log", text: timedLines(chainBase, time.Second, 2, 2, "app.log")},
	})
	di := describe(t, invalid.dir, invalid.name, Options{Scan: true})
	_, err := Samples(context.Background(), di, SamplesRequest{Part: "app.log", Lines: lines(t, "1")}, never)
	if !errors.Is(err, ErrChainInvalid) {
		t.Fatalf("an invalid chain: %v, want ErrChainInvalid", err)
	}
}

// RX_SAMPLES_MAX_LINES and RX_SAMPLES_MAX_BYTES bound the whole answer:
// a window whose lines fit in each part's read but not in their sum is
// refused, with samples' own errors.
func TestSamples_LimitsBoundTheWholeAnswer(t *testing.T) {
	c := windowChain(t, true)
	d := describe(t, c.dir, c.name, Options{Scan: true})
	read := func(req SamplesRequest) error {
		req.IndexLoader = samples.StoredIndex
		_, err := Samples(context.Background(), d, req, resolveReader)
		return err
	}
	// 296-305 holds 5 lines of app.log.4.gz, 2 of app.log.3 and 3 of
	// app.log.1.gz: each part's share fits in 6 lines, their sum does not.
	if err := read(SamplesRequest{Lines: lines(t, "296-305"), MaxLines: 6}); !errors.Is(err, samples.ErrTooManyLines) {
		t.Fatalf("10 lines with a limit of 6: %v", err)
	}
	if err := read(SamplesRequest{Lines: lines(t, "296-305"), MaxLines: 10}); err != nil {
		t.Fatalf("10 lines with a limit of 10: %v", err)
	}
	// Two keys that share their lines hold them twice.
	if err := read(SamplesRequest{Lines: lines(t, "301,302"), BeforeContext: 2, AfterContext: 2, MaxLines: 8}); !errors.Is(err, samples.ErrTooManyLines) {
		t.Fatalf("two windows of 5 lines with a limit of 8: %v", err)
	}
	line := int64(len(globalLine(c, 296)))
	if err := read(SamplesRequest{Lines: lines(t, "296-305"), MaxBytes: 9 * line}); !errors.Is(err, samples.ErrTooManyBytes) {
		t.Fatalf("10 lines in the bytes of 9: %v", err)
	}

	// Each part is read with what the answer has room for still, so no
	// read holds more than the limit: 6 lines for app.log.4.gz, then the
	// 1 left for app.log.3, whose read refuses its second line.
	var given []int
	limited := func(ctx context.Context, part Part, req samples.Request) (*rxtypes.SamplesResponse, error) {
		given = append(given, req.MaxLines)
		return samples.Resolve(ctx, req)
	}
	_, err := Samples(context.Background(), d, SamplesRequest{Lines: lines(t, "296-305"), MaxLines: 6, IndexLoader: samples.StoredIndex}, limited)
	if !errors.Is(err, samples.ErrTooManyLines) || !slices.Equal(given, []int{6, 1}) {
		t.Fatalf("limits given to the reads %v, error %v; want [6 1] and too many lines", given, err)
	}
}

// Lines appended to the active part between two calls keep every
// earlier line's global number, and -1 reaches the new last line; the
// fingerprint stays.
func TestSamples_TheActivePartGrows(t *testing.T) {
	c := windowChain(t, true)
	req := SamplesRequest{Lines: lines(t, "300,308,-1"), BeforeContext: 1, AfterContext: 1}
	before := chainSamples(t, c, req, true)
	d0 := describe(t, c.dir, c.name, Options{Scan: true})

	f, err := os.OpenFile(filepath.Join(c.dir, "app.log"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open the active file: %v", err)
	}
	added := timedLines(chainBase.Add(time.Hour), time.Second, 311, 4, "app.log")
	if _, err := f.Write(added); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	after := chainSamples(t, c, req, true)
	if d1 := describe(t, c.dir, c.name, Options{Scan: true}); d1.Response.Fingerprint != d0.Response.Fingerprint {
		t.Fatalf("the fingerprint changed with growth: %s, %s", d0.Response.Fingerprint, d1.Response.Fingerprint)
	}
	for _, key := range []string{"300", "308"} {
		if a, b := jsonOf(t, before.Samples[key]), jsonOf(t, after.Samples[key]); a != b || after.Lines[key] != before.Lines[key] {
			t.Fatalf("key %s changed with growth:\n%s\n%s", key, a, b)
		}
	}
	if before.Lines["310"] != 310 {
		t.Fatalf("-1 before the append: lines %v", before.Lines)
	}
	got, _ := flatten(after.Samples["314"])
	newLines := splitLines(added)
	if after.Lines["314"] != 314 || !slices.Equal(got, newLines[2:]) {
		t.Fatalf("-1 after the append: lines %v, samples %q, want %q", after.Lines, got, newLines[2:])
	}
}

// Every storage a part can have, a part that ends without a newline,
// and an active file answer as their concatenation, cold and indexed.
func TestSamples_MixedStorages(t *testing.T) {
	dir, files, order := mixedCodecChain(t)
	c := builtChain{dir: dir, name: "app.log", files: files, order: order}
	c.concat = concatenation(t, dir, files, order)
	requireLikeConcatenation(t, c,
		SamplesRequest{Lines: lines(t, "38-44,64-67,94-97,145-148,157-160,-1,1")},
		SamplesRequest{Lines: lines(t, "40,65,96,146,158"), BeforeContext: 2, AfterContext: 2},
		SamplesRequest{Timestamps: []string{"2026-10-01 00:39:30", "2026-10-01 01:35:00..2026-10-01 02:28:00", "2026-10-01T00:00"}},
	)
}

// Each piece's lines are what `rx samples PART --lines=A-B` gives for
// the part's own numbers of the piece.
func TestSamples_PiecesAreTheirPartsLines(t *testing.T) {
	c := windowChain(t, true)
	resp := chainSamples(t, c, SamplesRequest{Lines: lines(t, "81-180,299-308")}, true)
	for key, pieces := range resp.Samples {
		for _, p := range pieces {
			end := p.FirstLocalLine + int64(len(p.Lines)) - 1
			single, err := samples.Resolve(context.Background(), samples.Request{
				Path: filepath.Join(c.dir, p.Part), Lines: []samples.OffsetOrRange{{Start: p.FirstLocalLine, End: &end}},
				IndexLoader: samples.NoIndex,
			})
			if err != nil {
				t.Fatalf("rx samples %s: %v", p.Part, err)
			}
			if got := single.Samples[samples.OffsetOrRange{Start: p.FirstLocalLine, End: &end}.Key()]; !slices.Equal(got, p.Lines) {
				t.Fatalf("key %s piece of %s: %q, the part gives %q", key, p.Part, p.Lines, got)
			}
		}
	}
}

// globalLine is the text of global line g of a chain whose lines carry
// their global number (timedLines), read from the concatenation.
func globalLine(c builtChain, g int64) string {
	text, err := os.ReadFile(c.concat)
	if err != nil {
		panic(err)
	}
	all := splitLines(text)
	return all[g-1]
}

// splitLines splits a text into its lines without their line breaks.
func splitLines(text []byte) []string {
	var out []string
	start := 0
	for i, b := range text {
		if b == '\n' {
			out = append(out, string(text[start:i]))
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, string(text[start:]))
	}
	return out
}

// SECURITY: the pieces a request plans are bounded before any part is
// read. A piece of a frozen part holds at least one line, so a request
// that plans more of them than MaxLines is refused at once, however
// many positions it names.
func TestSamples_PlanningIsBoundedByTheLimit(t *testing.T) {
	c := windowChain(t, true)
	d := describe(t, c.dir, c.name, Options{Scan: true})
	reads := 0
	counting := func(ctx context.Context, part Part, req samples.Request) (*rxtypes.SamplesResponse, error) {
		reads++
		return samples.Resolve(ctx, req)
	}
	var spec []string
	for g := 1; g <= 300; g++ {
		spec = append(spec, fmt.Sprintf("%d-%d", g, g))
	}
	_, err := Samples(context.Background(), d, SamplesRequest{
		Lines: lines(t, strings.Join(spec, ",")), MaxLines: 100, IndexLoader: samples.StoredIndex,
	}, counting)
	if !errors.Is(err, samples.ErrTooManyLines) || reads != 0 {
		t.Fatalf("300 one-line ranges with a limit of 100: %v after %d reads", err, reads)
	}
}
