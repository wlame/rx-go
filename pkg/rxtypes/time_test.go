package rxtypes

import (
	"encoding/json"
	"testing"
	"time"
)

func TestISOTime_MarshalJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "microsecond precision UTC",
			in:   time.Date(2026, 4, 17, 12, 34, 56, 123456789, time.UTC),
			want: `"2026-04-17T12:34:56.123456"`, // nanos truncated to micros
		},
		{
			name: "whole second UTC pads microseconds",
			in:   time.Date(2026, 4, 17, 12, 34, 56, 0, time.UTC),
			want: `"2026-04-17T12:34:56.000000"`,
		},
		{
			name: "non-UTC input converted to UTC",
			in:   time.Date(2026, 4, 17, 12, 34, 56, 0, time.FixedZone("EST", -5*3600)),
			want: `"2026-04-17T17:34:56.000000"`,
		},
		{
			name: "zero value marshals to null",
			in:   time.Time{},
			want: `null`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(NewISOTime(tc.in))
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestISOTime_UnmarshalJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		want    time.Time
		wantErr bool
	}{
		{
			name:  "python canonical 6-digit microseconds",
			input: `"2026-04-17T12:34:56.123456"`,
			want:  time.Date(2026, 4, 17, 12, 34, 56, 123456000, time.UTC),
		},
		{
			name:  "python second precision (no fractional)",
			input: `"2026-04-17T12:34:56"`,
			want:  time.Date(2026, 4, 17, 12, 34, 56, 0, time.UTC),
		},
		{
			name:  "RFC3339 with Z suffix",
			input: `"2026-04-17T12:34:56Z"`,
			want:  time.Date(2026, 4, 17, 12, 34, 56, 0, time.UTC),
		},
		{
			name:  "RFC3339Nano",
			input: `"2026-04-17T12:34:56.123456789Z"`,
			want:  time.Date(2026, 4, 17, 12, 34, 56, 123456789, time.UTC),
		},
		{
			name:  "null literal decodes to zero",
			input: `null`,
			want:  time.Time{},
		},
		{
			name:  "empty string decodes to zero",
			input: `""`,
			want:  time.Time{},
		},
		{
			name:    "garbage fails",
			input:   `"not-a-time"`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got ISOTime
			err := json.Unmarshal([]byte(tc.input), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("got %v, want %v", got.Time, tc.want)
			}
		})
	}
}

func TestISOTime_RoundTrip(t *testing.T) {
	t.Parallel()
	// A round trip through marshal → unmarshal should be exact to the
	// microsecond. Losing sub-microsecond precision is acceptable and
	// intentional (Python parity).
	original := time.Date(2026, 4, 17, 12, 34, 56, 654321000, time.UTC)
	data, err := json.Marshal(NewISOTime(original))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded ISOTime
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !decoded.Equal(original) {
		t.Errorf("round-trip drift: %v -> %v", original, decoded.Time)
	}
}

func TestISOTime_String(t *testing.T) {
	t.Parallel()
	in := NewISOTime(time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC))
	got := in.String()
	want := "2026-04-17T12:00:00.000000"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	var zero ISOTime
	if zero.String() != "" {
		t.Errorf("zero.String() = %q, want empty", zero.String())
	}
}
