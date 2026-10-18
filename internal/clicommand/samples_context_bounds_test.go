package clicommand

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// -1 is the default on each context flag; a count below it has no
// meaning and exits 2 instead of answering a window that misses the
// target line, as it did.
func TestSamplesCommand_ContextBelowMinusOneIsRefused(t *testing.T) {
	f := thirtyLineFixture(t)

	for want, p := range map[string]samplesParams{
		"--context must be -1 or more, got -2": {ctxLines: -2},
		"--before must be -1 or more, got -2":  {ctxLines: 3, beforeCtx: -2, beforeSet: true},
		"--after must be -1 or more, got -3":   {ctxLines: 3, afterCtx: -3, afterSet: true},
	} {
		var out bytes.Buffer
		p.path, p.lines, p.jsonOutput = f, []string{"10"}, true
		err := runSamples(&out, p)
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
			t.Errorf("%+v: got %v, want exit code %d", p, err, ExitUsageError)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: message %q, want %q", p, err.Error(), want)
		}
		if out.Len() != 0 {
			t.Errorf("%+v: stdout should be empty, got %q", p, out.String())
		}
	}
}
