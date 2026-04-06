// context.go provides sample/context extraction around byte offsets and line numbers.
//
// GetContext reads lines surrounding a byte offset using os.File.ReadAt for efficient
// random access (no reading the entire file). GetContextByLines does the same for a
// 1-based line number. Both functions handle edge cases at file boundaries gracefully.
//
// When a *models.FileIndex is provided, GetContextByLines and GetLineRange use binary
// search over the index's checkpoints (~1 MB apart) to seek near the target line,
// avoiding a full scan from byte 0 on multi-GB files.
package fileutil

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wlame/rx/internal/models"
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
// When idx is non-nil, the function binary-searches the index's checkpoints to seek
// near the target line instead of scanning from byte 0. This turns O(file_size) into
// O(distance_from_checkpoint) for multi-GB files with ~1 MB checkpoint spacing.
// Pass nil for idx to fall back to a full sequential scan.
func GetContextByLines(path string, lineNumber, before, after int, idx ...*models.FileIndex) ([]string, error) {
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

	// Determine start position: use index checkpoint if available.
	startLine := 1
	startOffset := int64(0)
	// The earliest line we need is (lineNumber - before), but at least 1.
	earliestNeeded := lineNumber - before
	if earliestNeeded < 1 {
		earliestNeeded = 1
	}

	if len(idx) > 0 && idx[0] != nil && len(idx[0].LineIndex) > 1 {
		cpLine, cpOffset := findLineCheckpoint(idx[0].LineIndex, earliestNeeded)
		startLine = cpLine
		startOffset = int64(cpOffset)
	}

	if _, err := f.Seek(startOffset, 0); err != nil {
		return nil, fmt.Errorf("seek %s: %w", path, err)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// Sliding window for "before" context.
	windowSize := before + 1 // target line + lines before it
	window := make([]string, 0, windowSize)

	currentLine := startLine - 1 // will be incremented on first Scan()
	found := false
	var result []string

	for scanner.Scan() {
		currentLine++
		text := scanner.Text()

		if currentLine <= lineNumber {
			window = append(window, text)
			if len(window) > windowSize {
				window = window[1:]
			}
			if currentLine == lineNumber {
				found = true
				result = append(result, window...)
			}
		} else if found {
			if currentLine > lineNumber+after {
				break
			}
			result = append(result, text)
		}
	}

	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("scan %s: %w", path, err)
	}

	if !found {
		return nil, fmt.Errorf("line %d is out of bounds, file has %d lines", lineNumber, currentLine)
	}

	return result, nil
}

// GetLineRange reads exact lines from startLine to endLine (1-based, inclusive).
// Returns the lines without applying context.
//
// When idx is non-nil, binary-searches the index's checkpoints to seek near startLine
// instead of scanning from byte 0. Pass nil for idx to fall back to a full scan.
func GetLineRange(path string, startLine, endLine int, idx ...*models.FileIndex) ([]string, error) {
	if startLine < 1 {
		return nil, fmt.Errorf("startLine must be >= 1, got %d", startLine)
	}
	if endLine < startLine {
		return nil, fmt.Errorf("endLine (%d) must be >= startLine (%d)", endLine, startLine)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Use index checkpoint to skip ahead when available.
	seekLine := 1
	seekOffset := int64(0)
	if len(idx) > 0 && idx[0] != nil && len(idx[0].LineIndex) > 1 {
		cpLine, cpOffset := findLineCheckpoint(idx[0].LineIndex, startLine)
		seekLine = cpLine
		seekOffset = int64(cpOffset)
	}

	if _, err := f.Seek(seekOffset, 0); err != nil {
		return nil, fmt.Errorf("seek %s: %w", path, err)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var result []string
	currentLine := seekLine - 1
	for scanner.Scan() {
		currentLine++
		if currentLine < startLine {
			continue
		}
		if currentLine > endLine {
			break
		}
		result = append(result, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("scan %s: %w", path, err)
	}
	return result, nil
}

// GetByteRange finds the lines covering a byte range [startByte, endByte) and returns them.
// This is the offset-range equivalent of GetLineRange — given a byte range, it returns the
// full lines that overlap with that range.
func GetByteRange(path string, startByte, endByte int64) ([]string, error) {
	if startByte < 0 {
		return nil, fmt.Errorf("startByte must be >= 0, got %d", startByte)
	}
	if endByte < startByte {
		return nil, fmt.Errorf("endByte (%d) must be >= startByte (%d)", endByte, startByte)
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
	if startByte >= fileSize {
		return nil, nil
	}
	if endByte > fileSize {
		endByte = fileSize
	}

	// Expand the read window to include full lines: back up from startByte to
	// find the previous \n, and read past endByte to find the next \n.
	readStart := startByte
	if readStart > 0 {
		// Read backwards to find previous newline (up to 64KB back).
		backtrack := int64(64 * 1024)
		if backtrack > readStart {
			backtrack = readStart
		}
		buf := make([]byte, backtrack)
		n, _ := f.ReadAt(buf, readStart-backtrack)
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				readStart = readStart - backtrack + int64(i) + 1
				break
			}
		}
		if readStart > startByte {
			readStart = startByte // fallback
		}
	}

	readEnd := endByte
	// Read forward to find next newline (up to 64KB ahead).
	if readEnd < fileSize {
		buf := make([]byte, 64*1024)
		n, _ := f.ReadAt(buf, readEnd)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				readEnd = readEnd + int64(i) + 1
				break
			}
		}
	}
	if readEnd > fileSize {
		readEnd = fileSize
	}

	// Read only the relevant portion using ReadAt.
	chunk := make([]byte, readEnd-readStart)
	n, _ := f.ReadAt(chunk, readStart)
	chunk = chunk[:n]

	// Split into lines and return those overlapping [startByte, endByte).
	var result []string
	pos := readStart
	for _, line := range splitLinesFromBytes(chunk) {
		lineLen := int64(len(line))
		lineStart := pos
		lineEnd := pos + lineLen

		if lineStart < endByte && lineEnd > startByte {
			result = append(result, strings.TrimRight(line, "\n\r"))
		}
		if lineStart >= endByte {
			break
		}
		pos = lineEnd
	}
	return result, nil
}

// CountLines returns the number of lines in a file. A trailing newline does not
// add an extra empty line (matching Python's splitlines behavior).
func CountLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Count newlines by streaming with a large buffer — never loads the whole file.
	count := 0
	buf := make([]byte, 256*1024)
	for {
		n, err := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				count++
			}
		}
		if err != nil {
			break
		}
	}
	// If the file doesn't end with \n, the last line still counts.
	// Check by seeking to the last byte.
	if count > 0 {
		return count, nil
	}
	// Empty file or single line without newline.
	info, _ := f.Stat()
	if info != nil && info.Size() > 0 {
		return 1, nil
	}
	return 0, nil
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

// findLineCheckpoint binary-searches the line index for the checkpoint at or before
// targetLine. Each entry is [line_number, byte_offset, ...]. Returns (line, offset)
// of the best checkpoint, defaulting to (1, 0) when the index is empty.
func findLineCheckpoint(lineIndex [][]int, targetLine int) (line, offset int) {
	if len(lineIndex) == 0 {
		return 1, 0
	}
	// sort.Search returns the first index where lineIndex[i][0] > targetLine.
	// We want the entry just before that.
	i := sort.Search(len(lineIndex), func(i int) bool {
		return lineIndex[i][0] > targetLine
	})
	if i > 0 {
		i--
	}
	entry := lineIndex[i]
	if len(entry) < 2 {
		return 1, 0
	}
	return entry[0], entry[1]
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
