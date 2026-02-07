package trace

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/wlame/rx-go/pkg/models"
)

// ContextExtractor extracts context lines around matches
type ContextExtractor struct{}

// NewContextExtractor creates a new context extractor
func NewContextExtractor() *ContextExtractor {
	return &ContextExtractor{}
}

// ExtractContext extracts context lines for matches in a file
// Returns a map of match offset → context lines
func (e *ContextExtractor) ExtractContext(
	filePath string,
	matches []models.Match,
	beforeContext, afterContext int,
	index *models.FileIndex,
) (map[string][]models.ContextLine, error) {
	if beforeContext == 0 && afterContext == 0 {
		return make(map[string][]models.ContextLine), nil
	}

	// Open file
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Group matches by offset for efficient extraction
	matchOffsets := make(map[int64]bool)
	for _, match := range matches {
		matchOffsets[match.Offset] = true
	}

	// Extract context using index if available, otherwise byte-by-byte
	if index != nil && len(index.LineIndex) > 0 {
		return e.extractWithIndex(file, matches, beforeContext, afterContext, index)
	}

	return e.extractByteOffsetBased(file, matches, beforeContext, afterContext)
}

// extractWithIndex uses line index for efficient context extraction
func (e *ContextExtractor) extractWithIndex(
	file *os.File,
	matches []models.Match,
	beforeContext, afterContext int,
	index *models.FileIndex,
) (map[string][]models.ContextLine, error) {
	result := make(map[string][]models.ContextLine)

	for _, match := range matches {
		// Find line number for this match
		lineNum := match.AbsoluteLineNumber
		if lineNum <= 0 {
			// Try to calculate from offset
			lineNum = findLineNumberFromIndex(index.LineIndex, match.Offset)
		}

		if lineNum <= 0 {
			continue // Skip if we can't determine line number
		}

		// Calculate context range
		startLine := lineNum - beforeContext
		if startLine < 1 {
			startLine = 1
		}
		endLine := lineNum + afterContext

		// Extract lines in range
		contextLines, err := e.extractLineRange(file, startLine, endLine, index)
		if err != nil {
			continue // Skip on error
		}

		key := fmt.Sprintf("%d", match.Offset)
		result[key] = contextLines
	}

	return result, nil
}

// extractLineRange extracts lines from startLine to endLine (inclusive)
func (e *ContextExtractor) extractLineRange(
	file *os.File,
	startLine, endLine int,
	index *models.FileIndex,
) ([]models.ContextLine, error) {
	// Find byte offset for start line
	startOffset := findByteOffsetFromIndex(index.LineIndex, startLine)
	if startOffset < 0 {
		return nil, fmt.Errorf("start line %d not found in index", startLine)
	}

	// Seek to start position
	if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek to offset %d: %w", startOffset, err)
	}

	// Read lines
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024*10)

	lines := make([]models.ContextLine, 0, endLine-startLine+1)
	currentLine := startLine
	currentOffset := startOffset

	for scanner.Scan() && currentLine <= endLine {
		text := scanner.Text()

		lines = append(lines, models.ContextLine{
			LineNumber: currentLine,
			ByteOffset: currentOffset,
			Text:       text,
		})

		currentLine++
		currentOffset += int64(len(text)) + 1 // +1 for newline
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %w", err)
	}

	return lines, nil
}

// extractByteOffsetBased extracts context based on byte offsets (no index)
// This is less efficient but works without an index
func (e *ContextExtractor) extractByteOffsetBased(
	file *os.File,
	matches []models.Match,
	beforeContext, afterContext int,
) (map[string][]models.ContextLine, error) {
	result := make(map[string][]models.ContextLine)

	// For each match, read surrounding lines
	for _, match := range matches {
		contextLines, err := e.extractContextAroundOffset(
			file,
			match.Offset,
			beforeContext,
			afterContext,
		)
		if err != nil {
			continue // Skip on error
		}

		key := fmt.Sprintf("%d", match.Offset)
		result[key] = contextLines
	}

	return result, nil
}

// extractContextAroundOffset extracts context lines around a byte offset
func (e *ContextExtractor) extractContextAroundOffset(
	file *os.File,
	offset int64,
	beforeLines, afterLines int,
) ([]models.ContextLine, error) {
	// Strategy: scan backwards to find start of context, then scan forward

	// First, find the start of the line containing the match
	lineStart, err := e.findLineStart(file, offset)
	if err != nil {
		return nil, err
	}

	// Scan backwards to find beforeContext lines
	startOffset := lineStart
	if beforeLines > 0 {
		startOffset, err = e.scanBackwards(file, lineStart, beforeLines)
		if err != nil {
			startOffset = 0 // Fall back to file start
		}
	}

	// Seek to start position
	if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek to offset %d: %w", startOffset, err)
	}

	// Read lines forward
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024*10)

	lines := make([]models.ContextLine, 0, beforeLines+afterLines+1)
	currentOffset := startOffset
	linesRead := 0
	maxLines := beforeLines + afterLines + 1

	for scanner.Scan() && linesRead < maxLines {
		text := scanner.Text()

		lines = append(lines, models.ContextLine{
			LineNumber: -1, // Unknown without index
			ByteOffset: currentOffset,
			Text:       text,
		})

		currentOffset += int64(len(text)) + 1
		linesRead++
	}

	return lines, nil
}

// findLineStart finds the start of the line containing the given offset
func (e *ContextExtractor) findLineStart(file *os.File, offset int64) (int64, error) {
	if offset == 0 {
		return 0, nil
	}

	// Scan backwards to find previous newline
	const bufSize = 4096
	buf := make([]byte, bufSize)

	pos := offset
	for pos > 0 {
		readSize := bufSize
		if pos < bufSize {
			readSize = int(pos)
		}

		readPos := pos - int64(readSize)
		if _, err := file.Seek(readPos, io.SeekStart); err != nil {
			return 0, err
		}

		n, err := file.Read(buf[:readSize])
		if err != nil && err != io.EOF {
			return 0, err
		}

		// Scan backwards in buffer for newline
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return readPos + int64(i) + 1, nil
			}
		}

		pos = readPos
	}

	return 0, nil // Start of file
}

// scanBackwards scans backwards to find the start of the Nth previous line
func (e *ContextExtractor) scanBackwards(file *os.File, offset int64, numLines int) (int64, error) {
	const bufSize = 4096
	buf := make([]byte, bufSize)

	pos := offset
	linesFound := 0

	for pos > 0 && linesFound < numLines {
		readSize := bufSize
		if pos < bufSize {
			readSize = int(pos)
		}

		readPos := pos - int64(readSize)
		if _, err := file.Seek(readPos, io.SeekStart); err != nil {
			return 0, err
		}

		n, err := file.Read(buf[:readSize])
		if err != nil && err != io.EOF {
			return 0, err
		}

		// Scan backwards for newlines
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				linesFound++
				if linesFound == numLines {
					return readPos + int64(i) + 1, nil
				}
			}
		}

		pos = readPos
	}

	return 0, nil // Not enough lines before, return file start
}

// Helper functions for index-based lookups

func findLineNumberFromIndex(lineIndex [][]int64, offset int64) int {
	if len(lineIndex) == 0 {
		return -1
	}

	// Binary search for largest offset <= target
	left, right := 0, len(lineIndex)-1
	result := -1

	for left <= right {
		mid := (left + right) / 2
		if lineIndex[mid][1] <= offset {
			result = int(lineIndex[mid][0])
			left = mid + 1
		} else {
			right = mid - 1
		}
	}

	return result
}

func findByteOffsetFromIndex(lineIndex [][]int64, lineNumber int) int64 {
	if len(lineIndex) == 0 {
		return -1
	}

	targetLine := int64(lineNumber)

	// Binary search for largest line <= target
	left, right := 0, len(lineIndex)-1
	result := int64(-1)

	for left <= right {
		mid := (left + right) / 2
		if lineIndex[mid][0] <= targetLine {
			result = lineIndex[mid][1]
			left = mid + 1
		} else {
			right = mid - 1
		}
	}

	return result
}
