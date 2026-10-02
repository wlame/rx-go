package sigscan

// The entropy measure as the built-in high-entropy-token runs it: what
// share of random keys it finds, and what it leaves alone.

import (
	"math/rand/v2"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
)

// base64Alphabet is the alphabet of the random keys the recall test
// draws from.
const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// builtinEntropyMatcher is the value matcher of the built-in
// high-entropy-token, with its parameters as the catalogue loads them.
func builtinEntropyMatcher(t *testing.T) valueMatcher {
	t.Helper()
	sig, ok := builtinCatalogue(t).Lookup("high-entropy-token")
	if !ok || sig.Measure == nil {
		t.Fatal("no built-in high-entropy-token measure")
	}
	return newValueMatcher(sig.Measure.Params)
}

// foundWhole reports whether m finds token, alone on a line, as one
// value covering all of it.
func foundWhole(m valueMatcher, token string) bool {
	v, ok := m.next([]byte(token), 0)
	return ok && v[0] == 0 && v[1] == len(token)
}

// At least nine in ten random base64 keys of 24 to 64 characters are
// found (a seeded generator, 1,000 keys per length).
func TestEntropyMeasure_FindsRandomBase64Keys(t *testing.T) {
	m := builtinEntropyMatcher(t)
	rng := rand.New(rand.NewPCG(209, 31))
	for _, n := range []int{24, 28, 32, 36, 40, 44, 48, 56, 64} {
		found := 0
		const draws = 1000
		for i := 0; i < draws; i++ {
			b := make([]byte, n)
			for j := range b {
				b[j] = base64Alphabet[rng.IntN(len(base64Alphabet))]
			}
			if foundWhole(m, string(b)) {
				found++
			}
		}
		share := float64(found) / draws
		t.Logf("%d characters: %.1f%% found", n, 100*share)
		if share < 0.90 {
			t.Errorf("%d characters: %.1f%% of random keys found, want at least 90%%", n, 100*share)
		}
	}
}

// UUIDs, hex hashes and ids, numbers, words, identifiers and paths are
// not found with the built-in's parameters.
func TestEntropyMeasure_LeavesIdsHashesAndWordsAlone(t *testing.T) {
	m := builtinEntropyMatcher(t)
	for _, token := range []string{
		"550e8400-e29b-41d4-a716-446655440000",                                                // UUID
		"550E8400-E29B-41D4-A716-446655440000",                                                // UUID, upper case
		"550E8400-e29b-41D4-a716-446655440000",                                                // UUID, mixed case
		"9f86d081884c7d659a2feaa0c55ad015",                                                    // MD5
		"2fd4e1c67a2d28fced849ee1bb76e7391b93eb12",                                            // SHA-1
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",                    // SHA-256
		"4F9A0C2E8B7D6A5F4E3D2C1B0A9F8E7D",                                                    // request id, hex
		"12345678901234567890123456789012",                                                    // number
		"the_quick_brown_fox_jumps_over_the_lazy_dog",                                         // words
		"ConfigurationPropertiesBeanFactory",                                                  // identifier
		"/srv/app/data/Cache_2026_03_01/Worker-17/segment_00042/Index_File/Batch-9/queue_xyz", // path
	} {
		if v, ok := m.next([]byte(token), 0); ok {
			t.Errorf("%s: found %q", token, token[v[0]:v[1]])
		}
	}
}

// hex_ratio turns hex tokens on: a hash is then a finding.
func TestEntropyMeasure_HexRatioFindsHexTokens(t *testing.T) {
	sha := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	on := loadSigs(t, sigFile(measureSig("hexes", "entropy", "hex_ratio: 0.9")), "hexes")
	if !foundWhole(newValueMatcher(on[0].Measure.Params), sha) {
		t.Errorf("hex_ratio 0.9 does not find a SHA-256")
	}
	if got := on[0].Measure.Params.(signatures.EntropyParams).Ratio; got != 0.94 {
		t.Errorf("ratio %v, want the default 0.94", got)
	}
}

// A token without an upper-case letter, a lower-case letter and a digit
// is never found by the base64 rule, however random its characters.
func TestEntropyMeasure_NeedsTheThreeCharacterClasses(t *testing.T) {
	low := loadSigs(t, sigFile(measureSig("low", "entropy", "ratio: 0.1")), "low")
	m := newValueMatcher(low[0].Measure.Params)
	for _, token := range []string{"qwertzuiopasdfghjklyxcvbnm/+mnbvcxyz", "QWERTZUIOPASDFGHJKLYXC1234567890ZXCV", "qwertzuiop1234567890asdfghjklyxcvb"} {
		if _, ok := m.next([]byte(token), 0); ok {
			t.Errorf("%s: found without all three classes", token)
		}
	}
	if !foundWhole(m, "qwertzuiOP1234567890asdfghjklyxcvb") {
		t.Error("a token with the three classes is not found at ratio 0.1")
	}
}

// randomEntropy is the mean entropy of random tokens: the table's value
// agrees with the mean over 20,000 seeded draws to 0.01 bits, for both
// alphabets, inside the table and past it.
func TestRandomEntropy_IsTheMeanOfRandomTokens(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 209))
	for _, tc := range []struct {
		alphabet string
		k, n     int
	}{
		{base64Alphabet, base64Symbols, 24}, {base64Alphabet, base64Symbols, 64}, {base64Alphabet, base64Symbols, 1500},
		{"0123456789abcdef", hexSymbols, 32}, {"0123456789abcdef", hexSymbols, 2000},
	} {
		const draws = 20000
		draw := draws
		if tc.n > 1000 {
			draw = 2000
		}
		sum := 0.0
		b := make([]byte, tc.n)
		for i := 0; i < draw; i++ {
			for j := range b {
				b[j] = tc.alphabet[rng.IntN(len(tc.alphabet))]
			}
			sum += shannonEntropy(b)
		}
		mean, got := sum/float64(draw), randomEntropy(tc.k, tc.n)
		if d := mean - got; d > 0.01 || d < -0.01 {
			t.Errorf("k=%d n=%d: table %.4f, mean of draws %.4f", tc.k, tc.n, got, mean)
		}
	}
}
