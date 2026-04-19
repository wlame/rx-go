package trace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp/syntax"
	"strings"
	"syscall"
	"testing"
)

// ============================================================================
// Small coverage boosters: trivial helpers and uncovered branches that
// are worth exercising but don't justify their own file.
// ============================================================================

func TestNoopHookFirer_IsDropIn(t *testing.T) {
	// Smoke test — OnFile and OnMatch must never panic regardless of
	// inputs. These are the "default fallback" hook for CLI usage.
	hf := NoopHookFirer{}
	hf.OnFile(context.Background(), "x", FileInfo{})
	hf.OnMatch(context.Background(), "x", MatchInfo{Pattern: "p", Offset: 1, LineNumber: 2})
}

func TestIsBrokenPipe_DetectsKnownErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrClosed", os.ErrClosed, true},
		{"plain broken pipe string", errors.New("write pipe: broken pipe"), true},
		{"EPIPE marker", errors.New("write: EPIPE"), true},
		{"unrelated error", errors.New("some other error"), false},
		// On Linux, syscall.EPIPE stringifies as "broken pipe" so our
		// substring check (intentionally) treats it as a broken-pipe
		// error. That IS the correct behavior.
		{"syscall EPIPE", syscall.EPIPE, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := isBrokenPipe(tc.err)
			if got != tc.want {
				t.Errorf("isBrokenPipe(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestTrimTrailingNewline_Variants(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"hello", "hello"},
		{"hello\n", "hello"},
		{"hello\r\n", "hello"},
		{"hello\nworld\n", "hello\nworld"},
	}
	for _, tc := range cases {
		got := trimTrailingNewline(tc.in)
		if got != tc.want {
			t.Errorf("trimTrailingNewline(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFilterIncompatibleRgArgs(t *testing.T) {
	in := []string{"--byte-offset", "-i", "--only-matching", "-n"}
	got := filterIncompatibleRgArgs(in)
	want := []string{"-i", "-n"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// nil in → nil out (skip allocation)
	if filterIncompatibleRgArgs(nil) != nil {
		t.Error("nil input should produce nil output")
	}
}

func TestReadSourceLine_FindsExactLine(t *testing.T) {
	content := "line one\nline two\nline three\n"
	p := mustWriteFile(t, []byte(content))
	got, err := ReadSourceLine(p, 2, false)
	if err != nil {
		t.Fatalf("ReadSourceLine: %v", err)
	}
	if got != "line two" {
		t.Errorf("got %q, want %q", got, "line two")
	}
}

func TestReadSourceLine_OutOfRange(t *testing.T) {
	p := mustWriteFile(t, []byte("only line\n"))
	_, err := ReadSourceLine(p, 42, false)
	if err == nil {
		t.Error("want error for out-of-range line, got nil")
	}
}

func TestWorkerLimit_Precedence(t *testing.T) {
	t.Setenv("RX_WORKERS", "7")
	if got := workerLimit(); got != 7 {
		t.Errorf("RX_WORKERS=7 → %d, want 7", got)
	}
	t.Setenv("RX_WORKERS", "")
	t.Setenv("RX_MAX_SUBPROCESSES", "3")
	// Whichever is smaller of runtime.NumCPU() and 3 wins.
	if got := workerLimit(); got > 3 {
		t.Errorf("RX_MAX_SUBPROCESSES=3 but got %d", got)
	}
}

// TestParseEvent_BytesField exercises the base64 fallback path —
// ripgrep emits {"bytes": "..."} when the path contains non-UTF-8.
func TestParseEvent_BytesField(t *testing.T) {
	// Hand-construct a match event with {"bytes": "..."} in lines.
	line := []byte(`{"type":"match","data":{"path":{"bytes":"Zm9v"},"lines":{"bytes":"aGVsbG8K"},"line_number":1,"absolute_offset":0,"submatches":[]}}`)
	ev, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.Type != RgEventMatch {
		t.Fatalf("type = %s, want match", ev.Type)
	}
	// bytes-wrapped lines still produce a non-empty Text string.
	if ev.Match.Lines.Text == "" {
		t.Errorf("Lines.Text is empty; want base64 literal preserved")
	}
}

func TestStreamEvents_BlankLinesAreSkipped(t *testing.T) {
	in := []byte(strings.Join([]string{
		"",
		`{"type":"match","data":{"path":{"text":"-"},"lines":{"text":"x\n"},"line_number":1,"absolute_offset":0,"submatches":[]}}`,
		"",
		"",
		`{"type":"end","data":{"path":{"text":"-"},"stats":{"elapsed":{"secs":0,"nanos":1,"human":"0s"},"searches":1,"searches_with_match":1,"bytes_searched":1,"bytes_printed":0,"matched_lines":1,"matches":1}}}`,
	}, "\n"))
	var count int
	err := StreamEvents(context.Background(), bytes.NewReader(in),
		func(ev *RgEvent, err error) error {
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if ev != nil {
				count++
			}
			return nil
		})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if count != 2 {
		t.Errorf("event count = %d, want 2", count)
	}
}

// TestHasInlineFlag detects (?i) at the front.
func TestHasInlineFlag_DetectsInlineCase(t *testing.T) {
	// `(?i)foo` parses as a single concat-leaf ("foo") with the
	// FoldCase flag. The expression's Flags field carries it, which
	// is what hasInlineFlag inspects.
	if !hasInlineFlag("(?i)foo", syntax.FoldCase) {
		t.Errorf("hasInlineFlag did not detect (?i) prefix")
	}
	if hasInlineFlag("foo", syntax.FoldCase) {
		t.Errorf("hasInlineFlag returned true for a plain pattern")
	}
	// Parse errors → false (not a crash).
	if hasInlineFlag("(foo", syntax.FoldCase) {
		t.Errorf("hasInlineFlag on broken regex should return false")
	}
}
