package clicommand

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveFixture writes a gzipped archive, which is not text.
func archiveFixture(t *testing.T) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "logs.tar.gz")
	var body bytes.Buffer
	w := gzip.NewWriter(&body)
	_, _ = w.Write(bytes.Repeat([]byte{0, 1, 2, 3}, 4096))
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := os.WriteFile(archive, body.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return archive
}

func TestIndex_JSONGivesTheReasonForEachSkippedFile(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	tiny := tinyLogFile(t)

	result := runIndexJSON(t, indexParams{paths: []string{tiny}})

	skipped, _ := result["skipped"].([]any)
	if len(skipped) != 1 || skipped[0] != tiny {
		t.Errorf("skipped: got %v, want the tiny log", skipped)
	}
	reasons, _ := result["skip_reasons"].([]any)
	want := map[string]string{tiny: "file size 17 bytes is below threshold 1048576 bytes"}
	assertSkipReasons(t, reasons, want)
}

func TestIndex_JSONSaysAnArchiveIsNotText(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	archive := archiveFixture(t)

	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{archive}, threshold: &zero})

	reasons, _ := result["skip_reasons"].([]any)
	assertSkipReasons(t, reasons, map[string]string{archive: archiveReason})
}

func TestIndex_HumanOutputNamesEachSkippedFileWithItsReason(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	archive := archiveFixture(t)

	zero := 0
	var out bytes.Buffer
	if err := runIndex(&out, indexParams{paths: []string{archive}, threshold: &zero}); err != nil {
		t.Fatalf("runIndex: %v", err)
	}

	want := "No files indexed.\nSkipped 1 files:\n  " + archive + ": " + archiveReason + "\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// assertSkipReasons checks skip_reasons holds exactly the given
// path → reason pairs.
func assertSkipReasons(t *testing.T, reasons []any, want map[string]string) {
	t.Helper()
	if len(reasons) != len(want) {
		t.Fatalf("skip_reasons: got %v, want %d entries", reasons, len(want))
	}
	for _, raw := range reasons {
		item, _ := raw.(map[string]any)
		path, _ := item["path"].(string)
		reason, _ := item["reason"].(string)
		if want[path] != reason {
			t.Errorf("skip_reasons[%s]: got %q, want %q", path, reason, want[path])
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("skip_reasons[%s] has no reason", path)
		}
	}
}

// archiveReason is why archiveFixture is not indexed: its decompressed
// text holds NUL bytes, as a tar stream's does.
const archiveReason = "not a text file: a NUL byte in the first 8 KiB of its decompressed text"
