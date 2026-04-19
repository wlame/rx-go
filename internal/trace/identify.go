package trace

import (
	"regexp"
	"regexp/syntax"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// IdentifyMatchingPatterns is the 2-phase pattern-identification step.
//
// Context — why do we need this at all?
// ripgrep accepts multiple `-e PAT` patterns but its --json output
// doesn't report WHICH pattern matched a given line. When we give rg
// three regexps {p1: "foo", p2: "bar", p3: "baz"} and a line matches
// both "foo" and "bar", we need to know so the API response lists
// both. So: for each matched line, we re-run each pattern against the
// line text and the submatch set, and collect the patterns that
// reproduce the match.
//
// Algorithm (matches rx-python/src/rx/trace_worker.py::identify_matching_patterns):
//
//   - If submatches is empty (cache-reconstruction path), validate each
//     pattern against the line text directly; return all that match.
//   - Otherwise, compute the set of matched-text strings from submatches,
//     then for each pattern check if ANY of that pattern's findall
//     results intersect the submatch set. Patterns that do are kept.
//
// Flag handling: the Python version only cares about the `-i` case-
// insensitive flag (any other rg flag doesn't change which pattern
// matched a line). rx-go does the same.
//
// patternOrder carries the canonical pattern-ID order ("p1", "p2", ...).
// We iterate in this order so the returned slice is deterministic
// (Go map iteration is randomized).
//
// Returns an empty slice (never nil) if no patterns match — callers
// may treat this as a stale-cache hit and drop the match.
func IdentifyMatchingPatterns(
	lineText string,
	submatches []rxtypes.Submatch,
	patternIDs map[string]string,
	patternOrder []string,
	rgExtraArgs []string,
) []string {
	ignoreCase := hasFlag(rgExtraArgs, "-i", "--ignore-case")

	if len(submatches) == 0 {
		return identifyByFullLineMatch(lineText, patternIDs, patternOrder, ignoreCase)
	}

	// Build the set of matched text strings for intersection tests.
	matchedTexts := make(map[string]struct{}, len(submatches))
	for _, sm := range submatches {
		matchedTexts[sm.Text] = struct{}{}
	}

	matchingIDs := make([]string, 0, len(patternOrder))
	for _, pid := range patternOrder {
		patStr, ok := patternIDs[pid]
		if !ok {
			continue
		}
		re, err := compileRegex(patStr, ignoreCase)
		if err != nil {
			// Invalid regex — skip silently (Python does the same).
			continue
		}
		// findall against the line; any overlap with matchedTexts wins.
		for _, loc := range re.FindAllStringIndex(lineText, -1) {
			if _, inSet := matchedTexts[lineText[loc[0]:loc[1]]]; inSet {
				matchingIDs = append(matchingIDs, pid)
				break
			}
		}
	}
	return matchingIDs
}

// identifyByFullLineMatch is the submatch-less fallback path.
// Each pattern is tested against the whole line; every pattern that
// finds at least one match is returned.
func identifyByFullLineMatch(
	lineText string,
	patternIDs map[string]string,
	patternOrder []string,
	ignoreCase bool,
) []string {
	out := make([]string, 0, len(patternOrder))
	for _, pid := range patternOrder {
		patStr, ok := patternIDs[pid]
		if !ok {
			continue
		}
		re, err := compileRegex(patStr, ignoreCase)
		if err != nil {
			continue
		}
		if re.MatchString(lineText) {
			out = append(out, pid)
		}
	}
	return out
}

// compileRegex compiles a Python-style regex string into Go's RE2.
//
// Caveats (parity gotchas):
//   - Python's `re` accepts `(?i)` inline flags; so does Go's regexp.
//   - We prefix with `(?i)` when ignoreCase is true AND the pattern
//     doesn't already contain an inline flag directive. Appending via
//     re.Copy would be cleaner but Go's regexp lacks case-insensitive
//     runtime toggle — we must bake the flag at compile time.
//   - Python's `re` supports look-behind/look-ahead; Go RE2 does NOT.
//     We catch the compile error and return it; the caller skips.
//   - ripgrep itself uses Rust's regex crate, also RE2-style (no look-
//     behind). So any pattern that works in ripgrep (which rg already
//     validated upstream of us) will work here too.
func compileRegex(pattern string, ignoreCase bool) (*regexp.Regexp, error) {
	// Fast-parse the pattern to detect inline flags. If (?i) / (?s) /
	// etc. is already present at the head, don't prepend; otherwise
	// ignoreCase is applied with a (?i:...) non-capturing wrap.
	if ignoreCase && !hasInlineFlag(pattern, syntax.FoldCase) {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

// hasInlineFlag parses the pattern just far enough to see whether it
// already opts into the given syntax flag. Returns false on parse
// errors (the regex is broken either way; the caller surfaces that).
func hasInlineFlag(pattern string, flag syntax.Flags) bool {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return false
	}
	return parsed.Flags&flag != 0
}

// hasFlag checks whether any of the given alias strings appears in the
// ripgrep extra-args slice.
func hasFlag(args []string, aliases ...string) bool {
	for _, a := range args {
		for _, alias := range aliases {
			if a == alias {
				return true
			}
		}
	}
	return false
}
