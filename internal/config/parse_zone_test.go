package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// A zone a request names follows the RX_LOG_TZ rule: UTC, an IANA name
// or ±HH:MM with two digits in each field, up to 18 hours. Anything
// else, `local` and an empty value included, is refused with
// ErrInvalidZone and a message that says what is accepted.
func TestParseZone_AcceptsWhatRXLogTZAccepts(t *testing.T) {
	cases := []struct {
		value      string
		wantName   string
		wantOffset int
	}{
		{"UTC", "UTC", 0},
		{"Asia/Tokyo", "Asia/Tokyo", 9 * 3600},
		{"Europe/Berlin", "Europe/Berlin", 3600},
		{"+05:30", "+05:30", 5*3600 + 30*60},
		{"-07:00", "-07:00", -7 * 3600},
		{"+18:00", "+18:00", 18 * 3600},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			got, err := ParseZone(tc.value)
			if err != nil {
				t.Fatalf("ParseZone(%q): %v", tc.value, err)
			}
			if got.Name != tc.wantName || got.Location == nil || offsetSeconds(got.Location) != tc.wantOffset {
				t.Fatalf("ParseZone(%q) = %q at %+v, want %q at offset %d", tc.value, got.Name, got.Location, tc.wantName, tc.wantOffset)
			}
		})
	}
}

func TestParseZone_RefusesEverythingElse(t *testing.T) {
	for _, value := range []string{
		"", "Mars/Base", "+25:00", "+-1:00", "+18:01", "+5:30", "+05:60", "local", "Local",
		"../../etc/passwd", "/etc/localtime", strings.Repeat("A", 65), "Europe/Berlin ",
	} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			got, err := ParseZone(value)
			if !errors.Is(err, ErrInvalidZone) {
				t.Fatalf("ParseZone(%q) = %+v, %v; want ErrInvalidZone", value, got, err)
			}
			if !strings.Contains(err.Error(), "an IANA zone name or ±HH:MM") {
				t.Errorf("error %q does not say what is accepted", err)
			}
		})
	}
}

// Any value either names a zone whose name is the value, or is refused
// with ErrInvalidZone; nothing panics, and nothing longer than the bound
// is looked up.
func FuzzParseZone(f *testing.F) {
	for _, seed := range []string{"UTC", "Asia/Tokyo", "+05:30", "-07:00", "+-1:00", "local", "Mars/Base", "", "\x00", "../x"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got, err := ParseZone(value)
		if err != nil {
			if !errors.Is(err, ErrInvalidZone) {
				t.Fatalf("ParseZone(%q): error %v does not wrap ErrInvalidZone", value, err)
			}
			return
		}
		if got.Location == nil || got.Name != value || len(value) > maxZoneValueBytes || !utf8.ValidString(value) {
			t.Fatalf("ParseZone(%q) = %+v", value, got)
		}
	})
}
