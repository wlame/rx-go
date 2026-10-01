package sigscan

import (
	"context"
	"fmt"

	"github.com/wlame/rx-go/internal/signatures"
)

// CheckExamples runs a signature over its own examples with this
// engine: every `match` text must give at least one finding, and every
// `no_match` text none. Each text is scanned as a file of its own (a
// text of several lines is read as lines), with the default options.
//
// It is the function signatures.LoadOptions.CheckExamples takes: the
// commands that load signatures pass it, and a signature whose examples
// fail is refused at load with the file, line and id of the signature.
// The loader cannot call the engine itself, since this package imports
// it.
func CheckExamples(sig *signatures.Signature) error {
	for i, text := range sig.Examples.Match {
		count, _, err := countFindings(sig, text)
		if err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("match example %d gives no finding", i+1)
		}
	}
	for i, text := range sig.Examples.NoMatch {
		count, first, err := countFindings(sig, text)
		if err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("no_match example %d gives %d finding(s), the first on line %d", i+1, count, first)
		}
	}
	return nil
}

// countFindings scans text for sig alone and returns how many findings
// it gives and the first finding's line.
func countFindings(sig *signatures.Signature, text string) (int64, int64, error) {
	res, err := scanBytes(context.Background(), []byte(text), []*signatures.Signature{sig}, Options{Workers: 1})
	if err != nil {
		return 0, 0, err
	}
	got := res.BySignature[sig.ID]
	if got.Count == 0 {
		return 0, 0, nil
	}
	return got.Count, got.Findings[0].StartLine, nil
}
