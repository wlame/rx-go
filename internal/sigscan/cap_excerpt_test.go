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
