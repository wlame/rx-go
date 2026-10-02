package sigscan

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/testutil/counting"
)

// pemDef is a redacting block shaped like a PEM private key: the BEGIN
// line is its anchor, and the base64 lines and the END line after it,
// which hold the key material, are its body.
var pemDef = kindSig("pem-key", "block",
	`start: '^-----BEGIN [A-Z ]*PRIVATE KEY-----$'`,
	`continue: '^[A-Za-z0-9+/=]+$'`,
	`end: '^-----END [A-Z ]*PRIVATE KEY-----$'`) + "    redact: true\n"

// b64Def finds a run of 40 base64 bytes or more on a line. It does not
// redact: on a key line its excerpt would show the key.
var b64Def = measureSig("b64", "base64-run", "min_length: 40")

// keyLine is the i-th invented key line of a PEM body: 64 base64
// characters, distinct for every i, which no real key holds.
func keyLine(i int) string {
	return strings.Repeat(fmt.Sprintf("RkFLRS%04dS0VZ", i), 5)[:64]
}

// payload is a base64 run outside any key: not a secret.
var payload = strings.Repeat("QUJD", 15)

// pemText is a log with a PEM block on lines 2 to 6, whose body is
// lines 3 to 6, and a base64 payload outside it on line 7.
func pemText() string {
	return lines(
		"2026-01-15 10:00:00 INFO start",
		"-----BEGIN PRIVATE KEY-----",
		keyLine(1),
		keyLine(2),
		"QUJDRA==",
		"-----END PRIVATE KEY-----",
		"2026-01-15 10:00:01 INFO payload="+payload+" done",
	)
}

// requirePEMBodyMasked checks a scan of pemText: the body's lines are
// the one masked range; the base64-run findings on key lines show only
// the line's length and are marked masked, the one outside the body is
// as it was; no byte of the body is anywhere in the result.
func requirePEMBodyMasked(t *testing.T, res *Result) {
	t.Helper()
	if !slices.Equal(res.MaskedRanges, [][2]int64{{3, 6}}) {
		t.Errorf("masked ranges %v, want [[3 6]]", res.MaskedRanges)
	}
	want := []struct {
		line    int64
		excerpt string
		masked  bool
	}{
		{3, "[masked, 64 bytes]", true},
		{4, "[masked, 64 bytes]", true},
		{7, "2026-01-15 10:00:01 INFO payload=" + payload + " done", false},
	}
	got := res.BySignature["b64"].Findings
	if len(got) != len(want) {
		t.Fatalf("base64-run findings %s, want lines 3, 4 and 7", describe(got))
	}
	for i, w := range want {
		if f := got[i]; f.StartLine != w.line || f.Excerpt != w.excerpt || f.Masked != w.masked {
			t.Errorf("line %d: excerpt %q masked=%v, want %q masked=%v", f.StartLine, f.Excerpt, f.Masked, w.excerpt, w.masked)
		}
	}
	requireNoSecret(t, res, keyLine(1), keyLine(2), "QUJDRA==")
}

// A redacting block that is loaded and not selected masks its body:
// a base64-run finding on a key line shows `[masked, 64 bytes]` and is
// marked masked, the body's lines are the result's masked range, and
// the block itself is not in the result.
func TestScan_UnselectedRedactingBlockMasksTheFindingsOnItsBody(t *testing.T) {
	sigs := loadSigs(t, sigFile(pemDef, b64Def), "pem-key", "b64")
	res := scanOne(t, pemText(), sigs[1:], Options{Redactors: sigs[:1]})
	requirePEMBodyMasked(t, res)
	if _, ok := res.BySignature["pem-key"]; ok {
		t.Error("a redactor that was not selected is in the result")
	}
}

// The same with the redacting block selected: its own finding is there
// too, and the findings on its body are masked the same way.
func TestScan_SelectedRedactingBlockMasksTheFindingsOnItsBody(t *testing.T) {
	sigs := loadSigs(t, sigFile(pemDef, b64Def), "pem-key", "b64")
	res := scanOne(t, pemText(), sigs, Options{Redactors: sigs[:1]})
	requirePEMBodyMasked(t, res)
	if got := spans(res, "pem-key"); !slices.Equal(got, []lineSpan{{2, 6}}) {
		t.Errorf("pem-key findings %v, want lines 2-6", got)
	}
}

// bodyLog is a text with PEM blocks of 1 to 6 body lines (one of them a
// CRLF line), a redacting run of token lines, error lines between them,
// and a PEM block left open at the end of the text, which has no final
// newline. It returns the text and the masked ranges it holds.
func bodyLog() (string, [][2]int64) {
	var ls []string
	var ranges [][2]int64
	add := func(l ...string) { ls = append(ls, l...) }
	add("2026-01-15 10:00:00 INFO start")
	for k := range 5 {
		add("-----BEGIN RSA PRIVATE KEY-----")
		first := int64(len(ls)) + 1
		for j := range k + 1 {
			key := keyLine(10*k + j)
			if k == 2 && j == 1 {
				key += "\r"
			}
			add(key)
		}
		add("-----END RSA PRIVATE KEY-----")
		ranges = append(ranges, [2]int64{first, int64(len(ls))})
		add(fmt.Sprintf("2026-01-15 10:00:%02d ERROR after key %d", k, k))
	}
	add("token=abc1 sent", "token=abc2 sent", "token=abc3 sent")
	ranges = append(ranges, [2]int64{int64(len(ls)) - 1, int64(len(ls))})
	add("2026-01-15 10:00:09 ERROR after tokens", "-----BEGIN PRIVATE KEY-----", keyLine(98), keyLine(99))
	ranges = append(ranges, [2]int64{int64(len(ls)) - 1, int64(len(ls))})
	return strings.Join(ls, "\n"), ranges
}

// bodySigs are the signatures of bodyLog: the redacting block and run,
// then a base64-run, an error line and a token word, none of which
// redacts.
func bodySigs(t *testing.T) (redactors, plain []*signatures.Signature) {
	sigs := loadSigs(t, sigFile(pemDef,
		kindSig("tok-run", "run", `match: 'token=(\w+)'`, "min: 2")+"    redact: true\n",
		b64Def, regexSig("err", `ERROR`), regexSig("tok-word", `token`)),
		"pem-key", "tok-run", "b64", "err", "tok-word")
	return sigs[:2], sigs[2:]
}

// requireBodyExcerpts fails when a finding of a signature that does not
// redact starts in a masked range and its excerpt is not
// `[masked, N bytes]`, or starts outside every range and is masked.
func requireBodyExcerpts(t *testing.T, res *Result, ranges [][2]int64, ids ...string) {
	t.Helper()
	for _, id := range ids {
		for _, f := range res.BySignature[id].Findings {
			inside := slices.ContainsFunc(ranges, func(r [2]int64) bool { return r[0] <= f.StartLine && f.StartLine <= r[1] })
			if inside != (strings.HasPrefix(f.Excerpt, "[masked, ") && f.Masked) {
				t.Errorf("%s at line %d (in a body: %v): excerpt %q masked=%v", id, f.StartLine, inside, f.Excerpt, f.Masked)
			}
		}
	}
}

// Redacting bodies that cross chunk edges give the same ranges and the
// same masked excerpts with 1, 3 and 8 workers, chunks of a line to a
// few bytes, and gzip, bzip2, xz, zstd and seekable zstd copies: with
// the redacting signatures selected, and with them only loaded.
func TestScan_RedactingBodiesAcrossChunkingAndStorage(t *testing.T) {
	redactors, plain := bodySigs(t)
	text, ranges := bodyLog()
	variants := append(chunkVariants(), storageVariants()...)
	for name, sel := range map[string][]*signatures.Signature{
		"redactors selected":     slices.Concat(redactors, plain),
		"redactors not selected": plain,
	} {
		t.Run(name, func(t *testing.T) {
			opts := Options{Redactors: redactors}
			ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, []byte(text), sel, opts)
			if !slices.Equal(ref.MaskedRanges, ranges) {
				t.Fatalf("masked ranges %v, want %v", ref.MaskedRanges, ranges)
			}
			requireBodyExcerpts(t, ref, ranges, "b64", "err", "tok-word")
			if f := ref.BySignature["b64"].Findings[4]; f.StartLine != 13 || f.Excerpt != "[masked, 64 bytes]" {
				t.Errorf("the CRLF key line: line %d excerpt %q, want line 13 as [masked, 64 bytes]", f.StartLine, f.Excerpt)
			}
			requireNoSecret(t, ref, keyLine(0), keyLine(21), keyLine(40), keyLine(99))
			for _, v := range variants {
				t.Run(v.name, func(t *testing.T) {
					requireSameFindings(t, ref, scanVariant(t, v, []byte(text), sel, opts))
				})
			}
		})
	}
}

// A finding of any kind whose first line is a body line shows that
// line's length and is marked masked: a regex, a value measure, a block,
// a run, a long line, a json block and a step back in time. N counts
// the line without its ending ("\r\n" too). A finding that starts on the
// anchor or after the body is not touched.
func TestScan_EveryKindOfFindingOnABodyLineShowsItsLength(t *testing.T) {
	long := strings.Repeat("L ", 1500)
	text := lines(
		stampAt(0)+" boot",
		"BEGIN SECRET",
		stampAt(5*time.Second)+" ERR one\r",
		stampAt(2*time.Second)+" back in time",
		"payload="+payload,
		"RUN a",
		"RUN b",
		"XBLK start",
		"next",
		"{",
		`  "k": 1`,
		"}",
		long,
		"END SECRET",
		stampAt(9*time.Second)+" ERR outside",
	)
	sigs := loadSigs(t, sigFile(
		kindSig("secret", "block", `start: '^BEGIN SECRET'`, `end: '^END SECRET'`)+"    redact: true\n",
		regexSig("err", `ERR`), b64Def,
		kindSig("blk", "block", `start: '^XBLK'`, "lines: 1"),
		kindSig("runs", "run", `match: '^RUN'`, "min: 2"),
		measureSig("long", "long-line", "window: 2, factor: 5, min_bytes: 1000"),
		measureSig("json", "json-block", ""),
		measureSig("back", "time-backwards", ""),
	), "secret", "err", "b64", "blk", "runs", "long", "json", "back")
	res := scanOne(t, text, sigs[1:], Options{Redactors: sigs[:1], LineBytes: 4096})
	if !slices.Equal(res.MaskedRanges, [][2]int64{{3, 14}}) {
		t.Fatalf("masked ranges %v, want [[3 14]]", res.MaskedRanges)
	}
	stamp := len(stampAt(0))
	want := map[string][]string{
		"err":  {fmt.Sprintf("[masked, %d bytes]", stamp+len(" ERR one")), stampAt(9*time.Second) + " ERR outside"},
		"b64":  {fmt.Sprintf("[masked, %d bytes]", len("payload=")+len(payload))},
		"blk":  {"[masked, 10 bytes]"},
		"runs": {"[masked, 5 bytes]"},
		"long": {"[masked, 3000 bytes]"},
		"json": {"[masked, 1 bytes]"},
		"back": {fmt.Sprintf("[masked, %d bytes]", stamp+len(" back in time"))},
	}
	for id, excerpts := range want {
		got := res.BySignature[id].Findings
		if len(got) != len(excerpts) {
			t.Errorf("%s: findings %s, want %d", id, describe(got), len(excerpts))
			continue
		}
		for i, ex := range excerpts {
			inBody := got[i].StartLine <= 14
			if got[i].Excerpt != ex || got[i].Masked != inBody {
				t.Errorf("%s at line %d: excerpt %q masked=%v, want %q masked=%v", id, got[i].StartLine, got[i].Excerpt, got[i].Masked, ex, inBody)
			}
		}
	}
}

// A redacting block masks every line it joined after its anchor: a
// block shorter than its min_lines, which is no finding, still masks its
// body, and trailing blank lines it joined are masked too. A redacting
// run masks the lines after its first: a finding on its first line keeps
// its excerpt with the value masked, one on a later line shows the
// length.
func TestScan_RedactingBodyIsEveryLineJoinedAfterTheAnchor(t *testing.T) {
	sigs := loadSigs(t, sigFile(
		kindSig("short-key", "block", `start: '^BEGIN KEY'`, `continue: '^(\s*|[A-Za-z0-9+/=]+)$'`, "min_lines: 5")+"    redact: true\n",
		kindSig("tok-run", "run", `match: 'token=(\w+)'`, "min: 2")+"    redact: true\n",
		b64Def, kindSig("blanks", "run", "blank: true", "min: 2"), regexSig("tok-word", `token`),
	), "short-key", "tok-run", "b64", "blanks", "tok-word")
	text := lines(
		"BEGIN KEY",
		keyLine(1),
		"",
		"  ",
		"INFO after",
		"token=abcd1 first",
		"token=abcd2 second",
	)
	res := scanOne(t, text, sigs[2:], Options{Redactors: sigs[:2]})
	if !slices.Equal(res.MaskedRanges, [][2]int64{{2, 4}, {7, 7}}) {
		t.Fatalf("masked ranges %v, want [[2 4] [7 7]]", res.MaskedRanges)
	}
	if f := res.BySignature["b64"].Findings; len(f) != 1 || f[0].Excerpt != "[masked, 64 bytes]" {
		t.Errorf("the key line of a block under min_lines: %s", describe(f))
	}
	if f := res.BySignature["blanks"].Findings; len(f) != 1 || f[0].Excerpt != "[masked, 0 bytes]" || !f[0].Masked {
		t.Errorf("trailing blank lines the block joined: %s", describe(f))
	}
	tok := res.BySignature["tok-word"].Findings
	if len(tok) != 2 || tok[0].Excerpt != "to********* first" || tok[1].Excerpt != "[masked, 18 bytes]" {
		t.Errorf("the run's lines: %s", describe(tok))
	}
	requireNoSecret(t, res, keyLine(1), "bcd1", "abcd2")
}

// More PEM blocks than the findings cap: every body is still a masked
// range and every stored finding on a key line is masked; the base64-run
// count stays exact.
func TestScan_MoreRedactingBlocksThanTheCapAreAllMasked(t *testing.T) {
	sigs := loadSigs(t, sigFile(pemDef, b64Def), "pem-key", "b64")
	var ls []string
	var ranges [][2]int64
	for i := range 150 {
		ls = append(ls, "-----BEGIN PRIVATE KEY-----", keyLine(i), "-----END PRIVATE KEY-----")
		ranges = append(ranges, [2]int64{int64(len(ls)) - 1, int64(len(ls))})
	}
	res := scanOne(t, lines(ls...), sigs[1:], Options{Redactors: sigs[:1], Cap: 100})
	if !slices.Equal(res.MaskedRanges, ranges) {
		t.Errorf("masked ranges: %d of them, want the 150 bodies", len(res.MaskedRanges))
	}
	b64 := res.BySignature["b64"]
	if b64.Count != 150 || len(b64.Findings) != 100 {
		t.Errorf("base64-run count %d, stored %d, want 150 and 100", b64.Count, len(b64.Findings))
	}
	for _, f := range b64.Findings {
		if f.Excerpt != "[masked, 64 bytes]" {
			t.Fatalf("line %d: excerpt %q", f.StartLine, f.Excerpt)
		}
	}
}

// Past the bound on the bodies one redacting signature keeps, the scan
// fails closed: the bodies up to the bound are exact, and every line
// from the first body it could not keep up to the end of the text is
// masked, on every chunking and storage.
func TestScan_RedactingBodiesPastTheBoundMaskTheRestOfTheText(t *testing.T) {
	sigs := loadSigs(t, sigFile(pemDef, b64Def), "pem-key", "b64")
	var ls []string
	var ranges [][2]int64
	for i := range 30 {
		ls = append(ls, "-----BEGIN PRIVATE KEY-----", keyLine(i), keyLine(100+i), "-----END PRIVATE KEY-----", "INFO between")
		if i < 9 {
			ranges = append(ranges, [2]int64{int64(len(ls)) - 3, int64(len(ls)) - 1})
		}
	}
	// The tenth body's first line, line 47, to the last line of the text.
	ranges = append(ranges, [2]int64{47, int64(len(ls))})
	text := []byte(lines(ls...))
	opts := Options{Redactors: sigs[:1]}
	for _, v := range append(chunkVariants(), storageVariants()...) {
		v.tune.bodyCap = 10
		t.Run(v.name, func(t *testing.T) {
			res := scanVariant(t, v, text, sigs[1:], opts)
			if !slices.Equal(res.MaskedRanges, ranges) {
				t.Fatalf("masked ranges %v, want %v", res.MaskedRanges, ranges)
			}
			requireBodyExcerpts(t, res, ranges, "b64")
		})
	}
}

// pemBudgetDef is pemDef with at most 6 lines, so a block crossing a
// chunk edge is walked again for at most 7 lines.
var pemBudgetDef = strings.Replace(pemDef, "    redact: true\n", "      max_lines: 6\n    redact: true\n", 1)

// budgetLog writes a log of about 1 MiB with a PEM block every few
// lines, so blocks cross every chunk edge.
func budgetLog(w io.Writer) {
	for i := 0; i < 1<<20/400; i++ {
		_, _ = fmt.Fprintf(w, "2026-01-15 10:00:00 INFO line %d payload=%s\n-----BEGIN PRIVATE KEY-----\n%s\n%s\n-----END PRIVATE KEY-----\n",
			i, payload, keyLine(i), keyLine(i+1))
	}
}

// The walks of the redacting bodies read nothing more than the pass:
// on a plain file, a scan with a redacting block only loaded reads at
// most the lines a block crossing an edge is walked again for (7 lines
// of at most 65 bytes and one small read, per edge) more than the same
// scan without it; a gzip file is read once either way.
func TestScan_RedactingBodiesReadNothingMoreThanThePass(t *testing.T) {
	sigs := loadSigs(t, sigFile(pemBudgetDef, b64Def), "pem-key", "b64")
	dir := t.TempDir()
	plainPath, gzPath := filepath.Join(dir, "app.log"), filepath.Join(dir, "app.log.gz")
	writePlain(t, plainPath, budgetLog)
	writeGzip(t, gzPath, budgetLog)
	read := func(path string, redactors []*signatures.Signature) int64 {
		var counter *counting.ReaderAt
		tune := defaultTuning()
		tune.chunkBytes, tune.readBytes, tune.edgeReadBytes = 64<<10, 4096, 256
		tune.batchLines = 100
		tune.open = countingOpen(&counter)
		if _, err := scanFile(context.Background(), classify(t, path), sigs[1:], Options{Workers: 4, Redactors: redactors}, tune); err != nil {
			t.Fatal(err)
		}
		return counter.Load()
	}
	info, err := os.Stat(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	edges := info.Size() / (64 << 10)
	without, with := read(plainPath, nil), read(plainPath, sigs[:1])
	if limit := without + edges*(7*65+256); with < without || with > limit {
		t.Errorf("plain file: read %d bytes with the redacting block, %d without, at most %d allowed", with, without, limit)
	}
	if gzWithout, gzWith := read(gzPath, nil), read(gzPath, sigs[:1]); gzWith != gzWithout {
		t.Errorf("gzip file: read %d bytes with the redacting block, %d without", gzWith, gzWithout)
	}
}

// The time measures' findings that start on a body line show that
// line's length too: a burst whose first line is in the body (density),
// and a silence whose line before it is (time-gap).
func TestScan_TimeFindingsOnABodyLineShowTheirLength(t *testing.T) {
	var ls []string
	var begin, end int
	tick := func(s int) string { return stampAt(time.Duration(s)*time.Second) + " tick" }
	for s := range 100 {
		if s == 95 {
			ls = append(ls, "BEGIN SECRET")
			begin = len(ls)
		}
		ls = append(ls, tick(s))
		if s == 97 {
			for i := range 50 {
				ls = append(ls, fmt.Sprintf("%s burst %d", stampAt(97*time.Second), i))
			}
		}
	}
	for s := 700; s < 750; s++ {
		ls = append(ls, tick(s))
		if s == 702 {
			ls = append(ls, "END SECRET")
			end = len(ls)
		}
	}
	sigs := loadSigs(t, sigFile(
		kindSig("secret", "block", `start: '^BEGIN SECRET'`, `end: '^END SECRET'`)+"    redact: true\n",
		measureSig("burst", "density", "bucket: 1s, factor: 3, min_count: 10"),
		measureSig("gap", "time-gap", ""),
	), "secret", "burst", "gap")
	res := scanOne(t, lines(ls...), sigs[1:], Options{Redactors: sigs[:1]})
	if want := [][2]int64{{int64(begin) + 1, int64(end)}}; !slices.Equal(res.MaskedRanges, want) {
		t.Fatalf("masked ranges %v, want %v", res.MaskedRanges, want)
	}
	want := fmt.Sprintf("[masked, %d bytes]", len(tick(0)))
	for _, id := range []string{"burst", "gap"} {
		got := res.BySignature[id].Findings
		if len(got) != 1 || got[0].Excerpt != want || !got[0].Masked {
			t.Errorf("%s: %s, want one finding with the excerpt %q", id, describe(got), want)
		}
	}
}

// Masked ranges are sorted and joined where they overlap, touch or hold
// one another; ranges with a line between them stay apart.
func TestJoinRanges(t *testing.T) {
	cases := []struct {
		in, want [][2]int64
	}{
		{nil, [][2]int64{}},
		{[][2]int64{{6, 8}, {3, 5}}, [][2]int64{{3, 8}}},
		{[][2]int64{{3, 5}, {7, 9}}, [][2]int64{{3, 5}, {7, 9}}},
		{[][2]int64{{3, 9}, {4, 5}, {9, 12}}, [][2]int64{{3, 12}}},
		{[][2]int64{{10, 10}, {2, 4}, {3, 6}, {12, 20}}, [][2]int64{{2, 6}, {10, 10}, {12, 20}}},
	}
	for _, tc := range cases {
		if got := joinRanges(tc.in); !slices.Equal(got, tc.want) || got == nil {
			t.Errorf("joinRanges(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A result says how much text the scan read: its line count and its
// length in bytes (the decompressed text of a compressed file), with or
// without a final line break, a CRLF or a byte-order mark; the same on
// every chunking and storage. The text of a finding is never read past
// it.
func TestScan_ResultSaysHowMuchTextItRead(t *testing.T) {
	sigs := loadSigs(t, sigFile(regexSig("err", `ERROR`)), "err")
	cases := []struct {
		name  string
		text  string
		lines int64
	}{
		{"final line break", "INFO a\nERROR b\n", 2},
		{"no final line break", "INFO a\nERROR b", 2},
		{"CRLF and a byte-order mark", "\xEF\xBB\xBFERROR a\r\nINFO b\r\n", 2},
		{"one line", "ERROR", 1},
		{"the mixed text", mixedText(), 36},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, []byte(tc.text), sigs, Options{})
			if ref.TextLines != tc.lines || ref.TextBytes != int64(len(tc.text)) {
				t.Fatalf("text lines %d, bytes %d; want %d and %d", ref.TextLines, ref.TextBytes, tc.lines, len(tc.text))
			}
			for _, v := range append(chunkVariants(), storageVariants()...) {
				t.Run(v.name, func(t *testing.T) {
					requireSameFindings(t, ref, scanVariant(t, v, []byte(tc.text), sigs, Options{}))
				})
			}
		})
	}
}
