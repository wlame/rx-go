// pattern.go implements two-phase pattern identification and pattern validation.
//
// When ripgrep searches with multiple -e patterns simultaneously, it reports matches
// but does not indicate WHICH pattern produced each match. RX solves this with a
// two-phase approach:
//
//   Phase 1 (during search): every match stores ALL pattern IDs — no per-match regex work.
//   Phase 2 (post-processing): IdentifyPatterns tests each pattern's regex against the
//   match's line text and submatches to determine which patterns actually matched.
//
// This avoids expensive regex compilation inside the hot search loop.
package engine

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/wlame/rx/internal/models"
)

// compiledPattern holds a pre-compiled regex alongside its ID and original string.
type compiledPattern struct {
	id      string
	pattern string
	re      *regexp.Regexp
}

// IdentifyPatterns resolves which pattern actually produced each match.
//
// For each match, it compiles every pattern as a Go regex and checks whether the
// pattern matches any of the match's submatches. If submatches are not available,
// it falls back to searching the full line text.
//
// A single rg match event can produce multiple result entries when multiple patterns
// match the same line. The returned slice may be larger than the input.
//
// patternIDs maps pattern ID (e.g. "p1") to the regex string.
// rgExtraArgs is checked for "-i" to enable case-insensitive matching.
func IdentifyPatterns(matches []models.Match, patternIDs map[string]string, rgExtraArgs []string) []models.Match {
	if len(matches) == 0 || len(patternIDs) == 0 {
		return matches
	}

	// Check if case-insensitive mode is requested via rg extra args.
	caseInsensitive := false
	for _, arg := range rgExtraArgs {
		if arg == "-i" || arg == "--ignore-case" {
			caseInsensitive = true
			break
		}
	}

	// Pre-compile all patterns so we only do it once.
	compiled := make([]compiledPattern, 0, len(patternIDs))
	for id, pat := range patternIDs {
		regexStr := pat
		if caseInsensitive {
			regexStr = "(?i)" + regexStr
		}
		re, err := regexp.Compile(regexStr)
		if err != nil {
			// Skip patterns that don't compile in Go's regex engine.
			// (rg uses a different regex engine, so some patterns may not be compatible.)
			continue
		}
		compiled = append(compiled, compiledPattern{id: id, pattern: pat, re: re})
	}

	var result []models.Match

	for _, m := range matches {
		identified := identifyMatchPatterns(m, compiled)
		if len(identified) == 0 {
			// No pattern matched — keep the original match with empty pattern.
			// This can happen if the Go regex engine doesn't support the pattern.
			result = append(result, m)
			continue
		}

		// Create one match entry per identified pattern.
		for _, pid := range identified {
			dup := m
			dup.Pattern = pid
			result = append(result, dup)
		}
	}

	return result
}

// identifyMatchPatterns determines which compiled patterns match a single match entry.
// Returns a list of pattern IDs that matched.
func identifyMatchPatterns(m models.Match, compiled []compiledPattern) []string {
	lineText := ""
	if m.LineText != nil {
		lineText = *m.LineText
	}

	// If we have submatches, use the submatch text for more precise identification.
	if m.Submatches != nil && len(*m.Submatches) > 0 {
		matchedTexts := make(map[string]bool)
		for _, sm := range *m.Submatches {
			matchedTexts[sm.Text] = true
		}

		var ids []string
		for _, cp := range compiled {
			// Find all matches of this pattern in the line text.
			patternMatches := cp.re.FindAllString(lineText, -1)
			for _, pm := range patternMatches {
				if matchedTexts[pm] {
					ids = append(ids, cp.id)
					break
				}
			}
		}
		return ids
	}

	// Fallback: no submatches available (e.g. cache reconstruction).
	// Test each pattern against the full line text.
	var ids []string
	for _, cp := range compiled {
		if cp.re.MatchString(lineText) {
			ids = append(ids, cp.id)
		}
	}
	return ids
}

// ValidatePattern checks whether a regex pattern is valid for ripgrep's engine.
//
// It runs rg with empty input and the pattern. If rg exits with code 2, the pattern
// has a syntax error. The error message from rg's stderr is returned.
func ValidatePattern(pattern string) error {
	// Use a short timeout to avoid hanging on pathological patterns.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rg", "--quiet", "--", pattern)
	cmd.Stdin = strings.NewReader("")

	output, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 2 {
				msg := strings.TrimSpace(string(output))
				return fmt.Errorf("invalid regex pattern: %s", msg)
			}
			// Exit code 1 = no matches on empty input, which is expected.
			return nil
		}
		// Context timeout or other error.
		return fmt.Errorf("pattern validation failed: %w", err)
	}
	return nil
}
