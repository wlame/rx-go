package trace

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// Table-driven JSON fixture tests covering the five event types rx
// recognizes plus a few malformed / unusual shapes.
func TestParseEvent_Match(t *testing.T) {
	line := []byte(`{"type":"match","data":{"path":{"text":"-"},"lines":{"text":"hello error\n"},"line_number":1,"absolute_offset":0,"submatches":[{"match":{"text":"error"},"start":6,"end":11}]}}`)
	ev, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev == nil || ev.Type != RgEventMatch || ev.Match == nil {
		t.Fatalf("want match event, got %#v", ev)
	}
	if ev.Match.LineNumber != 1 {
		t.Errorf("LineNumber = %d, want 1", ev.Match.LineNumber)
	}
	if ev.Match.AbsoluteOffset != 0 {
		t.Errorf("AbsoluteOffset = %d, want 0", ev.Match.AbsoluteOffset)
	}
	if ev.Match.Lines.Text != "hello error\n" {
		t.Errorf("Lines.Text = %q, want %q", ev.Match.Lines.Text, "hello error\n")
	}
	if len(ev.Match.Submatches) != 1 {
		t.Fatalf("submatches len = %d, want 1", len(ev.Match.Submatches))
	}
	sm := ev.Match.Submatches[0]
	if sm.Text() != "error" || sm.Start != 6 || sm.End != 11 {
		t.Errorf("submatch = %+v, want text=error start=6 end=11", sm)
	}
}

func TestParseEvent_Begin_End_Summary(t *testing.T) {
	cases := []struct {
		name    string
		line    []byte
		wantTyp RgEventType
	}{
		{
			name:    "begin",
			line:    []byte(`{"type":"begin","data":{"path":{"text":"file.txt"}}}`),
			wantTyp: RgEventBegin,
		},
		{
			name:    "end",
			line:    []byte(`{"type":"end","data":{"path":{"text":"-"},"binary_offset":null,"stats":{"elapsed":{"secs":0,"nanos":1,"human":"0s"},"searches":1,"searches_with_match":1,"bytes_searched":10,"bytes_printed":0,"matched_lines":0,"matches":0}}}`),
			wantTyp: RgEventEnd,
		},
		{
			name:    "summary",
			line:    []byte(`{"type":"summary","data":{"elapsed_total":{"secs":0,"nanos":1,"human":"0s"},"stats":{"elapsed":{"secs":0,"nanos":1,"human":"0s"},"searches":1,"searches_with_match":1,"bytes_searched":10,"bytes_printed":0,"matched_lines":0,"matches":0}}}`),
			wantTyp: RgEventSummary,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ev, err := ParseEvent(tc.line)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ev.Type != tc.wantTyp {
				t.Errorf("type = %s, want %s", ev.Type, tc.wantTyp)
			}
		})
	}
}

func TestParseEvent_ContextEvent(t *testing.T) {
	line := []byte(`{"type":"context","data":{"path":{"text":"-"},"lines":{"text":"surrounding\n"},"line_number":3,"absolute_offset":50,"submatches":[]}}`)
	ev, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Type != RgEventContext || ev.Context == nil {
		t.Fatalf("want context event")
	}
	if ev.Context.LineNumber != 3 || ev.Context.AbsoluteOffset != 50 {
		t.Errorf("context numbers mismatch: %+v", ev.Context)
	}
}

func TestParseEvent_EmptyAndNull(t *testing.T) {
	for _, input := range [][]byte{{}, []byte("  \n"), []byte("null")} {
		ev, err := ParseEvent(input)
		if err != nil {
			t.Errorf("ParseEvent(%q): unexpected error %v", input, err)
		}
		if ev != nil {
			t.Errorf("ParseEvent(%q): want nil event, got %+v", input, ev)
		}
	}
}

func TestParseEvent_UnknownType(t *testing.T) {
	line := []byte(`{"type":"teapot","data":{}}`)
	ev, err := ParseEvent(line)
	if !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("want ErrUnknownEvent, got %v", err)
	}
	if ev == nil || ev.Type != "teapot" {
		t.Errorf("want event with type teapot, got %+v", ev)
	}
}

func TestParseEvent_MalformedJSON(t *testing.T) {
	_, err := ParseEvent([]byte(`{"type":`))
	if err == nil {
		t.Error("want JSON parse error, got nil")
	}
}

func TestParseEvent_BytesPath(t *testing.T) {
	// ripgrep emits {"bytes": "<base64>"} for non-UTF-8 paths.
	line := []byte(`{"type":"begin","data":{"path":{"bytes":"aGVsbG8="}}}`)
	ev, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Begin.Path.Text != "aGVsbG8=" {
		t.Errorf("Path.Text = %q, want base64 literal stored unchanged", ev.Begin.Path.Text)
	}
}

// ============================================================================
// Streaming
// ============================================================================

func TestStreamEvents_HandlesMultilineStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"begin","data":{"path":{"text":"-"}}}`,
		`{"type":"match","data":{"path":{"text":"-"},"lines":{"text":"x\n"},"line_number":1,"absolute_offset":0,"submatches":[]}}`,
		`{"type":"match","data":{"path":{"text":"-"},"lines":{"text":"y\n"},"line_number":2,"absolute_offset":2,"submatches":[]}}`,
		`{"type":"end","data":{"path":{"text":"-"},"stats":{"elapsed":{"secs":0,"nanos":1,"human":"0s"},"searches":1,"searches_with_match":1,"bytes_searched":2,"bytes_printed":0,"matched_lines":2,"matches":2}}}`,
	}, "\n")

	var matchCount int
	err := StreamEvents(context.Background(), bytes.NewReader([]byte(stream)),
		func(ev *RgEvent, err error) error {
			if err != nil {
				t.Errorf("unexpected per-event error: %v", err)
				return nil
			}
			if ev.Type == RgEventMatch {
				matchCount++
			}
			return nil
		})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if matchCount != 2 {
		t.Errorf("matchCount = %d, want 2", matchCount)
	}
}

func TestStreamEvents_Cancellation(t *testing.T) {
	// Build a large stream so the scanner has many lines to consume.
	var stream strings.Builder
	for i := 0; i < 10_000; i++ {
		stream.WriteString(`{"type":"match","data":{"path":{"text":"-"},"lines":{"text":"x\n"},"line_number":1,"absolute_offset":0,"submatches":[]}}` + "\n")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel upfront — should stop before consuming anything

	err := StreamEvents(ctx, strings.NewReader(stream.String()),
		func(ev *RgEvent, err error) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}
