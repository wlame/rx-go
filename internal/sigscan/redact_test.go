package sigscan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wlame/rx-go/internal/signatures"
)

// redactCatalogue redacts secrets of one shape in every way a
// signature can: a regex with a capture group behind a prefix, a regex
// that is the secret itself, a block and a match run anchored on it,
// and a hex regex for secrets behind padding.
var redactCatalogue = sigFile(
	regexSig("cap", `key=(SK[0-9A-Za-zé]+)`, "capture: 1", "redact: true"),
	regexSig("bare", `SK[0-9A-Za-zé]+`, "redact: true"),
	kindSig("blk", "block", `start: 'SK[0-9A-Za-zé]+'`, "lines: 1")+"    redact: true\n",
	kindSig("rn", "run", `match: 'SK[0-9A-Za-zé]+'`, "min: 2")+"    redact: true\n",
	regexSig("hex", `[0-9a-f]{32}`, "redact: true"),
)

// redactIDs are the ids of redactCatalogue.
var redactIDs = []string{"cap", "bare", "blk", "rn", "hex"}

// secretMaker plants secrets in generated lines and remembers them, by
// the signatures that redact them: "SK…" secrets for cap, bare, blk and
// rn, hex secrets for hex. A signature masks its own secrets only; the
// excerpt of another signature shows the line as that signature sees it.
type secretMaker struct {
	r       *rand.Rand
	planted []string
	hexes   []string
}

const secretChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789é"

// secret returns a new secret "SK…": mostly short, sometimes over 256
// bytes, sometimes a unit repeated three times, whose copies can overlap
// (overlap returns them).
func (m *secretMaker) secret() string {
	n := 8 + m.r.IntN(30)
	switch m.r.IntN(10) {
	case 0:
		n = 250 + m.r.IntN(60)
	case 1:
		unit := "SK" + m.chars(3+m.r.IntN(4))
		return m.keep(strings.Repeat(unit, 3))
	}
	return m.keep("SK" + m.chars(n))
}

// chars returns n random secret characters.
func (m *secretMaker) chars(n int) string {
	runes := []rune(secretChars)
	var b strings.Builder
	for range n {
		b.WriteRune(runes[m.r.IntN(len(runes))])
	}
	return b.String()
}

// hexSecret returns a 32-digit hex secret without a 0, so a padding run
// of zeros never continues it.
func (m *secretMaker) hexSecret() string {
	const digits = "123456789abcdef"
	var b strings.Builder
	for range 32 {
		b.WriteByte(digits[m.r.IntN(len(digits))])
	}
	m.hexes = append(m.hexes, b.String())
	return b.String()
}

// overlap returns two copies of s that overlap: s+u for a secret that
// is a unit u repeated three times, s followed by s otherwise.
func overlap(s string) string {
	if u := s[:len(s)/3]; strings.Repeat(u, 3) == s {
		return s + u
	}
	return s + s
}

// keep remembers a planted secret.
func (m *secretMaker) keep(s string) string {
	m.planted = append(m.planted, s)
	return s
}

// line builds one line: filler words, secrets behind `key=`, bare
// echoes of them before or after, overlapping copies, zero padding
// before a hex secret, and sometimes more than 8 distinct secrets or a
// secret right at the excerpt's cut.
func (m *secretMaker) line() string {
	var secrets []string
	for range 1 + m.r.IntN(12) {
		secrets = append(secrets, m.secret())
	}
	var parts []string
	if m.r.IntN(4) == 0 {
		// Push the first secret to the excerpt's cut.
		parts = append(parts, strings.Repeat("é", 85+m.r.IntN(15)))
	}
	for _, s := range secrets {
		switch m.r.IntN(5) {
		case 0:
			parts = append(parts, "echo "+s)
		case 1:
			parts = append(parts, overlap(s))
		}
		parts = append(parts, "user=ü"+m.chars(2), "key="+s)
		if m.r.IntN(3) == 0 {
			parts = append(parts, "again "+s)
		}
	}
	if m.r.IntN(3) == 0 {
		parts = append(parts, `pad=`+strings.Repeat("0", 50+m.r.IntN(3000))+` hex=`+m.hexSecret())
	}
	m.r.Shuffle(len(parts)-1, func(i, j int) { parts[i+1], parts[j+1] = parts[j+1], parts[i+1] })
	return strings.Join(parts, " ")
}

// hiddenWindows returns the 8-byte pieces of a secret that hold at
// least one byte its mask must hide (past the first characters the mask
// keeps: a quarter of them, at most maskKeepChars).
func hiddenWindows(s string) []string {
	kept := keptBytes([]byte(s))
	var out []string
	for i := 0; i+8 <= len(s); i++ {
		if i+8 > kept && utf8.RuneStart(s[i]) {
			out = append(out, s[i:i+8])
		}
	}
	return out
}

// requireHidden fails when a window of a secret that its mask must hide
// is in the results of the signatures ids, as JSON or as the excerpts
// themselves.
func requireHidden(t *testing.T, res *Result, ids, secrets []string) {
	t.Helper()
	var text strings.Builder
	for _, id := range ids {
		encoded, err := json.Marshal(res.BySignature[id])
		if err != nil {
			t.Fatal(err)
		}
		text.Write(encoded)
		for _, f := range res.BySignature[id].Findings {
			text.WriteString("\n" + f.Excerpt) // the excerpt as it is, unescaped
		}
	}
	for _, s := range secrets {
		for _, w := range hiddenWindows(s) {
			if strings.Contains(text.String(), w) {
				t.Fatalf("%q of the secret %q is in the results of %v", w, s, ids)
			}
		}
	}
}

// plainCatalogue are signatures that do not redact, on the same lines:
// their excerpts are masked by the redacting ones all the same.
var plainCatalogue = sigFile(
	regexSig("plain-user", `user=`),
	regexSig("plain-any", `\S+`),
	kindSig("plain-blk", "block", `start: 'key='`, "lines: 1"),
)

// plainIDs are the ids of plainCatalogue.
var plainIDs = []string{"plain-user", "plain-any", "plain-blk"}

// No byte of a planted secret beyond the part its mask keeps appears in
// any result: lines with several secrets, bare and overlapping copies
// before and after them, zero padding before a hex secret, a secret at
// the excerpt's cut, more than 8 distinct secrets and secrets over 256
// bytes, in UTF-8 text (fixed seeds). Every signature's excerpt is
// masked by every redacting one, selected or only given as a redactor,
// so no result of any signature shows any secret.
func TestScan_RedactionNeverLeaksAPlantedSecret(t *testing.T) {
	redacting := loadSigs(t, redactCatalogue, redactIDs...)
	plain := loadSigs(t, plainCatalogue, plainIDs...)
	allIDs := append(append([]string{}, redactIDs...), plainIDs...)
	for seed := uint64(1); seed <= 4; seed++ {
		m := &secretMaker{r: rand.New(rand.NewPCG(seed, seed*31))}
		var ls []string
		for range 150 {
			ls = append(ls, m.line())
		}
		secrets := append(append([]string{}, m.planted...), m.hexes...)
		res := scanOne(t, lines(ls...), append(append([]*signatures.Signature{}, redacting...), plain...), Options{Redactors: redacting})
		requireHidden(t, res, allIDs, secrets)
		if res.BySignature["cap"].Count < 150 {
			t.Fatalf("seed %d: %d findings of cap, the lines lost their secrets", seed, res.BySignature["cap"].Count)
		}
		// The redacting signatures given only as redactors, not selected.
		res = scanOne(t, lines(ls...), plain, Options{Redactors: redacting})
		requireHidden(t, res, plainIDs, secrets)
	}
}

// Masking a crafted 64 KiB line takes work linear in the line, counted
// in automaton steps (never in time): a value whose copies overlap all
// along the line, the most values the bounds allow over a periodic
// line, a value too long to follow, and more distinct values than can
// be followed.
func TestRedaction_WorkIsLinear(t *testing.T) {
	const n = 64 << 10
	within := 2*maxRedactValues*(n+2*maxRedactValueBytes) + 64
	failClosed := 2*maxRedactSeen*failClosedPrefixBytes + 4*n
	var distinct strings.Builder
	for i := 0; distinct.Len() < n; i++ {
		fmt.Fprintf(&distinct, "tok%08d ", i)
	}
	cases := []struct {
		name, line, anchor string
		bound              int
	}{
		{"overlapping copies", strings.Repeat("a", n), `a{100}`, within},
		{"eight values over a periodic line", eightValues(n), `(?:ab){121,128}`, within},
		{"a value too long to follow", strings.Repeat("A", n), `A{1000}`, failClosed},
		{"more distinct values than can be followed", distinct.String(), `tok\d{8}`, failClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := []byte(tc.line)
			r := redaction{view: view, values: redactMatches(regexp.MustCompile(tc.anchor), 0, view)}
			out := r.appendTo(nil, n)
			if r.steps > tc.bound {
				t.Errorf("%d steps for a %d-byte line, bound %d", r.steps, n, tc.bound)
			}
			if len(out) == 0 || len(out) > n || bytes.Contains(out, view[:min(len(view), 40)]) {
				t.Errorf("masked line of %d bytes starts %q", len(out), out[:min(len(out), 60)])
			}
		})
	}
}

// eightValues is a line of n bytes that starts with eight runs of "ab"
// of 128 to 121 pairs, apart, then goes on with "ab": eight distinct
// values of up to 256 bytes, each with an overlapping copy at every
// other byte of the rest.
func eightValues(n int) string {
	var b strings.Builder
	for k := 128; k > 120; k-- {
		b.WriteString(strings.Repeat("ab", k) + "|")
	}
	for b.Len() < n {
		b.WriteString("ab")
	}
	return b.String()[:n]
}

// A run that joins copies of several values keeps no more of its start
// than the value with the smallest kept part allows: here a 10-character
// value (2 kept) is a prefix of a 23-character one (4 kept).
func TestRedaction_JoinedRunKeepsTheLeast(t *testing.T) {
	view := []byte("x=SKabcdefgh y=SKabcdefghijklmnopqrstu")
	excerpt, _, _ := appendRedactedExcerpt(nil, nil, view, redactMatches(regexp.MustCompile(`=(SK\w+)`), 1, view))
	got := string(excerpt)
	if want := "x=SK******** y=SK************"; got != want {
		t.Errorf("excerpt %q, want %q", got, want)
	}
}

// A line beyond the bounds shows only its text before the first secret
// (or a copy of one before it), then stars; one with too many distinct
// values shows none of it.
func TestRedaction_FailsClosed(t *testing.T) {
	re := regexp.MustCompile(`key=(\w+)`)
	var many strings.Builder
	many.WriteString("echo v9xxxxxxxx ")
	for i := range 10 {
		fmt.Fprintf(&many, "key=v%dxxxxxxxx ", i)
	}
	long := "head key=" + strings.Repeat("L", 300) + " tail"
	cases := map[string]string{
		many.String(): "echo ************",
		long:          "head key=************",
	}
	var tooMany strings.Builder
	for i := range maxRedactSeen + 1 {
		fmt.Fprintf(&tooMany, "key=t%03dxxxxx ", i)
	}
	cases[tooMany.String()] = "************"
	for line, want := range cases {
		view := []byte(line)
		excerpt, _, _ := appendRedactedExcerpt(nil, nil, view, redactMatches(re, 1, view))
		if got := string(excerpt); got != want {
			t.Errorf("line %.40q…: excerpt %q, want %q", line, got, want)
		}
	}
}

// An excerpt is masked by every redacting signature given, whether it
// is selected or not: an error line, a block and a run of repeated
// lines holding an AWS key keep it masked, and their findings say so;
// a finding with nothing masked says it is not.
func TestScan_EveryRedactorMasksEveryExcerpt(t *testing.T) {
	const key = "AKIAIOSFODNN7EXAMPLE"
	sigs := loadSigs(t, sigFile(
		regexSig("err", `ERROR`),
		kindSig("blk", "block", `start: '^BEGIN'`, `end: '^END'`),
		kindSig("rep", "run", "same: true", "min: 2"),
	), "err", "blk", "rep", "aws-access-key")
	plain, aws := sigs[:3], sigs[3]
	text := lines("ERROR upload failed for key "+key, "BEGIN "+key, "END", "dup "+key, "dup "+key, "ERROR plain")
	for name, selected := range map[string][]*signatures.Signature{
		"redactor not selected": plain,
		"redactor selected":     sigs,
	} {
		t.Run(name, func(t *testing.T) {
			res := scanOne(t, text, selected, Options{Redactors: []*signatures.Signature{aws}})
			for _, id := range []string{"err", "blk", "rep"} {
				f := res.BySignature[id].Findings[0]
				if !strings.Contains(f.Excerpt, "AKIA************") || !f.Masked {
					t.Errorf("%s: excerpt %q masked=%v, want the key masked", id, f.Excerpt, f.Masked)
				}
			}
			if f := res.BySignature["err"].Findings[1]; f.Excerpt != "ERROR plain" || f.Masked {
				t.Errorf("a line without a secret: excerpt %q masked=%v", f.Excerpt, f.Masked)
			}
			requireNoSecret(t, res, key[4:])
		})
	}
}

// A redactor whose (?i) anchor matches only through the Kelvin sign
// (for k) or the long s (for s) still masks: such a line skips the
// redactors' keyword gate as it skips the scan's. The secret stays
// masked in another signature's excerpt whether the redactor is
// selected or only given, and a selected redactor finds the line.
func TestScan_RedactorMatchingThroughAFoldLetterMasks(t *testing.T) {
	const keySecret, passSecret = "SECRET9876543210", "hunter2hunter2xyz"
	sigs := loadSigs(t, sigFile(
		regexSig("err", `ERROR`),
		regexSig("key", `(?i)key=(\w+)`, "capture: 1", "redact: true"),
		regexSig("pass", `(?i)password=(\S+)`, "capture: 1", "redact: true"),
	), "err", "key", "pass")
	plain, redactors := sigs[:1], sigs[1:]
	text := lines("Key="+keySecret+" ERROR", "paſſword="+passSecret+" ERROR")
	cases := []struct {
		name     string
		selected []*signatures.Signature
	}{
		{"redactors given, not selected", plain},
		{"redactors selected", sigs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := scanOne(t, text, tc.selected, Options{Redactors: redactors})
			errs := res.BySignature["err"].Findings
			if len(errs) != 2 {
				t.Fatalf("err findings %v, want lines 1 and 2", spans(res, "err"))
			}
			for i, mask := range []string{"SECR************", "hunt************"} {
				if f := errs[i]; !strings.Contains(f.Excerpt, mask) || !f.Masked {
					t.Errorf("line %d: excerpt %q masked=%v, want %q", i+1, f.Excerpt, f.Masked, mask)
				}
			}
			requireNoSecret(t, res, keySecret[4:], passSecret[4:])
			if len(tc.selected) == len(sigs) {
				if got := spans(res, "key"); !slices.Equal(got, []lineSpan{{1, 1}}) {
					t.Errorf("key findings %v, want line 1", got)
				}
				if got := spans(res, "pass"); !slices.Equal(got, []lineSpan{{2, 2}}) {
					t.Errorf("pass findings %v, want line 2", got)
				}
			}
		})
	}
}
