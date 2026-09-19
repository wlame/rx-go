package trace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
)

// requirePatternError fails t unless err is ErrInvalidPattern and its
// message carries every one of wants.
func requirePatternError(t *testing.T, err error, wants ...string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidPattern) {
		t.Fatalf("err = %v, want ErrInvalidPattern", err)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not carry %q", err.Error(), want)
		}
	}
}

// ripgrep exits 2 for every fatal error, so the words on stderr are all
// that tells a pattern it cannot compile from a file it cannot read.
// Each stderr below is what ripgrep prints, word for word.
func TestRipgrepExitError_TellsPatternErrorsFromOtherFailures(t *testing.T) {
	patterns := map[string]string{"p1": "("}
	order := []string{"p1"}
	cases := []struct {
		name      string
		stderr    string
		isPattern bool
		want      string
	}{
		{
			name:      "default engine",
			stderr:    "rg: regex parse error:\n    (?:()\n    ^\nerror: unclosed group",
			isPattern: true,
			want:      `invalid regex pattern "(": unclosed group`,
		},
		{
			// The offset counts into the group ripgrep wraps every
			// pattern in, so it is left out.
			name:      "PCRE2 compile error",
			stderr:    "rg: PCRE2: error compiling pattern at offset 5: missing closing parenthesis",
			isPattern: true,
			want:      `invalid regex pattern "(": PCRE2: error compiling pattern: missing closing parenthesis`,
		},
		{
			name:      "ripgrep built without PCRE2",
			stderr:    "rg: PCRE2 is not available in this build of ripgrep",
			isPattern: true,
			want:      "PCRE2 is not available in this build of ripgrep",
		},
		{
			name:      "a line break in a pattern",
			stderr:    "rg: the literal \"\\n\" is not allowed in a regex\n\nConsider enabling multiline mode with the --multiline flag (or -U for short).\nWhen multiline mode is enabled, new line characters can be matched.",
			isPattern: true,
			want:      `invalid regex pattern "(": the literal "\n" is not allowed in a regex`,
		},
		{
			name:      "a pattern too large to compile",
			stderr:    "rg: compiled regex exceeds size limit of 104857600",
			isPattern: true,
			want:      `invalid regex pattern "(": compiled regex exceeds size limit of 104857600`,
		},
		{
			name:   "an input ripgrep cannot read",
			stderr: "rg: <stdin>: Permission denied (os error 13)",
			want:   "rg exit 2: rg: <stdin>: Permission denied (os error 13)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ripgrepExitError(2, tc.stderr, patterns, order)

			if got := errors.Is(err, ErrInvalidPattern); got != tc.isPattern {
				t.Fatalf("errors.Is(err, ErrInvalidPattern) = %v, want %v (err: %v)", got, tc.isPattern, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message = %q, want it to carry %q", err.Error(), tc.want)
			}
		})
	}
}

// The missing feature is named with what the caller can do about it,
// not as a mistake in the pattern.
func TestRipgrepExitError_MissingPCRE2SaysHowToSearchAnyway(t *testing.T) {
	err := ripgrepExitError(2, "rg: PCRE2 is not available in this build of ripgrep",
		map[string]string{"p1": "a(?=b)"}, []string{"p1"})

	requirePatternError(t, err, "PCRE2 is not available", "-P", "rg --pcre2-version")
	if strings.Contains(err.Error(), "invalid regex pattern") {
		t.Errorf("a valid pattern is not invalid: %q", err.Error())
	}
}

// writePatternErrorFixtures writes the same text as a plain file, a
// gzip file and a seekable zstd file of several frames.
func writePatternErrorFixtures(t *testing.T) map[string]string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= 2000; i++ {
		b.WriteString("LINE " + strconv.Itoa(i) + " a(b\n")
	}
	text := []byte(b.String())
	return map[string]string{
		"plain":    writeTextFile(t, "a.log", text),
		"gzip":     writeGzipFile(t, text),
		"seekable": writeSeekableZstdFile(t, text, 4096),
	}
}

// A pattern PCRE2 cannot compile ends the whole trace with the pattern's
// error before any file is read, whatever the files are stored as and
// however many patterns there are. It used to be answered as "0 matches,
// file skipped".
func TestTrace_InvalidPCRE2PatternFailsTheTraceBeforeAnyRead(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	files := writePatternErrorFixtures(t)
	dir := t.TempDir()
	for i := 0; i < 70; i++ {
		_ = os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)+".log"), []byte("a(b\n"), 0o600)
	}
	targets := map[string][]string{
		"plain":                       {files["plain"]},
		"gzip":                        {files["gzip"]},
		"seekable":                    {files["seekable"]},
		"a directory of 70 files":     {dir},
		"an empty directory":          {t.TempDir()},
		"every form of the same text": {files["plain"], files["gzip"], files["seekable"]},
	}
	patternSets := map[string][]string{
		"one pattern":                  {"("},
		"the bad one after a good one": {"a", "("},
	}
	for name, paths := range targets {
		for setName, patterns := range patternSets {
			t.Run(name+", "+setName, func(t *testing.T) {
				read := false
				opts := Options{RgExtraArgs: []string{"-P"}, beforeRead: func() { read = true }}

				resp, err := New().RunWithOptions(context.Background(), paths, patterns, opts)

				requirePatternError(t, err, "PCRE2")
				if resp != nil {
					t.Errorf("a failed trace answered %+v", resp)
				}
				if read {
					t.Error("a file was read before the pattern was refused")
				}
			})
		}
	}
}

// Every path that runs ripgrep over a file's text classifies a pattern
// ripgrep refuses as the pattern's error, so a pattern error can never
// pass for a file error on any of them.
func TestEverySearchPathReportsARefusedPatternAsThePatternsError(t *testing.T) {
	requireRipgrep(t)
	files := writePatternErrorFixtures(t)
	ids := map[string]string{"p1": "("}
	order := []string{"p1"}
	flags := []string{"-P"}
	ctx := context.Background()

	t.Run("chunk", func(t *testing.T) {
		src := pinForTest(t, files["plain"])
		fi, err := os.Stat(files["plain"])
		if err != nil {
			t.Fatal(err)
		}
		task := FileTask{Source: src, Count: fi.Size()}
		_, err = ProcessChunk(ctx, ChunkRequest{Task: task, PatternIDs: ids, PatternOrder: order, RgExtraArgs: flags})
		requirePatternError(t, err, "PCRE2")
	})
	t.Run("compressed stream", func(t *testing.T) {
		_, _, _, err := ProcessCompressed(ctx, pinForTest(t, files["gzip"]), compression.FormatGzip,
			ids, order, flags, 0, 0, nil)
		requirePatternError(t, err, "PCRE2")
	})
	t.Run("seekable frames", func(t *testing.T) {
		_, _, _, err := ProcessSeekable(ctx, pinForTest(t, files["seekable"]), ids, order, flags, 0, 0, nil)
		requirePatternError(t, err, "PCRE2")
	})
	t.Run("pattern credit", func(t *testing.T) {
		line := MatchRaw{Offset: 0, End: int64(len("LINE 1 a(b\n")), LineText: "LINE 1 a(b"}
		_, err := creditPatterns(ctx, creditRequest{
			files:        []creditFile{{source: pinForTest(t, files["plain"]), lines: []MatchRaw{line}}},
			patternIDs:   map[string]string{"p1": "a", "p2": "("},
			patternOrder: []string{"p1", "p2"},
			rgExtraArgs:  flags,
		})
		requirePatternError(t, err, "PCRE2")
	})
}

// A valid PCRE2 pattern is searched as before: look-around works and
// nothing is skipped.
func TestTrace_ValidPCRE2PatternIsSearched(t *testing.T) {
	requireRipgrep(t)
	requirePCRE2(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	files := writePatternErrorFixtures(t)
	for form, path := range files {
		t.Run(form, func(t *testing.T) {
			resp, err := New().RunWithOptions(context.Background(), []string{path},
				[]string{`(?<=LINE 7 )a\((?=b)`}, Options{RgExtraArgs: []string{"-P"}})
			if err != nil {
				t.Fatalf("RunWithOptions: %v", err)
			}
			if len(resp.Matches) != 1 || resp.Matches[0].AbsoluteLineNumber != 7 {
				t.Errorf("matches = %+v, want line 7 alone", resp.Matches)
			}
			if len(resp.SkippedFiles) != 0 {
				t.Errorf("skipped_files = %v, want none", resp.SkippedFiles)
			}
		})
	}
}

// A file that cannot be read is still that file's failure: it lands in
// skipped_files, and the other files of the trace are searched, with a
// PCRE2 pattern as with any other.
func TestTrace_UnreadableFileIsSkippedUnderAValidPCRE2Pattern(t *testing.T) {
	requireRipgrep(t)
	requirePCRE2(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	readable := filepath.Join(dir, "readable.log")
	unreadable := filepath.Join(dir, "unreadable.log")
	for _, p := range []string{readable, unreadable} {
		if err := os.WriteFile(p, []byte("a(b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}

	resp, err := New().RunWithOptions(context.Background(), []string{readable, unreadable},
		[]string{`a\((?=b)`}, Options{RgExtraArgs: []string{"-P"}})
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.Matches) != 1 {
		t.Errorf("matches = %+v, want the readable file's one", resp.Matches)
	}
	if !slices.Contains(resp.SkippedFiles, unreadable) {
		t.Errorf("skipped_files = %v, want %s in it", resp.SkippedFiles, unreadable)
	}
}
