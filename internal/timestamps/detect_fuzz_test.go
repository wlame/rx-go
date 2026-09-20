package timestamps

import "testing"

// FuzzDetect: no sample panics Detect, the same sample always gives the
// same answer, and a format it returns is one a Parser accepts.
func FuzzDetect(f *testing.F) {
	for _, tc := range detectCases() {
		f.Add(tc.sample)
	}
	f.Fuzz(func(t *testing.T, s []byte) {
		got, ok := Detect(s)
		again, againOK := Detect(s)
		if ok != againOK || !sameFormat(got, again) {
			t.Fatalf("Detect is not deterministic: %s,%t then %s,%t",
				formatString(got), ok, formatString(again), againOK)
		}
		if !ok {
			return
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("Detect returned a format NewParser refuses: %s: %v", formatString(got), err)
		}
	})
}
