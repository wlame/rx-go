// Package cli implements the cobra-based CLI commands for rx.
//
// This file provides shared output formatting helpers used by trace, samples,
// and other commands. It handles ANSI color output, NO_COLOR env var support,
// match highlighting, and newline symbol substitution.
package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ANSI color codes for terminal output.
const (
	ansiReset     = "\033[0m"
	ansiBoldRed   = "\033[1;31m"
	ansiCyan      = "\033[36m"
	ansiGreen     = "\033[32m"
	ansiBrightBlk = "\033[90m"
	ansiMagenta   = "\033[35m"
	ansiYellow    = "\033[33m"
)

// shouldColorize returns true when ANSI color codes should be included in output.
// Color is disabled when --no-color is set OR the NO_COLOR env var is set (per https://no-color.org/).
func shouldColorize(noColorFlag bool) bool {
	if noColorFlag {
		return false
	}
	// NO_COLOR — any non-empty value means "disable color".
	if v := os.Getenv("NO_COLOR"); v != "" {
		return false
	}
	return true
}

// colorize wraps text with ANSI color code if colorOn is true.
func colorize(text, code string, colorOn bool) string {
	if !colorOn {
		return text
	}
	return code + text + ansiReset
}

// highlightPattern highlights regex pattern matches in a line of text using bold red.
// If the pattern is invalid or colorOn is false, returns the original line unchanged.
func highlightPattern(line, pattern string, colorOn bool) string {
	if !colorOn || pattern == "" {
		return line
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return line
	}
	return re.ReplaceAllStringFunc(line, func(match string) string {
		return ansiBoldRed + match + ansiReset
	})
}

// formatFilePath formats a file path for terminal display (cyan when colored).
func formatFilePath(path string, colorOn bool) string {
	return colorize(path, ansiCyan, colorOn)
}

// formatLineNumber formats a line number for terminal display (green when colored).
func formatLineNumber(lineNum int, colorOn bool) string {
	return colorize(fmt.Sprintf("%d", lineNum), ansiGreen, colorOn)
}

// formatContextHeader builds the context block header:
//
//	=== file:line:offset [pattern] ===
func formatContextHeader(filePath string, lineNum int, offset int, pattern string, colorOn bool) string {
	if colorOn {
		return colorize("=== ", ansiBrightBlk, true) +
			colorize(filePath, ansiCyan, true) +
			colorize(":", ansiBrightBlk, true) +
			colorize(fmt.Sprintf("%d", lineNum), ansiYellow, true) +
			colorize(":", ansiBrightBlk, true) +
			fmt.Sprintf("%d", offset) +
			" " +
			colorize("[", ansiBrightBlk, true) +
			colorize(pattern, ansiMagenta, true) +
			colorize("]", ansiBrightBlk, true) +
			" " +
			colorize("===", ansiBrightBlk, true)
	}
	return fmt.Sprintf("=== %s:%d:%d [%s] ===", filePath, lineNum, offset, pattern)
}

// processNewlineSymbol replaces the configured newline symbol in text.
// The NEWLINE_SYMBOL env var (with \n/\r escape processing) is applied
// to output lines so users can see where newlines fall in multi-line matches.
func processNewlineSymbol(text, newlineSymbol string) string {
	if newlineSymbol == "\n" || newlineSymbol == "" {
		// Default symbol — no substitution needed, newlines render normally.
		return text
	}
	return strings.ReplaceAll(text, "\n", newlineSymbol)
}

// formatMatchLine formats a single match for trace CLI output:
//
//	file_path:line_number:line_text
func formatMatchLine(filePath string, lineNum int, lineText, pattern string, colorOn bool) string {
	pathPart := formatFilePath(filePath, colorOn)
	linePart := formatLineNumber(lineNum, colorOn)
	sep := colorize(":", ansiBrightBlk, colorOn)
	textPart := highlightPattern(lineText, pattern, colorOn)
	return pathPart + sep + linePart + sep + textPart
}
