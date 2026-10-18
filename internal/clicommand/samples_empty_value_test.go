package clicommand

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A flag given with nothing in it is a usage error that names the flag
// (`--lines`, `--offsets`), not an "empty spec" that names none.
func TestSamplesCommand_EmptyValueNamesTheFlag(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(f, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		params samplesParams
		want   string
	}{
		{samplesParams{path: f, lines: []string{""}}, "--lines needs a value"},
		{samplesParams{path: f, lines: []string{" "}}, "--lines needs a value"},
		{samplesParams{path: f, offsets: []string{""}}, "--offsets needs a value"},
	} {
		var out bytes.Buffer
		err := runSamples(&out, tc.params)
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
			t.Fatalf("%+v: got %v, want exit code %d", tc.params, err, ExitUsageError)
		}
		if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "empty spec") {
			t.Errorf("%+v: error %q, want it to say %q and not \"empty spec\"", tc.params, err.Error(), tc.want)
		}
	}
}
