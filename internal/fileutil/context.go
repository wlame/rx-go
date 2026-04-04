// context.go provides sample/context extraction around byte offsets and line numbers.
//
// GetContext reads lines surrounding a byte offset using os.File.ReadAt for efficient
// random access (no reading the entire file). GetContextByLines does the same for a
// 1-based line number. Both functions handle edge cases at file boundaries gracefully.
package fileutil

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// lineEstimateBytes is the assumed max average line length used to calculate how
// many bytes to read around a target offset. Matches RX_MAX_LINE_SIZE_KB default (8KB).
const lineEstimateBytes = 8 * 1024

// GetContext reads lines of context around a byte offset in a file.
// It returns the line containing the offset plus `before` lines above and `after` lines below.
// The returned lines have trailing newline/carriage-return characters stripped.
//
// Uses ReadAt for efficient random access — does not read the entire file.
func GetContext(path string, byteOffset int64, before, after int) ([]string, error) {
	if before < 0 || after < 0 {
		return nil, fmt.Errorf("context values must be non-negative")
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	fileSize := info.Size()

	if byteOffset < 0 || byteOffset >= fileSize {
		return nil, nil
	}

	// Calculate a read window large enough to capture the requested context lines.
	bytesBefore := int64(lineEstimateBytes) * int64(before+1)
	bytesAfter := int64(lineEstimateBytes) * int64(after+1)

	startPos := byteOffset - bytesBefore
	if startPos < 0 {
		startPos = 0
	}
	endPos := byteOffset + bytesAfter
	if endPos > fileSize {
		endPos = fileSize
	}
	readSize := endPos - startPos

	// Read the chunk using ReadAt — no seeking state, safe for concurrent use.
	buf := make([]byte, readSize)
	n, err := f.ReadAt(buf, startPos)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read %s at %d: %w", path, startPos, err)
	}
	buf = buf[:n]

	// Split on \n only (matching Python behavior — don't use strings.Split which
	// would lose information about whether lines end with \n).
	text := string(buf)
	lines := splitLines(text)

	// Find the line that contains the target offset.
	currentPos := startPos
	targetLineIdx := -1
	for i, line := range lines {
		lineBytes := int64(len(line))
		lineStart := currentPos
		lineEnd := currentPos + lineBytes

		if lineStart <= byteOffset && byteOffset < lineEnd {
			targetLineIdx = i
			break
		}
		currentPos = lineEnd
	}

	if targetLineIdx < 0 {
		return nil, nil
	}

	// Extract the context window.
	startIdx := targetLineIdx - before
	if startIdx < 0 {
		startIdx = 0
	}
	endIdx := targetLineIdx + after + 1
	if endIdx > len(lines) {
		endIdx = len(lines)
	}

	result := make([]string, 0, endIdx-startIdx)
	for _, line := range lines[startIdx:endIdx] {
		result = append(result, strings.TrimRight(line, "\n\r"))
	}
	return result, nil
}

// GetContextByLines reads lines of context around a 1-based line number.
// It returns the target line plus `before` lines above and `after` lines below.
// The returned lines have trailing newline/carriage-return characters stripped.
//
// For simplicity this reads the file sequentially from the start — adequate for
// files up to a few hundred MB. For very large files the caller should use an index
// to find the byte offset and call GetContext instead.
func GetContextByLines(path string, lineNumber, before, after int) ([]string, error) {
	if before < 0 || after < 0 {
		return nil, fmt.Errorf("context values must be non-negative")
	}
	if lineNumber < 1 {
		return nil, fmt.Errorf("line number must be >= 1, got %d", lineNumber)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Read the entire file into memory. For the use cases in this project
	// (context extraction with small before/after values), this is acceptable.
	// A streaming approach is possible but adds complexity for marginal gain
	// when context windows are small.
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Split on \n only — matching Python's behavior.
	allLines := splitLinesFromBytes(data)
	totalLines := len(allLines)

	if lineNumber > totalLines {
		return nil, fmt.Errorf("line %d is out of bounds, file has %d lines", lineNumber, totalLines)
	}

	// Convert to 0-based index.
	targetIdx := lineNumber - 1

	startIdx := targetIdx - before
	if startIdx < 0 {
		startIdx = 0
	}
	endIdx := targetIdx + after + 1
	if endIdx > totalLines {
		endIdx = totalLines
	}

	result := make([]string, 0, endIdx-startIdx)
	for _, line := range allLines[startIdx:endIdx] {
		result = append(result, strings.TrimRight(line, "\n\r"))
	}
	return result, nil
}

// splitLines splits text on \n boundaries, keeping the \n at the end of each line
// (except possibly the last line if the file doesn't end with \n).
// This matches the Python reference: text.split('\n') then re-adding '\n'.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	// Re-add \n to all lines except the last (which may or may not have had one).
	result := make([]string, 0, len(parts))
	for i := 0; i < len(parts)-1; i++ {
		result = append(result, parts[i]+"\n")
	}
	// Add the last part only if it's non-empty (file didn't end with \n).
	if parts[len(parts)-1] != "" {
		result = append(result, parts[len(parts)-1])
	}
	return result
}

// splitLinesFromBytes is like splitLines but works on raw bytes for efficiency.
// Returns lines with \n attached (except possibly the last).
func splitLinesFromBytes(data []byte) []string {
	if len(data) == 0 {
		return nil
	}

	var lines []string
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			// No more newlines — add remaining data as last line if non-empty.
			if len(data) > 0 {
				lines = append(lines, string(data))
			}
			break
		}
		// Include the \n in the line.
		lines = append(lines, string(data[:idx+1]))
		data = data[idx+1:]
	}
	return lines
}
