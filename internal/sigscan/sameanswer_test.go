package sigscan

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// mixedCatalogue has a signature of every kind and form, so one scan
// exercises the gate, regex values, redaction, blocks in both forms,
// min_lines, max_lines and the three runs.
var mixedCatalogue = sigFile(
	regexSig("err-code", `ERROR code=(\d+)`, "capture: 1"),
	regexSig("api-key", `key=(AKIA[A-Z0-9]{16})`, "capture: 1", "redact: true"),
	kindSig("tb", "block", `start: '^Traceback \(most recent call last\):'`, `continue: '^\s'`, `end: '^[A-Za-z_][\w.]*(?::|$)'`),
	chainedExceptionSig,
	kindSig("begin-end", "block", `start: '^BEGIN'`, `end: '^END'`, "max_lines: 5", "min_lines: 2"),
	kindSig("head-two", "block", `start: '^HEAD'`, "lines: 2"),
	kindSig("blanks", "run", "blank: true", "min: 2"),
	kindSig("repeats", "run", "same: true", "min: 2"),
	kindSig("errors", "run", "match: 'ERROR'", "min: 2"),
	kindSig("pem", "block", `start: '^-----BEGIN [A-Z ]*PRIVATE KEY-----$'`, `continue: '^[A-Za-z0-9+/=]+$'`,
		`end: '^-----END [A-Z ]*PRIVATE KEY-----$'`, "max_lines: 5")+"    redact: true\n",
	kindSig("tok-run", "run", `match: 'token=(\w+)'`, "min: 2")+"    redact: true\n",
)

// mixedIDs are the ids of mixedCatalogue.
var mixedIDs = []string{"err-code", "api-key", "tb", "py-chain", "begin-end", "head-two", "blanks", "repeats", "errors", "pem", "tok-run"}

// mixedLineBytes is the view size of the same-answer tests: small, so
// that generated long lines exceed it.
const mixedLineBytes = 1024

// mixedText is a fixed text with every shape the catalogue finds, CRLF
// lines, a byte-order mark and no final newline.
func mixedText() string {
	long := strings.Repeat("x", 1500)
	return "\xEF\xBB\xBFTraceback (most recent call last):\n" + strings.Join([]string{
		"  File \"app.py\", line 1",
		"ValueError: one",
		"INFO ok",
		"ERROR code=7 first",
		"ERROR code=8 second\r",
		"",
		"\r",
		"BEGIN",
		"BEGIN inner",
		"END",
		"HEAD",
		"key=AKIAIOSFODNN7EXAMPLE",
		long,
		long,
		"same",
		"same",
		"Traceback (most recent call last):",
		"  File \"app.py\", line 3",
		"ValueError: bad input",
		"",
		"During handling of the above exception, another exception occurred:",
		"",
		"Traceback (most recent call last):",
		"  File \"app.py\", line 5",
		"KeyError: 'x'",
		"",
		"",
		"BEGIN",
		"x",
		"y",
		"z",
		"w",
		"v",
		"END",
		"ERROR code=9",
	}, "\n") // no final newline
}

// variant is one way of reading a text: a storage and a tuning.
type variant struct {
	name   string
	format string // "" plain, else a compressedcopy format
	// frameBytes > 0 writes a seekable file with frames of this size.
	frameBytes int
	tune       tuning
	workers    int
}

// chunkVariants are the plain-file chunkings of the same-answer rule:
// 1, 3 and 8 workers with chunks of 1, 2, 7 and 64 lines, and nominal
// chunks of a few bytes, which put chunk starts inside lines.
func chunkVariants() []variant {
	var out []variant
	for _, workers := range []int{1, 3, 8} {
		for _, n := range []int{1, 2, 7, 64} {
			t := defaultTuning()
			t.chunkLines = n
			out = append(out, variant{name: fmt.Sprintf("plain/%dw/%d-lines", workers, n), tune: t, workers: workers})
		}
		for _, b := range []int64{1, 13, 200} {
			t := defaultTuning()
			t.chunkBytes = b
			t.readBytes, t.edgeReadBytes = 7, 3
			out = append(out, variant{name: fmt.Sprintf("plain/%dw/%d-bytes", workers, b), tune: t, workers: workers})
		}
	}
	return out
}

// storageVariants are the compressed copies: streams in batches of 1,
// 2, 7 and 64 lines and in tiny buffers (lines longer than a buffer),
// and seekable files with frames that cut lines, one to seven frames a
// chunk.
func storageVariants() []variant {
	var out []variant
	for _, format := range []string{compressedcopy.Gzip, compressedcopy.Bzip2, compressedcopy.Xz, compressedcopy.Zstd} {
		for _, n := range []int{1, 2, 7, 64} {
			t := defaultTuning()
			t.batchLines = n
			out = append(out, variant{name: fmt.Sprintf("%s/%d-lines", format, n), format: format, tune: t, workers: 3})
		}
		t := defaultTuning()
		t.batchBytes = 16
		out = append(out, variant{name: format + "/16-byte-buffers", format: format, tune: t, workers: 8})
	}
	for _, frameBytes := range []int{7, 50, 300} {
		for _, frames := range []int{1, 2, 7} {
			t := defaultTuning()
			t.framesPerChunk = frames
			out = append(out, variant{name: fmt.Sprintf("seekable/%d-byte-frames/%d-per-chunk", frameBytes, frames),
				format: compressedcopy.SeekableZstd, frameBytes: frameBytes, tune: t, workers: 3})
		}
	}
	return out
}

// scanVariant writes text as the variant stores it and scans it.
func scanVariant(t *testing.T, v variant, text []byte, sigs []*signatures.Signature, opts Options) *Result {
	t.Helper()
	var data []byte
	switch {
	case v.frameBytes > 0:
		data = seekablefile.Encode(t, seekablefile.SplitEvery(text, v.frameBytes))
	case v.format != "":
		data = compressedcopy.Encode(t, v.format, text)
		if data == nil {
			t.Skipf("no encoder for %s on this host", v.format)
		}
	default:
		data = text
	}
	opts.Workers = v.workers
	res, err := scanFile(context.Background(), writeFile(t, "app.log", data), sigs, opts, v.tune)
	if err != nil {
		t.Fatal(err)
	}
	requireValidExcerpts(t, res)
	return res
}

// requireSameFindings fails when two results differ in any finding or
// count, in their masked ranges, or in how much text they read.
func requireSameFindings(t *testing.T, want, got *Result) {
	t.Helper()
	if !slices.Equal(want.MaskedRanges, got.MaskedRanges) {
		t.Fatalf("masked ranges differ:\nwant %v\ngot  %v", want.MaskedRanges, got.MaskedRanges)
	}
	if want.TextLines != got.TextLines || want.TextBytes != got.TextBytes {
		t.Fatalf("text read: want %d lines and %d bytes, got %d and %d", want.TextLines, want.TextBytes, got.TextLines, got.TextBytes)
	}
	for id, w := range want.BySignature {
		g := got.BySignature[id]
		if g == nil || !reflect.DeepEqual(w, g) {
			t.Fatalf("%s differs:\nwant count %d %s\ngot  count %d %s", id, w.Count, describe(w.Findings), g.Count, describe(g.Findings))
		}
	}
}

// describe prints findings compactly.
func describe(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "[%d-%d %d-%d %q v=%d-%d] ", f.StartLine, f.EndLine, f.StartOffset, f.EndOffset, f.Excerpt, deref(f.ValueStart), deref(f.ValueEnd))
	}
	return b.String()
}

// mixedGoldens are what the mixed text holds.
var mixedGoldens = map[string][]lineSpan{
	"err-code":  {{5, 5}, {6, 6}, {36, 36}},
	"api-key":   {{13, 13}},
	"tb":        {{1, 3}, {18, 20}, {24, 26}},
	"py-chain":  {{1, 3}, {18, 26}},
	"begin-end": {{9, 11}, {29, 33}},
	"head-two":  {{12, 14}},
	"blanks":    {{7, 8}, {27, 28}},
	"repeats":   {{7, 8}, {14, 15}, {16, 17}, {27, 28}},
	"errors":    {{5, 6}},
}

// The mixed fixture gives its goldens, and the same answer
// with 1, 3 and 8 workers and chunks of 1, 2, 7 and 64 lines (and of a
// few bytes).
func TestScan_SameAnswerAcrossChunking(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	text := []byte(mixedText())
	opts := Options{LineBytes: mixedLineBytes, Redactors: sigs}
	ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, text, sigs, opts)
	for id, want := range mixedGoldens {
		if got := spans(ref, id); !slices.Equal(got, want) {
			t.Errorf("%s: findings %v, want %v", id, got, want)
		}
	}
	for _, v := range chunkVariants() {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, ref, scanVariant(t, v, text, sigs, opts))
		})
	}
}

// The same goldens on gzip, bzip2, xz, zstd and seekable zstd
// copies, in any batching or frame layout.
func TestScan_SameAnswerOnCompressedCopies(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	text := []byte(mixedText())
	opts := Options{LineBytes: mixedLineBytes, Redactors: sigs}
	ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, text, sigs, opts)
	for _, v := range storageVariants() {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, ref, scanVariant(t, v, text, sigs, opts))
		})
	}
}

// A CRLF line's "\r" is not in the view (a blank run sees
// "\r\n" lines as empty); a last line without a newline is a line whose
// end offset is the end of the text; a UTF-8 byte-order mark is not in
// line 1's view, so ^Traceback matches line 1, and offsets count it.
// Every chunking and every compressed copy agree (the same-answer tests
// above run this text too).
func TestScan_CRLFNoFinalNewlineAndBOM(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	text := mixedText()
	res := scanOne(t, text, sigs, Options{LineBytes: mixedLineBytes, Redactors: sigs})

	tb := res.BySignature["tb"].Findings[0]
	if tb.StartLine != 1 || tb.StartOffset != 0 || tb.Excerpt != "Traceback (most recent call last):" {
		t.Errorf("BOM line: start %d offset %d excerpt %q", tb.StartLine, tb.StartOffset, tb.Excerpt)
	}
	// Line 2 starts after the 3-byte mark and the 34-byte line 1 and its '\n'.
	if want := int64(3 + 34 + 1); res.BySignature["tb"].Findings[0].EndOffset <= want {
		t.Errorf("offsets do not count the mark: end offset %d", res.BySignature["tb"].Findings[0].EndOffset)
	}
	blank := res.BySignature["blanks"].Findings[0]
	if blank.StartLine != 7 || blank.EndLine != 8 {
		t.Errorf("a \\r\\n line is not blank: run %d-%d", blank.StartLine, blank.EndLine)
	}
	errCode := res.BySignature["err-code"].Findings[1]
	if strings.Contains(errCode.Excerpt, "\r") {
		t.Errorf("the \\r of a CRLF line is in the view: %q", errCode.Excerpt)
	}
	last := res.BySignature["err-code"].Findings[2]
	if last.StartLine != 36 || last.EndOffset != int64(len(text)) {
		t.Errorf("last line without newline: line %d end offset %d, want 36 and %d", last.StartLine, last.EndOffset, len(text))
	}
}

// A property test plants blocks, runs and long lines across
// chunk edges (random texts from fixed seeds) and checks that every
// chunking and storage gives the one-chunk answer.
func TestScan_SameAnswerProperty(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	opts := Options{LineBytes: mixedLineBytes, Redactors: sigs}
	variants := append(chunkVariants(), storageVariants()...)
	for seed := uint64(1); seed <= 6; seed++ {
		text := []byte(randomText(seed, 160))
		ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, text, sigs, opts)
		for i, v := range variants {
			// Each seed runs a third of the variants, all of them across
			// the seeds, to keep the test fast.
			if i%3 != int(seed%3) {
				continue
			}
			t.Run(fmt.Sprintf("seed-%d/%s", seed, v.name), func(t *testing.T) {
				requireSameFindings(t, ref, scanVariant(t, v, text, sigs, opts))
			})
		}
	}
}

// randomText builds n lines from pieces that start, continue and end
// every kind of finding, with CRLF endings, long lines, a byte-order
// mark and a missing final newline chosen by the seed.
func randomText(seed uint64, n int) string {
	r := rand.New(rand.NewPCG(seed, seed*7919))
	pieces := [][]string{
		{"INFO ok"}, {"ERROR code=1 boom"}, {"ERROR code=2", "ERROR code=3"},
		{""}, {"", ""}, {"  ", "\t", ""},
		{"Traceback (most recent call last):", "  File \"a.py\", line 1", "ValueError: v"},
		{"Traceback (most recent call last):", "  File \"a.py\", line 2"},
		{"During handling of the above exception, another exception occurred:", ""},
		{"BEGIN", "a", "END"}, {"BEGIN"}, {"END"},
		{"HEAD", "h1"}, {"same", "same", "same"},
		{"key=AKIAIOSFODNN7EXAMPLE"}, {strings.Repeat("L", 1100)}, {strings.Repeat("L", 1100), strings.Repeat("L", 1100)},
		{"-----BEGIN PRIVATE KEY-----", "RkFLRUtFWQ==", "-----END PRIVATE KEY-----"}, {"-----BEGIN PRIVATE KEY-----", "RkFLRQ=="},
		{"token=abc1 ERROR", "token=abc2"},
	}
	var b strings.Builder
	if r.IntN(2) == 0 {
		b.WriteString("\xEF\xBB\xBF")
	}
	written := 0
	for written < n {
		for _, l := range pieces[r.IntN(len(pieces))] {
			b.WriteString(l)
			if r.IntN(5) == 0 {
				b.WriteString("\r")
			}
			b.WriteString("\n")
			written++
		}
	}
	text := b.String()
	if r.IntN(2) == 0 {
		text = strings.TrimSuffix(text, "\n")
	}
	return text
}

// A byte-order mark before a line longer than the view: the view is the
// line's first bytes after the mark, on every path (a stream keeps the
// mark's 3 bytes more of a long line than the view).
func TestScan_ByteOrderMarkBeforeALongLine(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	long := strings.Repeat("a", 2000)
	text := []byte("\xEF\xBB\xBF" + long + "\n" + long + "\n")
	opts := Options{LineBytes: mixedLineBytes, Redactors: sigs}
	ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, text, sigs, opts)
	if got := spans(ref, "repeats"); !slices.Equal(got, []lineSpan{{1, 2}}) {
		t.Fatalf("a marked line equal to the next after the mark: findings %v, want one run of lines 1-2", got)
	}
	for _, v := range append(chunkVariants(), storageVariants()...) {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, ref, scanVariant(t, v, text, sigs, opts))
		})
	}
}

// An empty text has no line and no finding, on every path; every
// selected signature is in the result with a count of 0.
func TestScan_EmptyText(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	for _, v := range append(chunkVariants()[:1], storageVariants()...) {
		t.Run(v.name, func(t *testing.T) {
			res := scanVariant(t, v, nil, sigs, Options{})
			for _, id := range mixedIDs {
				got := res.BySignature[id]
				if got == nil || got.Count != 0 || got.Findings == nil || len(got.Findings) != 0 {
					t.Errorf("%s: %+v, want a count of 0 and no findings", id, got)
				}
			}
		})
	}
}

// Every fixture of the scan's rules gives its findings, and the same
// answer on every chunking and storage.
func TestScan_EveryScenarioFixtureAcrossChunking(t *testing.T) {
	variants := append(chunkVariants(), storageVariants()...)
	for _, fx := range scenarioFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			sigs := loadSigs(t, sigFile(fx.def), fx.ids...)
			text := []byte(fx.text)
			opts := fx.optionsFor(sigs)
			ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, text, sigs, opts)
			fx.check(t, ref)
			for _, v := range variants {
				t.Run(v.name, func(t *testing.T) {
					requireSameFindings(t, ref, scanVariant(t, v, text, sigs, opts))
				})
			}
		})
	}
}
