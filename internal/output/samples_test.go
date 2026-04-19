package output

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TestFormatSamplesCLI_NoColorSingleLine covers the plain-text single-
// line format:
//
//	File: <path>
//	Context: B before, A after
//
//	=== <path>:<line>:<offset> ===
//	<context lines>
func TestFormatSamplesCLI_NoColorSingleLine(t *testing.T) {
	resp := &rxtypes.SamplesResponse{
		Path:          "/var/log/app.log",
		Lines:         map[string]int64{"5": 100},
		BeforeContext: 1,
		AfterContext:  1,
		Samples:       map[string][]string{"5": {"line 4", "line 5", "line 6"}},
	}
	got := FormatSamplesCLI(resp, false, "")
	want := strings.Join([]string{
		"File: /var/log/app.log",
		"Context: 1 before, 1 after",
		"",
		"=== /var/log/app.log:5:100 ===",
		"line 4",
		"line 5",
		"line 6",
		"",
	}, "\n")
	if got != want {
		t.Errorf("plain output mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestFormatSamplesCLI_NoColorRange covers range keys ("10-12").
func TestFormatSamplesCLI_NoColorRange(t *testing.T) {
	resp := &rxtypes.SamplesResponse{
		Path:          "/tmp/a.log",
		Lines:         map[string]int64{"10-12": -1},
		BeforeContext: 0,
		AfterContext:  0,
		Samples:       map[string][]string{"10-12": {"row a", "row b", "row c"}},
	}
	got := FormatSamplesCLI(resp, false, "")
	if !strings.Contains(got, "=== /tmp/a.log:10-12 ===") {
		t.Errorf("missing range header: %q", got)
	}
	if !strings.Contains(got, "row a\nrow b\nrow c") {
		t.Errorf("content lines missing: %q", got)
	}
}

// TestFormatSamplesCLI_ColorizedSingleLine verifies ANSI codes are
// emitted around each header segment.
func TestFormatSamplesCLI_ColorizedSingleLine(t *testing.T) {
	resp := &rxtypes.SamplesResponse{
		Path:          "/var/log/app.log",
		Lines:         map[string]int64{"5": 100},
		BeforeContext: 1,
		AfterContext:  1,
		Samples:       map[string][]string{"5": {"ctx"}},
	}
	got := FormatSamplesCLI(resp, true, "")
	// The exact header string from the Python reference:
	//   \033[96m/var/log/app.log\033[0m\033[90m:\033[0m\033[93m5\033[0m\033[90m:\033[0m\033[37m100\033[0m
	// Wrapped in "=== ... ==="
	wantHeader := "=== " + ColorBrightCyan + "/var/log/app.log" + ColorReset +
		ColorGrey + ":" + ColorReset +
		ColorBrightYellow + "5" + ColorReset +
		ColorGrey + ":" + ColorReset +
		ColorLightGrey + "100" + ColorReset + " ==="
	if !strings.Contains(got, wantHeader) {
		t.Errorf("colored header not found\nwant-contains:\n%q\n\ngot:\n%q", wantHeader, got)
	}
}

// TestFormatSamplesCLI_RegexHighlight verifies the bright-red wrap.
func TestFormatSamplesCLI_RegexHighlight(t *testing.T) {
	resp := &rxtypes.SamplesResponse{
		Path:          "/x.log",
		Lines:         map[string]int64{"1": 0},
		BeforeContext: 0,
		AfterContext:  0,
		Samples:       map[string][]string{"1": {"error occurred at line 1"}},
	}
	got := FormatSamplesCLI(resp, true, "error")
	wantHighlight := ColorBrightRed + "error" + ColorReset
	if !strings.Contains(got, wantHighlight) {
		t.Errorf("regex highlight missing: got %q", got)
	}
}

// TestFormatSamplesCLI_RegexHighlight_NoColorIgnored — when color is
// disabled, regex highlighting must be dropped (Python parity).
func TestFormatSamplesCLI_RegexHighlight_NoColorIgnored(t *testing.T) {
	resp := &rxtypes.SamplesResponse{
		Path:          "/x.log",
		Lines:         map[string]int64{"1": 0},
		BeforeContext: 0,
		AfterContext:  0,
		Samples:       map[string][]string{"1": {"error occurred"}},
	}
	got := FormatSamplesCLI(resp, false, "error")
	if strings.Contains(got, ColorBrightRed) || strings.Contains(got, ColorReset) {
		t.Errorf("ANSI codes leaked into non-color output: %q", got)
	}
}

// TestFormatSamplesCLI_Compressed shows the "Compressed:" header.
func TestFormatSamplesCLI_Compressed(t *testing.T) {
	fmt := "xz"
	resp := &rxtypes.SamplesResponse{
		Path:              "/data/a.log.xz",
		Lines:             map[string]int64{"1": 0},
		BeforeContext:     0,
		AfterContext:      0,
		Samples:           map[string][]string{"1": {"hi"}},
		IsCompressed:      true,
		CompressionFormat: &fmt,
	}
	got := FormatSamplesCLI(resp, false, "")
	if !strings.Contains(got, "Compressed: xz") {
		t.Errorf("missing Compressed header: %q", got)
	}
}

// TestFormatSamplesCLI_OffsetMode exercises the byte-offset branch.
// Key is the offset string; the map value is the line number.
func TestFormatSamplesCLI_OffsetMode(t *testing.T) {
	resp := &rxtypes.SamplesResponse{
		Path:          "/x.log",
		Offsets:       map[string]int64{"1024": 42},
		BeforeContext: 0,
		AfterContext:  0,
		Samples:       map[string][]string{"1024": {"the line at offset 1024"}},
	}
	got := FormatSamplesCLI(resp, false, "")
	// In offset mode the header is path:<line>:<offset> — line comes
	// from the map value.
	if !strings.Contains(got, "=== /x.log:42:1024 ===") {
		t.Errorf("offset-mode header wrong: %q", got)
	}
}

// TestFormatSamplesCLI_BadRegex_FallsBack verifies an invalid regex
// triggers uncolored output (no crash).
func TestFormatSamplesCLI_BadRegex_FallsBack(t *testing.T) {
	resp := &rxtypes.SamplesResponse{
		Path:          "/x.log",
		Lines:         map[string]int64{"1": 0},
		BeforeContext: 0,
		AfterContext:  0,
		Samples:       map[string][]string{"1": {"line"}},
	}
	// "[" is an unterminated character class — re.compile will fail.
	got := FormatSamplesCLI(resp, true, "[invalid(")
	if !strings.Contains(got, "line") {
		t.Errorf("fallback content missing: %q", got)
	}
}

// TestFormatSamplesCLI_PythonGoldenFile verifies byte-for-byte parity
// with the Python reference output captured in testdata/.
//
// To regenerate: use the Python snippet in testdata/README.md (it
// instantiates a SamplesResponse and calls .to_cli).
func TestFormatSamplesCLI_PythonGoldenFile(t *testing.T) {
	cases := []struct {
		name   string
		golden string
		resp   *rxtypes.SamplesResponse
	}{
		{
			name:   "line_mode_no_color",
			golden: "testdata/samples-line-nocolor.golden.txt",
			resp: &rxtypes.SamplesResponse{
				Path:          "/var/log/app.log",
				Lines:         map[string]int64{"5": 100, "10": 200},
				BeforeContext: 1,
				AfterContext:  1,
				Samples: map[string][]string{
					"5":  {"line 4", "line 5", "line 6"},
					"10": {"line 9", "line 10", "line 11"},
				},
			},
		},
		{
			name:   "line_mode_color",
			golden: "testdata/samples-line-color.golden.txt",
			resp: &rxtypes.SamplesResponse{
				Path:          "/var/log/app.log",
				Lines:         map[string]int64{"5": 100},
				BeforeContext: 1,
				AfterContext:  1,
				Samples:       map[string][]string{"5": {"line 4", "line 5", "line 6"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			goldenPath := filepath.Join(tc.golden)
			b, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Skipf("skipping: missing golden file %s (%v)", goldenPath, err)
			}
			colorize := strings.Contains(tc.name, "color") &&
				!strings.Contains(tc.name, "no_color")
			got := FormatSamplesCLI(tc.resp, colorize, "")
			// Python's '\n'.join(...) output ends in exactly one '\n'
			// because the final '' entry in the list gets a preceding
			// separator. Our FormatSamplesCLI returns exactly that
			// shape too. Both strings END with "\n".
			want := string(b)
			if got != want {
				t.Errorf("golden mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
			}
		})
	}
}

// TestKeyLeadingInt exercises the prefix-int parser used by key sorting.
func TestKeyLeadingInt(t *testing.T) {
	cases := []struct {
		in string
		n  int
		ok bool
	}{
		{"100", 100, true},
		{"-5", -5, true},
		{"100-200", 100, true},
		{"abc", 0, false},
		{"", 0, false},
		{"-", 0, false},
	}
	for _, tc := range cases {
		n, ok := keyLeadingInt(tc.in)
		if n != tc.n || ok != tc.ok {
			t.Errorf("keyLeadingInt(%q) = (%d, %v), want (%d, %v)", tc.in, n, ok, tc.n, tc.ok)
		}
	}
}

// TestKeyLess verifies numeric keys sort numerically.
func TestKeyLess(t *testing.T) {
	keys := []string{"100", "5", "20", "3-10", "1", "50"}
	insertionSortKeys(keys)
	want := []string{"1", "3-10", "5", "20", "50", "100"}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("sorted[%d]: got %q, want %q (full: %v)", i, keys[i], want[i], keys)
			break
		}
	}
}
