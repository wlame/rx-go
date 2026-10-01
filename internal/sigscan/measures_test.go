package sigscan

import (
	"strings"
	"testing"
)

// measureSig is a measure signature definition: params is the flow
// mapping's body after the name ("factor: 5"), extra are more fields.
func measureSig(id, name, params string, extra ...string) string {
	body := "{name: " + name
	if params != "" {
		body += ", " + params
	}
	return fmtSig(id, "measure: "+body+"}", extra)
}

// fmtSig is a signature of group "structure" with one kind line and
// extra fields.
func fmtSig(id, kindLine string, extra []string) string {
	return "  - id: " + id + "\n    name: " + id + "\n    group: structure\n    description: Test.\n    " + kindLine + "\n" + indentFields(extra)
}

// Every measure has a version, and a name that is no measure has 0.
func TestMeasureVersion(t *testing.T) {
	for _, name := range []string{"long-line", "binary", "entropy", "json-block", "unprefixed", "base64-run", "density", "time-gap", "time-backwards"} {
		if MeasureVersion(name) < 1 {
			t.Errorf("%s has version %d", name, MeasureVersion(name))
		}
	}
	if MeasureVersion("sparkle") != 0 {
		t.Errorf("an unknown measure has version %d", MeasureVersion("sparkle"))
	}
}

// textHead is 9 lines of about 1 KB: a file with a NUL byte in its
// first 8 KiB is not text and is not scanned at all, so the binary
// fixture starts after it.
var textHead = strings.Repeat(strings.Repeat("filler ", 142)+"\n", 9)

// binaryFixture: a NUL in the middle of a line, an invalid UTF-8 byte,
// and three such lines in a row, which give one finding. A valid
// multi-byte character is not binary, and neither is one the line view
// cuts in two.
var binaryFixture = one("binary lines", measureSig("bin", "binary", ""),
	textHead+lines(
		"ok",
		"bad\x00line",
		"héllo",
		"bad \xff byte",
		"ok",
		"a\x00",
		"b\xfe",
		"c\x00\xff",
		"ok",
		strings.Repeat("x", 1023)+"é",
	),
	lineSpan{11, 11}, lineSpan{13, 13}, lineSpan{15, 17},
).withOpts(Options{LineBytes: 1024})

// Lines with a NUL or bytes that are not UTF-8 are found as runs.
func TestScan_BinaryMeasure(t *testing.T) {
	binaryFixture.run(t)
}

// entropyToken is a 40-character token of 40 different characters: 5.3
// bits per byte.
const entropyToken = "Zx8Qp2Lm9Rt4Wv7YbN3kD5fH1jS6gA0cEuTo+IqB"

// entropyFixture: a random base64 token and a random hex token are
// found, the first one per line; a UUID, a 32-digit number and a run of
// English words (4.4 bits per byte) are not.
var entropyFixture = one("entropy tokens", measureSig("ent", "entropy", ""),
	lines(
		"login key "+entropyToken+" accepted",
		"request 550e8400-e29b-41d4-a716-446655440000 done",
		"order 12345678901234567890123456789012 shipped",
		"note the_quick_brown_fox_jumps_over_the_lazy_dog",
		"digest 9f86d081884c7d659a2feaa0c55ad015 second "+entropyToken,
	),
	lineSpan{1, 1}, lineSpan{5, 5},
).withValue(10, 50)

// A random token is found with its value span; ids, numbers and words
// are not.
func TestScan_EntropyMeasure(t *testing.T) {
	entropyFixture.run(t)
	res := scanOne(t, entropyFixture.text, loadSigs(t, sigFile(entropyFixture.def), "ent"), Options{})
	f := res.BySignature["ent"].Findings[1]
	if *f.ValueStart != 7 || *f.ValueEnd != 39 {
		t.Errorf("the hex token is the first on line 5: value %d-%d, want 7-39", *f.ValueStart, *f.ValueEnd)
	}
	// A UUID never reaches 4.5 bits (17 symbols), so its skip shows only
	// with a lower threshold.
	low := loadSigs(t, sigFile(measureSig("low", "entropy", "base64: 2.0")), "low")
	if got := scanOne(t, lines("request 550e8400-e29b-41d4-a716-446655440000 done"), low, Options{}).BySignature["low"]; got.Count != 0 {
		t.Errorf("a UUID is a finding at 2 bits per byte: %d", got.Count)
	}
}

// With redact, the token is masked in the excerpt, and the entropy
// signature masks every excerpt of the line as a redactor, selected or
// not.
func TestScan_EntropyRedactsItsToken(t *testing.T) {
	sigs := loadSigs(t, sigFile(measureSig("ent", "entropy", "", "redact: true"), regexSig("login", `login`)), "ent", "login")
	text := entropyFixture.text
	for _, selected := range [][]int{{0, 1}, {1}} {
		var pick = sigs[:0:0]
		for _, i := range selected {
			pick = append(pick, sigs[i])
		}
		res := scanOne(t, text, pick, Options{Redactors: sigs[:1]})
		requireNoSecret(t, res, entropyToken)
		for id, r := range res.BySignature {
			for _, f := range r.Findings {
				if !strings.Contains(f.Excerpt, "Zx8Q*") || !f.Masked {
					t.Errorf("%s: excerpt %q masked %v, want the token masked", id, f.Excerpt, f.Masked)
				}
			}
		}
	}
}

// base64Fixture: a 120-byte run inside a line, with its == padding, is
// found with its span; a 99-byte run is not.
var base64Fixture = one("base64 runs", measureSig("b64", "base64-run", ""),
	lines(
		"payload="+strings.Repeat("QUJD", 29)+"QU=="+" end",
		"short "+strings.Repeat("Q", 99)+" end",
	),
	lineSpan{1, 1},
).withValue(8, 128)

// A run of base64 bytes is found by a byte scan, its padding included;
// a shorter one is not. A run is inside one line, so it never crosses a
// chunk edge.
func TestScan_Base64RunMeasure(t *testing.T) {
	base64Fixture.run(t)
}

// unprefixedSig finds runs of 5 lines without a timestamp.
var unprefixedSig = measureSig("bare", "unprefixed", "")

// stampedRun returns n ISO-stamped lines from second from on.
func stampedRun(from, n int) []string {
	out := make([]string, n)
	for i := range out {
		s := from + i
		out[i] = "2026-01-02T10:" + twoDigits(s/60) + ":" + twoDigits(s%60) + ".000Z INFO step"
	}
	return out
}

// twoDigits writes n as two digits.
func twoDigits(n int) string {
	return string([]byte{byte('0' + n/10%10), byte('0' + n%10)})
}

// dump is a 6-line dump without timestamps.
var dump = []string{"Heap dump:", "  objects 12", "  bytes 4096", "  roots 3", "  threads 7", "end of dump"}

// unprefixedFixture: a 6-line dump in a stamped log is found.
var unprefixedFixture = fixture{
	name: "unprefixed dump in a stamped log",
	def:  unprefixedSig,
	ids:  []string{"bare"},
	text: lines(append(append(stampedRun(0, 20), dump...), stampedRun(20, 10)...)...),
	want: map[string][]lineSpan{"bare": {{21, 26}}},
}

// A 6-line dump in a stamped log is found; the same dump in a file with
// 30% stamped lines is not, nor in a file without timestamps.
func TestScan_UnprefixedMeasure(t *testing.T) {
	unprefixedFixture.run(t)
	sigs := loadSigs(t, sigFile(unprefixedSig), "bare")
	sparse := lines(append(append(stampedRun(0, 3), dump...), "trailer")...)
	if got := scanOne(t, sparse, sigs, Options{}).BySignature["bare"]; got.Count != 0 || len(got.Findings) != 0 {
		t.Errorf("30%% stamped lines: %d findings, want none", got.Count)
	}
	plain := lines(append([]string{"a", "b"}, dump...)...)
	if got := scanOne(t, plain, sigs, Options{}).BySignature["bare"]; got.Count != 0 {
		t.Errorf("no timestamps: %d findings, want none", got.Count)
	}
}
