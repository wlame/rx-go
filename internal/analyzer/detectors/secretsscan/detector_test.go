package secretsscan

// Unit and end-to-end tests for the secrets-scan detector.
//
// Mirrors the layout used by the other detectors in this tree:
//
//  1. Unit: drive a fresh Detector through a real Coordinator with
//     in-memory or fixture-loaded line input. Tests verify the
//     combined regex matches each family of secret shape and ignores
//     obvious lookalikes.
//
//  2. End-to-end: one test drives the detector through index.Build
//     with Analyze=true on a real fixture so the full coordinator +
//     flush-context path is exercised. Asserts the anomaly surfaces on
//     UnifiedFileIndex.Anomalies with Detector == detectorName.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/index"
)

// rawLine is one line's bytes plus its absolute byte-offset span in the
// source. Offsets follow the coordinator's end-exclusive convention —
// end - start includes the trailing newline terminator.
type rawLine struct {
	bytes      []byte
	start, end int64
}

// feedFixture reads testdata/<name>, splits into lines preserving byte
// offsets, and drives a fresh Detector through a real Coordinator.
// Returns the anomalies emitted by the detector directly.
func feedFixture(t *testing.T, name string) ([]analyzer.Anomaly, []rawLine) {
	t.Helper()

	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	lines := splitLinesPreserveOffsets(data)
	return feedLines(t, lines), lines
}

// feedLines runs a fresh Detector through a real Coordinator with the
// given line list. Factored so fixture-based and synthetic-input tests
// share the same driver.
func feedLines(t *testing.T, lines []rawLine) []analyzer.Anomaly {
	t.Helper()

	d := New()
	// Window size of 128 matches the default elsewhere; secrets-scan is
	// stateless per-line so the size only affects coordinator bookkeeping.
	coord := analyzer.NewCoordinator(128, []analyzer.LineDetector{d})
	for i, l := range lines {
		coord.ProcessLine(int64(i+1), l.start, l.end, l.bytes)
	}
	return d.Finalize(nil)
}

// splitLinesPreserveOffsets splits data on '\n' and returns each line
// WITHOUT the trailing newline, along with the byte offset range of the
// line in the original data. Offsets include the terminator so the
// end-exclusive convention lines up with how the index builder feeds
// the coordinator.
func splitLinesPreserveOffsets(data []byte) []rawLine {
	out := make([]rawLine, 0, bytes.Count(data, []byte{'\n'})+1)
	var off int64
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			// Final line without a newline terminator.
			line := data
			out = append(out, rawLine{
				bytes: line,
				start: off,
				end:   off + int64(len(line)),
			})
			return out
		}
		line := data[:i]
		out = append(out, rawLine{
			bytes: line,
			start: off,
			end:   off + int64(i+1),
		})
		off += int64(i + 1)
		data = data[i+1:]
	}
	return out
}

// linesFromStrings turns a []string of line contents (no trailing
// newlines) into []rawLine with offsets computed as if each line were
// terminated by a single '\n'. Handy for tests that don't need a
// fixture file on disk.
func linesFromStrings(ss []string) []rawLine {
	out := make([]rawLine, 0, len(ss))
	var off int64
	for _, s := range ss {
		start := off
		end := start + int64(len(s)) + 1 // +1 for the '\n' terminator
		out = append(out, rawLine{
			bytes: []byte(s),
			start: start,
			end:   end,
		})
		off = end
	}
	return out
}

// TestDetector_AwsKeyFixture confirms an AWS access-key-shaped string
// embedded in log prose fires a single anomaly whose byte range covers
// exactly the key, not the surrounding text.
func TestDetector_AwsKeyFixture(t *testing.T) {
	got, lines := feedFixture(t, "aws_key.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 3 || a.EndLine != 3 {
		t.Errorf("span: start=%d end=%d, want 3..3", a.StartLine, a.EndLine)
	}
	// The anomaly byte range must point at JUST the AWS key, not the
	// whole line. Extract the substring and confirm.
	lineIdx := a.StartLine - 1
	line := lines[lineIdx]
	relStart := a.StartOffset - line.start
	relEnd := a.EndOffset - line.start
	matched := string(line.bytes[relStart:relEnd])
	if matched != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("matched bytes = %q, want %q", matched, "AKIAIOSFODNN7EXAMPLE")
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if a.Category != detectorCategory {
		t.Errorf("category = %q, want %q", a.Category, detectorCategory)
	}
}

// TestDetector_GithubPatFixture confirms a GitHub PAT shape matches and
// the byte range spans only the token.
func TestDetector_GithubPatFixture(t *testing.T) {
	got, lines := feedFixture(t, "github_pat.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 2 {
		t.Errorf("span: start=%d end=%d, want 2..2", a.StartLine, a.EndLine)
	}
	line := lines[a.StartLine-1]
	matched := string(line.bytes[a.StartOffset-line.start : a.EndOffset-line.start])
	if !strings.HasPrefix(matched, "ghp_") {
		t.Errorf("matched bytes = %q, want prefix ghp_", matched)
	}
	if len(matched) != 4+36 {
		t.Errorf("matched length = %d, want 40 (ghp_ + 36 chars)", len(matched))
	}
}

// TestDetector_SlackTokenFixture covers the Slack bot-token variant.
func TestDetector_SlackTokenFixture(t *testing.T) {
	got, lines := feedFixture(t, "slack_token.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 2 {
		t.Errorf("span: start=%d end=%d, want 2..2", a.StartLine, a.EndLine)
	}
	line := lines[a.StartLine-1]
	matched := string(line.bytes[a.StartOffset-line.start : a.EndOffset-line.start])
	if !strings.HasPrefix(matched, "xoxb-") {
		t.Errorf("matched bytes = %q, want prefix xoxb-", matched)
	}
}

// TestDetector_JwtFixture covers the JWT-shaped string. JWTs are long
// and contain dots and underscores; the matched range should include
// the full three-segment token.
func TestDetector_JwtFixture(t *testing.T) {
	got, lines := feedFixture(t, "jwt.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 2 {
		t.Errorf("span: start=%d end=%d, want 2..2", a.StartLine, a.EndLine)
	}
	line := lines[a.StartLine-1]
	matched := string(line.bytes[a.StartOffset-line.start : a.EndOffset-line.start])
	if !strings.HasPrefix(matched, "eyJ") {
		t.Errorf("matched bytes = %q, want prefix eyJ", matched)
	}
	// Three dot-separated segments means exactly two dots inside the match.
	if n := strings.Count(matched, "."); n != 2 {
		t.Errorf("matched %q has %d dots, want 2", matched, n)
	}
}

// TestDetector_PemKeyFixture covers the PEM BEGIN banner. The match
// must span the whole banner line's banner substring.
func TestDetector_PemKeyFixture(t *testing.T) {
	got, lines := feedFixture(t, "pem_key.log")
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 2 || a.EndLine != 2 {
		t.Errorf("span: start=%d end=%d, want 2..2", a.StartLine, a.EndLine)
	}
	line := lines[a.StartLine-1]
	matched := string(line.bytes[a.StartOffset-line.start : a.EndOffset-line.start])
	if matched != "-----BEGIN RSA PRIVATE KEY-----" {
		t.Errorf("matched bytes = %q, want PEM BEGIN banner", matched)
	}
}

// TestDetector_NegativesFixture covers lookalikes that must NOT fire:
// AKIA too short, broken JWT missing a segment, PEM with missing BEGIN
// marker, short GitHub PAT, missing-dash Slack prefix.
//
// If this test starts failing the combined regex has grown sloppy — the
// false positives it would produce are cheap to dismiss individually
// but damaging in aggregate because they train users to ignore the
// detector.
func TestDetector_NegativesFixture(t *testing.T) {
	got, _ := feedFixture(t, "negatives.log")
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (all lookalikes); %+v", len(got), got)
	}
}

// TestDetector_MultipleOnOneLine confirms two separate secrets on the
// same line produce two separate anomalies with disjoint byte ranges.
// This is the case FindAllIndex handles for us; the test pins the
// contract so a refactor to FindIndex (which returns only the first
// match) fails loudly.
func TestDetector_MultipleOnOneLine(t *testing.T) {
	lines := linesFromStrings([]string{
		"ctx AKIAIOSFODNN7EXAMPLE and ghp_0123456789abcdefABCDEF0123456789wxyz together",
	})
	got := feedLines(t, lines)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	// Byte ranges must be disjoint and in left-to-right order. FindAllIndex
	// returns non-overlapping matches in source order.
	if got[0].EndOffset > got[1].StartOffset {
		t.Errorf("ranges overlap: first end=%d, second start=%d",
			got[0].EndOffset, got[1].StartOffset)
	}
	// Both must point at the same line.
	if got[0].StartLine != 1 || got[1].StartLine != 1 {
		t.Errorf("both matches should be on line 1; got %d and %d",
			got[0].StartLine, got[1].StartLine)
	}
}

// TestDetector_ByteRangeExcludesSurroundingText confirms the anomaly
// range is just the match, NOT the whole line. This is the core
// difference between secrets-scan and the multi-line region detectors
// (traceback-* etc.) and a regression here would make the UI highlight
// the wrong bytes.
func TestDetector_ByteRangeExcludesSurroundingText(t *testing.T) {
	lines := linesFromStrings([]string{
		"prefix-padding-here AKIAIOSFODNN7EXAMPLE trailing-padding",
	})
	got := feedLines(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	// Expected: match covers "AKIAIOSFODNN7EXAMPLE" which starts at
	// byte offset 20 (after "prefix-padding-here ") and is 20 bytes long.
	if a.StartOffset != 20 {
		t.Errorf("StartOffset = %d, want 20", a.StartOffset)
	}
	if a.EndOffset != 40 {
		t.Errorf("EndOffset = %d, want 40", a.EndOffset)
	}
}

// TestDetector_EmptyInputNoAnomalies covers the trivial zero-line case.
func TestDetector_EmptyInputNoAnomalies(t *testing.T) {
	got := feedLines(t, nil)
	if len(got) != 0 {
		t.Errorf("got %d anomalies on empty input, want 0", len(got))
	}
}

// TestDetector_SlackTokenVariants pins the five Slack prefix variants
// so a typo in the regex character class is caught immediately.
func TestDetector_SlackTokenVariants(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"bot", "xoxb-12345-67890-AbCdEfGh"},
		{"user-app", "xoxa-2-12345-67890-AbCdEfGh"},
		{"legacy-user", "xoxp-12345-67890-AbCdEfGh"},
		{"refresh", "xoxr-12345-67890-AbCdEfGh"},
		{"legacy", "xoxs-12345-67890-AbCdEfGh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := linesFromStrings([]string{"TOKEN=" + c.token})
			got := feedLines(t, lines)
			if len(got) != 1 {
				t.Fatalf("got %d anomalies for %s, want 1; %+v",
					len(got), c.name, got)
			}
		})
	}
}

// TestDetector_PemKeyTypes pins the optional key-type prefixes in the
// PEM banner so the non-capturing group doesn't accidentally exclude a
// supported type.
func TestDetector_PemKeyTypes(t *testing.T) {
	cases := []string{
		"-----BEGIN PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----",
		"-----BEGIN DSA PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			lines := linesFromStrings([]string{c})
			got := feedLines(t, lines)
			if len(got) != 1 {
				t.Fatalf("got %d anomalies for %s, want 1; %+v",
					len(got), c, got)
			}
		})
	}
}

// TestDetector_Metadata nails the plan-mandated metadata. If a constant
// drifts this test fails loudly so the change is deliberate.
func TestDetector_Metadata(t *testing.T) {
	d := New()
	cases := []struct {
		got, want string
		field     string
	}{
		{d.Name(), "secrets-scan", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "secrets", "Category"},
		{
			d.Description(),
			"Credential-shaped strings (AWS key, GitHub PAT, Slack token, JWT, PEM key)",
			"Description",
		},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	// Severity is a package constant used by OnLine; check it explicitly
	// so a mistaken edit is caught.
	if severity != 1.0 {
		t.Errorf("severity = %v, want 1.0", severity)
	}
}

// TestDetector_EndToEnd_ViaIndexBuild confirms the detector plugs into
// the real index.Build pipeline and its anomalies surface in the
// UnifiedFileIndex.Anomalies list with Detector == detectorName.
func TestDetector_EndToEnd_ViaIndexBuild(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "aws_key.log"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	idx, err := index.Build(path, index.BuildOptions{
		Analyze:   true,
		Detectors: []analyzer.LineDetector{New()},
	})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if idx.Anomalies == nil {
		t.Fatal("idx.Anomalies is nil; expected populated slice under Analyze=true")
	}

	anomalies := *idx.Anomalies
	if len(anomalies) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(anomalies), anomalies)
	}
	a := anomalies[0]
	if a.Detector != detectorName {
		t.Errorf("Detector = %q, want %q", a.Detector, detectorName)
	}
	if a.Category != detectorCategory {
		t.Errorf("Category = %q, want %q (semantic bucket)", a.Category, detectorCategory)
	}
	if a.StartLine != 3 || a.EndLine != 3 {
		t.Errorf("span: start=%d end=%d, want 3..3", a.StartLine, a.EndLine)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}
