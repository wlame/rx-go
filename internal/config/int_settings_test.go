package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// captureWarnings sends slog's default logger to a buffer for the rest
// of the test and returns the buffer.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// Every integer setting follows one rule: unset keeps the default; a
// value that is not a whole number or is below the minimum keeps the
// default; a value above the maximum is used as the maximum; anything
// in range is used as it is.
func TestIntSettings_EveryValueFollowsOneRule(t *testing.T) {
	for _, s := range IntSettings {
		cases := []struct {
			value string
			want  int
		}{
			{"", s.Default},
			{strconv.Itoa(s.Min), s.Min},
			{strconv.Itoa(s.Max), s.Max},
			{strconv.Itoa(s.Min - 1), s.Default},
			{"-1", s.Default},
			{strconv.Itoa(s.Max + 1), s.Max},
			{"999999999999999999999", s.Default},
			{"lots", s.Default},
			{"1.5", s.Default},
			{" 2", s.Default},
			{"2MB", s.Default},
		}
		if s.Min <= 0 {
			cases = append(cases, struct {
				value string
				want  int
			}{"0", 0})
		} else {
			cases = append(cases, struct {
				value string
				want  int
			}{"0", s.Default})
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s=%q", s.Name, tc.value), func(t *testing.T) {
				captureWarnings(t)
				t.Setenv(s.Name, tc.value)
				if got := s.Value(); got != tc.want {
					t.Errorf("%s=%q gives %d, want %d", s.Name, tc.value, got, tc.want)
				}
			})
		}
	}
}

// A value the rule does not accept as it is gets one warning per
// process, naming the variable, the value and what rx uses instead; an
// accepted value gets none.
func TestIntSettings_WarnOnceForAValueNotAccepted(t *testing.T) {
	cases := []struct {
		value     string
		wantWarns int
		wantUsing string
	}{
		{"0", 1, "using=50"},
		{"-7", 1, "using=50"},
		{"fifty", 1, "using=50"},
		{"5000000", 1, "using=1048576"},
		{"64", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			logged := captureWarnings(t)
			// The warning is remembered per process; a name the other
			// tests never set keeps this test independent of them.
			s := IntSetting{Name: "RX_TEST_WARN_ONCE_" + strings.ToUpper(tc.value), Default: 50, Min: 1, Max: 1 << 20}
			t.Setenv(s.Name, tc.value)

			for range 3 {
				s.Value()
			}

			if got := strings.Count(logged.String(), "invalid_setting"); got != tc.wantWarns {
				t.Fatalf("%d invalid_setting warnings, want %d:\n%s", got, tc.wantWarns, logged)
			}
			if tc.wantWarns == 0 {
				return
			}
			for _, want := range []string{"name=" + s.Name, "value=" + tc.value, "accepted=\"1 to 1048576\"", tc.wantUsing} {
				if !strings.Contains(logged.String(), want) {
					t.Errorf("warning lacks %q:\n%s", want, logged)
				}
			}
		})
	}
}

// Each getter reads its own setting, so a bad value of any variable
// ends at its default rather than at 0 or below.
func TestIntSettings_GettersReadTheirSetting(t *testing.T) {
	cases := []struct {
		setting IntSetting
		get     func() int
	}{
		{WorkersSetting, Workers},
		{MaxSubprocessesSetting, MaxSubprocesses},
		{MinChunkSizeMBSetting, MinChunkSizeMB},
		{LargeFileMBSetting, LargeFileMB},
		{MaxLineTextBytesSetting, MaxLineTextBytes},
		{MaxSubmatchesPerLineSetting, MaxSubmatchesPerLine},
		{AnalyzeWindowLinesSetting, AnalyzeWindowLines},
		{TaskTTLMinutesSetting, func() int { return int(TaskTTL() / time.Minute) }},
		{SamplesWaitSecondsSetting, func() int { return int(SamplesIndexWait() / time.Second) }},
		{TimestampLookbackKBSetting, func() int { return int(TimestampLookbackBytes() / 1024) }},
	}
	if len(cases) != len(IntSettings) {
		t.Fatalf("%d getters tested, %d settings declared", len(cases), len(IntSettings))
	}
	for _, tc := range cases {
		t.Run(tc.setting.Name, func(t *testing.T) {
			captureWarnings(t)
			t.Setenv(tc.setting.Name, "-1")
			if got := tc.get(); got != tc.setting.Default {
				t.Errorf("%s=-1 gives %d, want the default %d", tc.setting.Name, got, tc.setting.Default)
			}
			t.Setenv(tc.setting.Name, strconv.Itoa(tc.setting.Max))
			if got := tc.get(); got != tc.setting.Max {
				t.Errorf("%s=%d gives %d", tc.setting.Name, tc.setting.Max, got)
			}
		})
	}
}

// Each range fits the arithmetic its value goes into: a size in MB
// becomes bytes as an int64, a TTL in minutes becomes a time.Duration.
func TestIntSettings_RangesFitTheirArithmetic(t *testing.T) {
	for _, s := range IntSettings {
		if s.Min > s.Max {
			t.Errorf("%s: minimum %d above maximum %d", s.Name, s.Min, s.Max)
		}
		if s.Default != 0 && (s.Default < s.Min || s.Default > s.Max) {
			t.Errorf("%s: default %d outside %d to %d", s.Name, s.Default, s.Min, s.Max)
		}
	}
	for _, s := range []IntSetting{MinChunkSizeMBSetting, LargeFileMBSetting} {
		if bytes := int64(s.Max) << 20; bytes>>20 != int64(s.Max) {
			t.Errorf("%s: %d MB overflows int64 bytes", s.Name, s.Max)
		}
	}
	if d := time.Duration(TaskTTLMinutesSetting.Max) * time.Minute; d/time.Minute != time.Duration(TaskTTLMinutesSetting.Max) {
		t.Errorf("RX_TASK_TTL_MINUTES: %d minutes overflows time.Duration", TaskTTLMinutesSetting.Max)
	}
}

// docs/configuration.md lists every integer setting with its default
// and range; the rows must say what the table here says.
func TestIntSettings_DocumentedRangesMatch(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("read docs: %v", err)
	}
	for _, s := range IntSettings {
		row := fmt.Sprintf("| `%s` | %s | %d | %d |", s.Name, documentedDefault(s), s.Min, s.Max)
		if !bytes.Contains(doc, []byte(row)) {
			t.Errorf("docs/configuration.md lacks the row %s", row)
		}
	}
}

// documentedDefault is how the documentation's table shows a default.
func documentedDefault(s IntSetting) string {
	if s == WorkersSetting {
		return "unset"
	}
	return strconv.Itoa(s.Default)
}
