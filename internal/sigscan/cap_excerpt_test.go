package sigscan

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// 25 findings with a cap of 10 keep the first 10 by line and
// count 25, under every chunking and storage.
func TestScan_CapKeepsTheFirstByLine(t *testing.T) {
	sigs := loadSigs(t, sigFile(
		regexSig("err", `ERROR`),
		kindSig("pair", "block", `start: '^BEGIN'`, `end: '^END'`),
	), "err", "pair")
	var ls []string
	for i := range 25 {
		ls = append(ls, "INFO", "ERROR "+strings.Repeat("e", i), "BEGIN", "END")
	}
	text := []byte(lines(ls...))
	opts := Options{Cap: 10}
	ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, text, sigs, opts)
	wantErr, wantPair := []lineSpan{}, []lineSpan{}
	for i := range 10 {
		n := int64(4 * i)
		wantErr = append(wantErr, lineSpan{n + 2, n + 2})
		wantPair = append(wantPair, lineSpan{n + 3, n + 4})
	}
	for id, want := range map[string][]lineSpan{"err": wantErr, "pair": wantPair} {
		if got := spans(ref, id); !slices.Equal(got, want) || ref.BySignature[id].Count != 25 {
			t.Errorf("%s: findings %v count %d, want %v and 25", id, got, ref.BySignature[id].Count, want)
		}
	}
	for _, v := range append(chunkVariants(), storageVariants()...) {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, ref, scanVariant(t, v, text, sigs, opts))
		})
	}
}

// When a block crosses a chunk edge and swallows more of the next
// chunk's own findings than the chunk stored (the chunk hit the cap
// inside the block), the merge regenerates the findings after the block
// itself, so the first Cap by line are still the answer's.
func TestScan_CapAfterABlockThatSwallowsStoredFindings(t *testing.T) {
	sigs := loadSigs(t, sigFile(kindSig("xs", "block",
		"states:",
		`  - {state: start, regex: '^X', end: true}`,
		`  - {state: start, regex: '^S', next: body}`,
		`  - {state: body, regex: '^X', next: body}`,
		`  - {state: body, regex: '^E', end: true}`,
	)), "xs")
	ls := []string{"X", "X", "X", "INFO", "INFO", "INFO", "S"}
	for range 25 {
		ls = append(ls, "X")
	}
	ls = append(ls, "E")
	for range 20 {
		ls = append(ls, "X")
	}
	text := []byte(lines(ls...))
	opts := Options{Cap: 10}
	want := []lineSpan{{1, 1}, {2, 2}, {3, 3}, {7, 33}, {34, 34}, {35, 35}, {36, 36}, {37, 37}, {38, 38}, {39, 39}}
	ref := scanVariant(t, variant{name: "reference", tune: defaultTuning(), workers: 1}, text, sigs, opts)
	if got := spans(ref, "xs"); !slices.Equal(got, want) || ref.BySignature["xs"].Count != 24 {
		t.Fatalf("findings %v count %d, want %v and 24", got, ref.BySignature["xs"].Count, want)
	}
	for _, v := range append(chunkVariants(), storageVariants()...) {
		t.Run(v.name, func(t *testing.T) {
			requireSameFindings(t, ref, scanVariant(t, v, text, sigs, opts))
		})
	}
}

// An excerpt is the first line's view cut to 200 bytes on a
// UTF-8 boundary.
func TestScan_ExcerptCutOnACharacterBoundary(t *testing.T) {
	sigs := loadSigs(t, sigFile(regexSig("err", `ERROR`)), "err")
	head := "ERROR " + strings.Repeat("x", 193) // 199 bytes
	res := scanOne(t, lines(head+"é and more", "ERROR short"), sigs, Options{})
	got := res.BySignature["err"].Findings
	if got[0].Excerpt != head {
		t.Errorf("excerpt is %d bytes ending %q, want the 199 bytes before the 2-byte character", len(got[0].Excerpt), got[0].Excerpt[len(got[0].Excerpt)-3:])
	}
	if !utf8.ValidString(got[0].Excerpt) || got[1].Excerpt != "ERROR short" {
		t.Errorf("excerpts %q, %q", got[0].Excerpt, got[1].Excerpt)
	}
}

// A redacted value is masked in the excerpt (4 characters
// kept, the rest '*', at most 12), for every kind, and the unmasked
// bytes appear nowhere in the result.
func TestScan_RedactedValueNeverLeavesTheMatcher(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	sigs := loadSigs(t, sigFile(
		regexSig("key-value", `key=(AKIA[A-Z0-9]{16})`, "capture: 1", "redact: true"),
		kindSig("key-block", "block", `start: 'AKIA[A-Z0-9]{16}'`, `continue: '^ '`)+"    redact: true\n",
		kindSig("key-run", "run", `match: 'AKIA[A-Z0-9]{16}'`, "min: 2")+"    redact: true\n",
	), "key-value", "key-block", "key-run")
	text := lines("export key="+secret+" and again "+secret, "key="+secret, "  more")
	res := scanOne(t, text, sigs, Options{})
	masked := "AKIA************"
	for _, id := range []string{"key-value", "key-block", "key-run"} {
		got := res.BySignature[id].Findings
		if len(got) == 0 {
			t.Fatalf("%s: no finding", id)
		}
		f := got[0]
		if !f.Masked || !strings.Contains(f.Excerpt, masked) {
			t.Errorf("%s: excerpt %q masked=%v, want the value as %s", id, f.Excerpt, f.Masked, masked)
		}
	}
	if v := res.BySignature["key-value"].Findings[0]; deref(v.ValueStart) != 11 || deref(v.ValueEnd) != 31 {
		t.Errorf("value span %d-%d, want 11-31", deref(v.ValueStart), deref(v.ValueEnd))
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), secret[4:]) {
		t.Errorf("the unmasked value is in the result: %s", encoded)
	}
}

// requireNoSecret fails when any of the secrets is anywhere in the
// result, encoded as JSON.
func requireNoSecret(t *testing.T, res *Result, secrets ...string) {
	t.Helper()
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(string(encoded), s) {
			t.Errorf("the unmasked value %q is in the result: %s", s, encoded)
		}
	}
}

// Every match of a redacting anchor that reaches the excerpt is masked:
// a second, different key on the line too, for regex, block and match
// run signatures alike.
func TestScan_RedactMasksEveryMatchOnTheLine(t *testing.T) {
	const k1, k2 = "AKIAAAAAAAAAAAAAAAAA", "AKIABBBBBBBBBBBBBBBB"
	sigs := loadSigs(t, sigFile(
		regexSig("rk", `AKIA[A-Z0-9]{16}`, "redact: true"),
		kindSig("bk", "block", `start: 'AKIA[A-Z0-9]{16}'`, `continue: '^ '`)+"    redact: true\n",
		kindSig("mk", "run", `match: 'AKIA[A-Z0-9]{16}'`, "min: 2")+"    redact: true\n",
	), "rk", "bk", "mk")
	res := scanOne(t, lines("a "+k1+" b "+k2, "a "+k1+" b "+k2, "  x"), sigs, Options{})
	for _, id := range []string{"rk", "bk", "mk"} {
		f := res.BySignature[id].Findings[0]
		if want := "a AKIA************ b AKIA************"; f.Excerpt != want || !f.Masked {
			t.Errorf("%s: excerpt %q masked=%v, want %q", id, f.Excerpt, f.Masked, want)
		}
	}
	requireNoSecret(t, res, k1, k2, k1[4:], k2[4:])
}

// A redacting regex signature whose capture group takes no part in the
// match masks the whole match.
func TestScan_RedactWithoutACaptureSpanMasksTheWholeMatch(t *testing.T) {
	const secret = "s3cr3tT0kenValue123xyz"
	sigs := loadSigs(t, sigFile(regexSig("tok", `(?:key=(\S+)|Bearer \S+)`, "capture: 1", "redact: true")), "tok")
	res := scanOne(t, lines("auth Bearer "+secret, "auth key="+secret), sigs, Options{})
	got := res.BySignature["tok"].Findings
	if len(got) != 2 || got[0].Excerpt != "auth Bear************" || got[1].Excerpt != "auth key=s3cr************" {
		t.Errorf("excerpts %q, want the whole match masked when the group took no part", describe(got))
	}
	requireNoSecret(t, res, secret, secret[4:])
}

// Masking goes by the match's position: an earlier copy of the value
// that overlaps the match does not shift the mask off the value (the
// copy is masked with it).
func TestScan_RedactMasksByPosition(t *testing.T) {
	const line = "SECRETXYSECRETXYSECRETXY"
	sigs := loadSigs(t, sigFile(regexSig("ov", `^\w{8}(\w{16})`, "capture: 1", "redact: true")), "ov")
	res := scanOne(t, lines(line), sigs, Options{})
	f := res.BySignature["ov"].Findings[0]
	if f.Excerpt != "SECR************" || deref(f.ValueStart) != 8 || deref(f.ValueEnd) != 24 {
		t.Errorf("excerpt %q value %d-%d, want the value at 8-24 and its overlapping copy masked", f.Excerpt, deref(f.ValueStart), deref(f.ValueEnd))
	}
	requireNoSecret(t, res, line[8:], line[12:])
}

// An anchor that can match the empty string still masks its later,
// non-empty match on the line, for blocks and runs.
func TestScan_RedactAnEmptyFirstMatch(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	sigs := loadSigs(t, sigFile(
		kindSig("eb", "block", `start: '(?:AKIA[A-Z0-9]{16})?'`, "lines: 1")+"    redact: true\n",
		kindSig("er", "run", `match: '(?:AKIA[A-Z0-9]{16})?'`, "min: 2")+"    redact: true\n",
	), "eb", "er")
	res := scanOne(t, lines("key "+secret, "next "+secret), sigs, Options{})
	for _, id := range []string{"eb", "er"} {
		if f := res.BySignature[id].Findings[0]; f.Excerpt != "key AKIA************" {
			t.Errorf("%s: excerpt %q", id, f.Excerpt)
		}
	}
	requireNoSecret(t, res, secret[4:])
}

// excerptFixtures put the excerpt and redaction cases through the
// same-answer test: the excerpts, masks and value spans must not depend
// on the chunking or the storage.
var excerptFixtures = []fixture{
	one("excerpt cut on a character boundary", regexSig("err", `ERROR`),
		lines("ERROR "+strings.Repeat("x", 193)+"é and more", "ERROR short"), lineSpan{1, 1}, lineSpan{2, 2}),
	{name: "redaction of every kind",
		def: regexSig("key-value", `key=(AKIA[A-Z0-9]{16})`, "capture: 1", "redact: true") +
			kindSig("key-block", "block", `start: 'AKIA[A-Z0-9]{16}'`, `continue: '^ '`) + "    redact: true\n" +
			kindSig("key-run", "run", `match: 'AKIA[A-Z0-9]{16}'`, "min: 2") + "    redact: true\n",
		ids:  []string{"key-value", "key-block", "key-run"},
		text: lines("a key=AKIAAAAAAAAAAAAAAAAA b AKIABBBBBBBBBBBBBBBB", "key=AKIAIOSFODNN7EXAMPLE", "  more"),
		want: map[string][]lineSpan{"key-value": {{1, 1}, {2, 2}}, "key-block": {{1, 1}, {2, 3}}, "key-run": {{1, 2}}}},
	one("redaction without a capture span", regexSig("tok", `(?:key=(\S+)|Bearer \S+)`, "capture: 1", "redact: true"),
		lines("auth Bearer s3cr3tT0kenValue123xyz", "auth key=s3cr3tT0kenValue123xyz"), lineSpan{1, 1}, lineSpan{2, 2}),
	one("redaction of an overlapping copy", regexSig("ov", `^\w{8}(\w{16})`, "capture: 1", "redact: true"),
		lines("SECRETXYSECRETXYSECRETXY"), lineSpan{1, 1}).withValue(8, 24),
}
