package main

// `--json` exists so a script can read the answer, which only holds if
// nothing else reaches stdout. A progress note, a warning or a stray
// Println in front of the document turns the flag into a parse error.
//
// rx-python printed exactly such a note on its compressed-file path, and
// nothing in either backend said the stdout stream was reserved. These
// tests say it: for every subcommand that takes `--json`, and for each
// storage form a file can arrive in, stdout parses.

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonFixtureLines is large enough that samples and trace both have to
// reach past the first chunk, and small enough to stay fast.
const jsonFixtureLines = 500

// writeJSONFixtures builds the same content as a plain file, a gzip
// member and a seekable zstd, and returns them keyed by storage form.
//
// The .zst is produced by `rx compress` rather than by a library call:
// it is the seekable layout the samples path actually reads, and running
// the subcommand here also covers its own `--json` output.
func writeJSONFixtures(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()

	var body strings.Builder
	for i := 1; i <= jsonFixtureLines; i++ {
		fmt.Fprintf(&body, "line %d alpha beta gamma\n", i)
	}

	plain := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write plain fixture: %v", err)
	}

	gzPath := filepath.Join(dir, "app.log.gz")
	gzFile, err := os.Create(gzPath) //nolint:gosec // path is inside t.TempDir
	if err != nil {
		t.Fatalf("create gzip fixture: %v", err)
	}
	gzWriter := gzip.NewWriter(gzFile)
	if _, err := gzWriter.Write([]byte(body.String())); err != nil {
		t.Fatalf("write gzip fixture: %v", err)
	}
	if err := gzWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	if err := gzFile.Close(); err != nil {
		t.Fatalf("close gzip fixture: %v", err)
	}

	zstPath := filepath.Join(dir, "seekable.log.zst")
	code, stdout, stderr := runRx(t, "compress", plain, "-o", zstPath, "--json")
	if code != 0 {
		t.Fatalf("rx compress: exit %d, stderr: %s", code, stderr)
	}
	assertOnlyJSON(t, "compress", stdout)

	return map[string]string{"plain": plain, "gzip": gzPath, "seekable": zstPath}
}

// assertOnlyJSON fails with the offending stdout rather than a bare
// unmarshal error, because "invalid character 'P'" does not say which
// line got in the way.
func assertOnlyJSON(t *testing.T, label, stdout string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		head := stdout
		if len(head) > 400 {
			head = head[:400]
		}
		t.Errorf("%s: stdout is not JSON (%v); it starts:\n%s", label, err, head)
	}
}

func TestJSONOutput_StdoutCarriesNothingButTheDocument(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	fixtures := writeJSONFixtures(t)

	// Each subcommand named with the arguments it needs; the path is
	// appended per storage form below.
	subcommands := map[string][]string{
		"samples": {"samples", "--lines=100", "--json"},
		"trace":   {"trace", "alpha", "--json"},
		"index":   {"index", "--json"},
	}

	for form, path := range fixtures {
		for name, args := range subcommands {
			label := name + "/" + form
			t.Run(label, func(t *testing.T) {
				// The path goes last for trace, whose first positional is
				// the pattern; for the others it is the only positional,
				// so appending works for all three.
				code, stdout, stderr := runRx(t, append(append([]string{}, args...), path)...)
				if code != 0 {
					t.Fatalf("%s: exit %d, stderr: %s", label, code, stderr)
				}
				assertOnlyJSON(t, label, stdout)
			})
		}
	}
}
