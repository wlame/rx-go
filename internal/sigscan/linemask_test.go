package sigscan

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wlame/rx-go/internal/signatures"
)

// lineMaskSigs are redacting signatures of every kind a line masker
// follows: a regex with a capture behind a prefix, a regex that is the
// secret itself, a block and a run anchored on it, and an entropy
// measure (a value measure masks its values).
func lineMaskSigs(t testing.TB) []*signatures.Signature {
	t.Helper()
	body := redactCatalogue + "  - id: ent\n    name: ent\n    group: secret\n    description: Test.\n" +
		"    redact: true\n    measure: {name: entropy, min_length: 40}\n"
	return loadSigs(t, body, append(slices2(redactIDs), "ent")...)
}

// slices2 copies ids, so appending to it leaves the package variable
// alone.
func slices2(ids []string) []string { return append([]string(nil), ids...) }

// newMasker builds a line masker or fails the test.
func newMasker(t testing.TB, sigs []*signatures.Signature, viewBytes int) *LineMasker {
	t.Helper()
	m, err := NewLineMasker(sigs, viewBytes)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A line masked whole and cut to the excerpt's 200 bytes is the excerpt
// a scan gives the line, mask for mask, and is marked masked exactly
// when the excerpt is: the lines of a finding and its excerpt never
// disagree about a secret.
func TestLineMasker_MasksLikeTheExcerpt(t *testing.T) {
	sigs := lineMaskSigs(t)
	every := loadSigs(t, sigFile(regexSig("any", `.`)), "any")
	for seed := uint64(1); seed <= 3; seed++ {
		r := rand.New(rand.NewPCG(seed, 7))
		m := &secretMaker{r: r}
		var text strings.Builder
		var lines []string
		for range 300 {
			line := plantedLine(m)
			if r.IntN(2) == 0 {
				line = m.line()
			}
			lines = append(lines, line)
			text.WriteString(line + "\n")
		}
		res := scanOne(t, text.String(), every, Options{Redactors: sigs})
		masker := newMasker(t, sigs, 0)
		for _, f := range res.BySignature["any"].Findings {
			line := lines[f.StartLine-1]
			got, masked := masker.MaskLine([]byte(line), excerptBytes)
			if got != f.Excerpt || masked != f.Masked {
				t.Fatalf("seed %d line %d %q:\nmasked line %q (%v)\nexcerpt     %q (%v)",
					seed, f.StartLine, line, got, masked, f.Excerpt, f.Masked)
			}
		}
	}
}

// plantedLine is a generated line with secrets in it, some past the
// 200 bytes of an excerpt.
func plantedLine(m *secretMaker) string {
	var b strings.Builder
	for b.Len() < 120+m.r.IntN(400) {
		switch m.r.IntN(6) {
		case 0:
			b.WriteString(" key=" + m.secret())
		case 1:
			b.WriteString(" " + m.secret())
		case 2:
			b.WriteString(" " + m.hexSecret())
		default:
			b.WriteString(" word" + strings.Repeat("x", m.r.IntN(12)))
		}
	}
	return b.String()
}

// A secret anywhere on the line is masked, not only in its first 200
// bytes, and its bytes appear nowhere in the masked line.
func TestLineMasker_MasksSecretsPastTheExcerpt(t *testing.T) {
	sigs := lineMaskSigs(t)
	masker := newMasker(t, sigs, 0)
	const secret = "SKabcdefghijklmnop0123"
	line := strings.Repeat("filler ", 300) + "key=" + secret + " tail"
	got, masked := masker.MaskLine([]byte(line), 1<<20)
	if !masked || strings.Contains(got, secret[4:]) {
		t.Fatalf("masked %v, line ends %q", masked, got[len(got)-60:])
	}
	if !strings.HasSuffix(got, "key=SKab************ tail") {
		t.Errorf("line ends %q", got[len(got)-40:])
	}
}

// The masked line is valid UTF-8 and cut at the limit on a character
// boundary, as an excerpt is at 200 bytes.
func TestLineMasker_CutsValidUTF8AtTheLimit(t *testing.T) {
	masker := newMasker(t, nil, 0)
	cases := []struct {
		name, line string
		limit      int
		want       string
	}{
		{"short line", "plain text", 100, "plain text"},
		{"cut", "abcdefghij", 4, "abcd"},
		{"cut before a character", "abcé", 4, "abc"},
		{"invalid byte", "a\xffb", 100, "a\uFFFDb"},
		{"replacement too long for the room", "ab\xff", 4, "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, masked := masker.MaskLine([]byte(tc.line), tc.limit)
			if got != tc.want || masked {
				t.Errorf("MaskLine(%q, %d) = %q, %v; want %q", tc.line, tc.limit, got, masked, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("not valid UTF-8: %q", got)
			}
		})
	}
}

// On a line longer than the line view, a value the scan's view matches
// is masked although the whole line no longer matches there (an anchor
// that ends at the view's end), and a value past the view, which no
// scan saw, is masked too.
func TestLineMasker_LongLineMasksWhatTheViewAndTheLineMatch(t *testing.T) {
	sigs := loadSigs(t, sigFile(
		regexSig("tail", `token=(\w+)$`, "capture: 1", "redact: true"),
		regexSig("key", `key=(SK\w+)`, "capture: 1", "redact: true"),
	), "tail", "key")
	const view = 1024
	head := strings.Repeat("x", view-20) + " token="
	line := head + strings.Repeat("Q", 400) + " more key=SKsecretvalue99 end"
	masker := newMasker(t, sigs, view)
	got, masked := masker.MaskLine([]byte(line), 1<<20)
	if !masked {
		t.Fatal("not masked")
	}
	if strings.Contains(got, "QQQQQQQQ") {
		t.Errorf("the value the view matched shows: %q", got[view-40:view+40])
	}
	if strings.Contains(got, "secretvalue99") {
		t.Errorf("the value past the view shows: %q", got[len(got)-40:])
	}
}

// A line with more distinct values than the masking follows is masked
// from its earliest value on, with nothing of the rest readable.
func TestLineMasker_FailsClosed(t *testing.T) {
	sigs := loadSigs(t, sigFile(regexSig("key", `key=(SK\w+)`, "capture: 1", "redact: true")), "key")
	var b strings.Builder
	b.WriteString("start ")
	for i := range maxRedactValues + 2 {
		b.WriteString("key=SK" + strings.Repeat(string(rune('a'+i)), 10) + " ")
	}
	got, masked := newMasker(t, sigs, 0).MaskLine([]byte(b.String()), 1<<20)
	if !masked || strings.Contains(got, "SK") || !strings.HasPrefix(got, "start key=") {
		t.Errorf("masked %v: %q", masked, got)
	}
}

// A case-insensitive anchor that matches through the Kelvin sign masks
// its value, as the scan's excerpt does.
func TestLineMasker_FoldLetterMasks(t *testing.T) {
	sigs := loadSigs(t, sigFile(regexSig("key", `(?i)key=(\w+)`, "capture: 1", "redact: true")), "key")
	got, masked := newMasker(t, sigs, 0).MaskLine([]byte("\u212aey=SECRET9876543210 end"), 1000)
	if !masked || strings.Contains(got, "9876543210") {
		t.Errorf("masked %v: %q", masked, got)
	}
}

// Without a redacting signature a line is only made valid UTF-8 and cut,
// and a signature that does not redact masks nothing.
func TestLineMasker_NonRedactingSignaturesMaskNothing(t *testing.T) {
	sigs := loadSigs(t, sigFile(regexSig("key", `key=(SK\w+)`, "capture: 1")), "key")
	got, masked := newMasker(t, sigs, 0).MaskLine([]byte("key=SKvisible"), 100)
	if masked || got != "key=SKvisible" {
		t.Errorf("MaskLine = %q, %v", got, masked)
	}
}

// Masking a line costs work linear in the line: on crafted lines of
// 256 KiB, longer than the line view so both passes run, the redaction
// takes at most twice its documented bound per pass.
func TestLineMasker_WorkIsLinear(t *testing.T) {
	const n = 256 << 10
	within := 2 * (2*maxRedactValues*(n+2*maxRedactValueBytes) + 64)
	failClosed := 2*maxRedactSeen*failClosedPrefixBytes + 4*n + 64
	cases := []struct {
		name, line, anchor string
		bound              int
	}{
		{"overlapping copies", strings.Repeat("a", n), `a{100}`, within},
		{"eight values over a periodic line", eightValues(n), `(?:ab){121,128}`, within},
		{"a value too long to follow", strings.Repeat("A", n), `A{1000}`, failClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sigs := loadSigs(t, sigFile(regexSig("v", tc.anchor, "redact: true", "keywords: []")), "v")
			m := newMasker(t, sigs, 64<<10)
			got, masked := m.MaskLine([]byte(tc.line), n)
			if m.steps > tc.bound {
				t.Errorf("%d steps for a %d-byte line, bound %d", m.steps, n, tc.bound)
			}
			if !masked || bytes.Contains([]byte(got), []byte(tc.line[:40])) {
				t.Errorf("masked %v, line starts %q", masked, got[:min(len(got), 60)])
			}
		})
	}
}

// On a line longer than the view, a match both passes find counts once
// toward the masking's match bound: 3,000 copies of one secret in the
// view stay within it, so the text around them stays readable rather
// than the whole line being masked.
func TestLineMasker_MatchSeenByBothPassesCountsOnce(t *testing.T) {
	sigs := loadSigs(t, sigFile(regexSig("key", `SK[a-z]+`, "redact: true")), "key")
	line := strings.Repeat("v=SKabcdef ", 3000) + strings.Repeat("y", 40000)
	got, masked := newMasker(t, sigs, 64<<10).MaskLine([]byte(line), 1<<20)
	if !masked || !strings.HasPrefix(got, "v=SK****** v=") || strings.Contains(got, "abcdef") {
		t.Errorf("masked %v, line starts %q", masked, got[:min(len(got), 40)])
	}
}
