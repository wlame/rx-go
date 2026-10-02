package sigscan

// The built-in catalogue against a corpus of its own examples: every
// example of every built-in, in catalogue order, with stamped filler
// lines between them. The findings of the default set and of every
// built-in are kept in a golden file, and every storage of the corpus
// (plain, gzip, bzip2, xz, zstd, seekable zstd) with one worker and
// with eight gives the golden answer.
//
// After a deliberate change to the catalogue or the engine, rewrite the
// golden file with:
//
//	go test ./internal/sigscan/ -run=TestBuiltinCorpus -update-corpus

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// updateCorpus rewrites the golden file instead of comparing with it.
var updateCorpus = flag.Bool("update-corpus", false, "rewrite testdata/catalogue/corpus.golden.json")

// corpusGolden is the golden file of the corpus findings.
var corpusGolden = filepath.Join("testdata", "catalogue", "corpus.golden.json")

// fillerLinesBetween is how many filler lines separate two examples.
const fillerLinesBetween = 3

// placedExample is where one example sits in the corpus.
type placedExample struct {
	sig        *signatures.Signature
	match      bool
	start, end int64 // lines, 1-based, inclusive
}

// buildCorpus writes every example of sigs, in order (match examples
// first), each after fillerLinesBetween filler lines, and returns the
// text and where each example went. Filler lines carry a timestamp at
// their start, one second apart from 09:00:00 on the day the examples
// use, and a counter, so no two are equal and none is blank or opens a
// block; the examples' own times, near 10:00, then stand out as a
// burst for the density measure.
func buildCorpus(sigs []*signatures.Signature) (string, []placedExample) {
	var b strings.Builder
	var placed []placedExample
	line := int64(0)
	filler := 0
	write := func(text string) (int64, int64) {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		first := line + 1
		line += int64(strings.Count(text, "\n"))
		b.WriteString(text)
		return first, line
	}
	for _, s := range sigs {
		examples := append(append([]string{}, s.Examples.Match...), s.Examples.NoMatch...)
		for i, ex := range examples {
			for k := 0; k < fillerLinesBetween; k++ {
				write(fmt.Sprintf("2026-03-01 %02d:%02d:%02d INFO filler %d\n", 9+filler/3600, filler/60%60, filler%60, filler))
				filler++
			}
			start, end := write(ex)
			placed = append(placed, placedExample{sig: s, match: i < len(s.Examples.Match), start: start, end: end})
		}
	}
	return b.String(), placed
}

// corpusAnswer is the comparable form of one scan of the corpus: every
// signature's count and findings, by id, and the masked ranges.
type corpusAnswer struct {
	Signatures   map[string]*SignatureResult `json:"signatures"`
	MaskedRanges [][2]int64                  `json:"masked_ranges"`
	TextLines    int64                       `json:"text_lines"`
	TextBytes    int64                       `json:"text_bytes"`
}

// corpusGoldenDoc is the golden file: the answer of each selection.
type corpusGoldenDoc struct {
	Default corpusAnswer `json:"default"`
	All     corpusAnswer `json:"all"`
}

// answerOf turns a result into its comparable form.
func answerOf(res *Result) corpusAnswer {
	return corpusAnswer{Signatures: res.BySignature, MaskedRanges: res.MaskedRanges, TextLines: res.TextLines, TextBytes: res.TextBytes}
}

// corpusVariants are the storages and tunings the corpus is scanned in:
// each with one worker and with eight, chunks and batches of seven
// lines and seekable frames of 300 bytes, so findings cross edges.
func corpusVariants() []variant {
	var out []variant
	for _, workers := range []int{1, 8} {
		plain := defaultTuning()
		plain.chunkLines = 7
		out = append(out, variant{name: fmt.Sprintf("plain/%dw", workers), tune: plain, workers: workers})
		for _, format := range []string{compressedcopy.Gzip, compressedcopy.Bzip2, compressedcopy.Xz, compressedcopy.Zstd} {
			stream := defaultTuning()
			stream.batchLines = 7
			out = append(out, variant{name: fmt.Sprintf("%s/%dw", format, workers), format: format, tune: stream, workers: workers})
		}
		seek := defaultTuning()
		seek.framesPerChunk = 1
		out = append(out, variant{name: fmt.Sprintf("seekable/%dw", workers), format: compressedcopy.SeekableZstd, frameBytes: 300, tune: seek, workers: workers})
	}
	return out
}

// corpusOptions are the options of every corpus scan: the production
// lookback and every built-in redactor, as a command scans with them.
func corpusOptions(cat *signatures.Catalogue) Options {
	return Options{LookbackBytes: 64 << 10, Redactors: cat.Redactors()}
}

// The corpus of every built-in example gives the golden findings, with
// the default set and with every built-in, on every storage with one
// worker and with eight.
func TestBuiltinCorpus_GoldenOnEveryStorage(t *testing.T) {
	cat := builtinCatalogue(t)
	text, _ := buildCorpus(cat.Signatures())
	if i := strings.IndexByte(text, 0); i >= 0 && i < 8<<10 {
		t.Fatalf("a NUL byte at %d is in the first 8 KiB: the corpus would not be text", i)
	}
	selections := map[string][]*signatures.Signature{"default": cat.DefaultSet(), "all": cat.Signatures()}
	ref := variant{name: "reference", tune: defaultTuning(), workers: 1}
	got := corpusGoldenDoc{
		Default: answerOf(scanVariant(t, ref, []byte(text), selections["default"], corpusOptions(cat))),
		All:     answerOf(scanVariant(t, ref, []byte(text), selections["all"], corpusOptions(cat))),
	}
	gotJSON := marshalCorpus(t, got)
	if *updateCorpus {
		if err := os.WriteFile(corpusGolden, gotJSON, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(corpusGolden)
	if err != nil {
		t.Fatalf("%v (run with -update-corpus to write it)", err)
	}
	if !bytes.Equal(gotJSON, want) {
		t.Fatalf("the corpus findings differ from %s; run with -update-corpus after a deliberate change\n%s", corpusGolden, firstDifference(gotJSON, want))
	}
	for _, v := range corpusVariants() {
		for name, sel := range selections {
			t.Run(v.name+"/"+name, func(t *testing.T) {
				res := answerOf(scanVariant(t, v, []byte(text), sel, corpusOptions(cat)))
				wantAnswer := got.Default
				if name == "all" {
					wantAnswer = got.All
				}
				if g, w := marshalCorpus(t, res), marshalCorpus(t, wantAnswer); !bytes.Equal(g, w) {
					t.Fatalf("differs from the reference scan\n%s", firstDifference(g, w))
				}
			})
		}
	}
}

// In the corpus, every match example of a regex, block or run built-in
// holds a finding of its signature, and no finding of its signature
// starts inside one of its no_match examples. (A measure looks at the
// lines around an example, so its examples are checked on their own,
// by the examples check.)
func TestBuiltinCorpus_ExamplesHoldInContext(t *testing.T) {
	cat := builtinCatalogue(t)
	text, placed := buildCorpus(cat.Signatures())
	res := scanOne(t, text, cat.Signatures(), corpusOptions(cat))
	for _, p := range placed {
		if p.sig.Kind == signatures.KindMeasure {
			continue
		}
		var inside int
		for _, f := range res.BySignature[p.sig.ID].Findings {
			if f.StartLine >= p.start && f.StartLine <= p.end {
				inside++
			}
		}
		switch {
		case p.match && inside == 0:
			t.Errorf("%s: no finding in its match example at lines %d-%d", p.sig.ID, p.start, p.end)
		case !p.match && inside > 0:
			t.Errorf("%s: %d finding(s) in its no_match example at lines %d-%d", p.sig.ID, inside, p.start, p.end)
		}
	}
}

// marshalCorpus writes an answer as indented JSON with its signatures
// in id order (encoding/json sorts map keys).
func marshalCorpus(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// firstDifference shows the first line where two documents differ.
func firstDifference(got, want []byte) string {
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return fmt.Sprintf("line %d:\n got  %s\n want %s", i+1, gl, wl)
		}
	}
	return "no line differs"
}

// The corpus holds findings of every built-in group and of most
// built-ins, so the golden file checks the catalogue rather than an
// empty answer.
func TestBuiltinCorpus_GoldenCoversTheCatalogue(t *testing.T) {
	data, err := os.ReadFile(corpusGolden)
	if err != nil {
		t.Fatal(err)
	}
	var doc corpusGoldenDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	cat := builtinCatalogue(t)
	var silent []string
	for _, s := range cat.Signatures() {
		if r := doc.All.Signatures[s.ID]; r == nil || r.Count == 0 {
			silent = append(silent, s.ID)
		}
	}
	sort.Strings(silent)
	if len(silent) > 0 {
		t.Errorf("built-ins without a finding in the corpus: %v", silent)
	}
}
