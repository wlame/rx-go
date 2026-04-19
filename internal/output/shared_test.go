package output

import "testing"

func TestHumanSize(t *testing.T) {
	// Ports from rx-python/tests/test_human_readable_size.py.
	// Expected values come from running `human_readable_size(N)` in Python.
	cases := []struct {
		name  string
		bytes int64
		want  string
	}{
		{"zero", 0, "0.00 B"},
		{"one byte", 1, "1.00 B"},
		{"500 bytes", 500, "500.00 B"},
		{"just under KB", 1023, "1023.00 B"},
		{"exactly 1 KB", 1024, "1.00 KB"},
		{"1.5 KB", 1536, "1.50 KB"},
		{"5 MB", 5 * 1024 * 1024, "5.00 MB"},
		{"2 GB", 2 * 1024 * 1024 * 1024, "2.00 GB"},
		{"3 TB", 3 * 1024 * 1024 * 1024 * 1024, "3.00 TB"},
		{"2 PB", 2 * 1024 * 1024 * 1024 * 1024 * 1024, "2.00 PB"},
		{"10 PB", 10 * 1024 * 1024 * 1024 * 1024 * 1024, "10.00 PB"},
		{"1500 bytes → two decimals", 1500, "1.46 KB"},
		{"negative", -1024, "-1.00 KB"},
		{"negative byte", -500, "-500.00 B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HumanSize(tc.bytes)
			if got != tc.want {
				t.Errorf("HumanSize(%d) = %q, want %q", tc.bytes, got, tc.want)
			}
		})
	}
}

// ANSI constants exist and are non-empty — catches any accidental typo
// that might silently strip a color code.
func TestANSIColorsDefined(t *testing.T) {
	codes := map[string]string{
		"Reset":        ColorReset,
		"Bold":         ColorBold,
		"Grey":         ColorGrey,
		"Red":          ColorRed,
		"BrightRed":    ColorBrightRed,
		"Green":        ColorGreen,
		"BoldGreen":    ColorBoldGreen,
		"Yellow":       ColorYellow,
		"BrightYellow": ColorBrightYellow,
		"Blue":         ColorBlue,
		"Magenta":      ColorMagenta,
		"BoldMagenta":  ColorBoldMagenta,
		"Cyan":         ColorCyan,
		"BoldCyan":     ColorBoldCyan,
		"BrightCyan":   ColorBrightCyan,
		"LightGrey":    ColorLightGrey,
		"White":        ColorWhite,
		"Orange214":    ColorOrange214,
		"Orange208":    ColorOrange208,
	}
	for name, c := range codes {
		if len(c) == 0 {
			t.Errorf("%s is empty", name)
		}
		// Every code must start with the CSI intro: ESC + '['.
		if c[0] != 0x1b || c[1] != '[' {
			t.Errorf("%s = %q; expected CSI prefix", name, c)
		}
		// Every code must end with 'm' (SGR).
		if c[len(c)-1] != 'm' {
			t.Errorf("%s = %q; expected SGR terminator 'm'", name, c)
		}
	}
}
