package sigscan

import (
	"math"
	"sync"
)

// The alphabet sizes the entropy measure compares a token with.
const (
	base64Symbols = 64
	hexSymbols    = 16
)

// randomEntropyTableLen is the longest token length whose average
// entropy is worked out exactly; past it the closed form is used.
const randomEntropyTableLen = 1024

// randomEntropyTables holds, for each alphabet size, the average
// Shannon entropy (bits per byte) of a token of n characters drawn
// uniformly from it, for n from 0 to randomEntropyTableLen.
//
// Go note: sync.OnceValue runs the function once, the first time the
// returned function is called, from whichever goroutine calls it first,
// and hands every caller the same result after that; the tables are
// read-only from then on, so the scan's workers share them freely.
var randomEntropyTables = sync.OnceValue(func() map[int][]float64 {
	return map[int][]float64{
		base64Symbols: averageEntropyTable(base64Symbols, randomEntropyTableLen),
		hexSymbols:    averageEntropyTable(hexSymbols, randomEntropyTableLen),
	}
})

// randomEntropy returns the average Shannon entropy, in bits per byte,
// of a token of n characters drawn uniformly from an alphabet of k
// symbols (k is base64Symbols or hexSymbols). It is below log2(min(n,
// k)): a random token repeats some characters.
func randomEntropy(k, n int) float64 {
	if n <= randomEntropyTableLen {
		return randomEntropyTables()[k][n]
	}
	// The Miller-Madow form: the bias of the entropy of n draws is
	// about (k-1)/(2n) nats below log2(k); off by under 0.001 bits past
	// 1,024 characters.
	return math.Log2(float64(k)) - float64(k-1)/(2*float64(n)*math.Ln2)
}

// averageEntropyTable works out the average entropy of tokens of 0 to
// maxLen characters over k symbols. Each symbol's count c in a token of
// n characters is binomial (n, 1/k), and the entropy of the token is
// log2(n) minus the sum over the symbols of (c/n)·log2(c), so its
// average is log2(n) − (k/n)·E[c·log2(c)]. The binomial probabilities
// come from the recurrence P(c) = P(c−1)·(n−c+1)/c·p/(1−p), with no
// factorial, in O(maxLen²) steps once.
func averageEntropyTable(k, maxLen int) []float64 {
	table := make([]float64, maxLen+1)
	p := 1 / float64(k)
	odds := p / (1 - p)
	for n := 1; n <= maxLen; n++ {
		prob := math.Pow(1-p, float64(n)) // P(c = 0)
		sum := 0.0
		for c := 1; c <= n; c++ {
			prob *= float64(n-c+1) / float64(c) * odds
			sum += prob * float64(c) * math.Log2(float64(c))
		}
		table[n] = math.Log2(float64(n)) - float64(k)/float64(n)*sum
	}
	return table
}
