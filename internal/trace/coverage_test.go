package trace

import (
	"bytes"
	"context"
	"errors"
	"os"
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

// RX_WORKERS has an upper bound, so the environment cannot start an
// unbounded number of ripgrep processes; 0 or below means "not set".
func TestWorkerLimit_RX_WORKERSIsCapped(t *testing.T) {
	cases := []struct {
		value string
		want  int
	}{
		{"256", 256},
		{"100000", 256},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("RX_WORKERS", tc.value)
			if got := workerLimit(); got != tc.want {
				t.Errorf("RX_WORKERS=%s gives %d workers, want %d", tc.value, got, tc.want)
			}
		})
	}
	for _, unset := range []string{"0", "-4", "many"} {
		t.Run(unset, func(t *testing.T) {
			t.Setenv("RX_WORKERS", unset)
			t.Setenv("RX_MAX_SUBPROCESSES", "2")
			if got := workerLimit(); got < 1 || got > 2 {
				t.Errorf("RX_WORKERS=%s gives %d workers, want the computed 1 or 2", unset, got)
			}
		})
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
	// A bytes-wrapped payload holds the bytes it stands for.
	if ev.Match.Lines.Text != "hello\n" || ev.Match.Path.Text != "foo" {
		t.Errorf("Lines.Text = %q, Path.Text = %q; want the decoded bytes \"hello\\n\" and \"foo\"", ev.Match.Lines.Text, ev.Match.Path.Text)
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
