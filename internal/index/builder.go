package index

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wlame/rx-go/pkg/models"
)

// Builder builds line→offset indexes for files
type Builder struct {
	stepBytes int64 // Sparse sampling: record every N bytes
}

// NewBuilder creates a new index builder
func NewBuilder(stepBytes int64) *Builder {
	if stepBytes <= 0 {
		stepBytes = 100 * 1024 * 1024 // Default: 100MB
	}
	return &Builder{
		stepBytes: stepBytes,
	}
}

// BuildIndex creates a line→offset index for the given file
func (b *Builder) BuildIndex(filePath string, analyze bool) (*models.FileIndex, error) {
	startTime := time.Now()

	// Get file info
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute path: %w", err)
	}

	// Open file
	file, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Build index
	lineIndex, lineEnding, totalLines, err := b.buildLineIndex(file)
	if err != nil {
		return nil, fmt.Errorf("failed to build line index: %w", err)
	}

	buildTime := time.Since(startTime).Seconds()

	// Create index structure
	index := &models.FileIndex{
		Version:          1,
		IndexType:        models.IndexTypeRegular,
		SourcePath:       absPath,
		SourceModifiedAt: fileInfo.ModTime().Format(time.RFC3339),
		SourceSizeBytes:  fileInfo.Size(),
		CreatedAt:        time.Now().Format(time.RFC3339),
		BuildTimeSeconds: &buildTime,
		IndexStepBytes:   &b.stepBytes,
		LineIndex:        lineIndex,
		TotalLines:       &totalLines,
	}

	// Optionally analyze file
	if analyze {
		file.Seek(0, io.SeekStart) // Reset to beginning
		analysis, err := b.analyzeFile(file, lineIndex, lineEnding)
		if err != nil {
			return nil, fmt.Errorf("failed to analyze file: %w", err)
		}
		index.Analysis = analysis
	}

	return index, nil
}

// buildLineIndex scans file and records line→offset mappings
func (b *Builder) buildLineIndex(r io.Reader) ([][]int64, models.LineEnding, int, error) {
	lineIndex := make([][]int64, 0, 1000)
	lineNumber := 1
	byteOffset := int64(0)
	nextRecordThreshold := int64(0)

	// Always record first line (line 1 at offset 0)
	lineIndex = append(lineIndex, []int64{int64(lineNumber), byteOffset})

	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024) // 64KB buffer
	scanner.Buffer(buf, 1024*1024*10) // Max 10MB line

	for scanner.Scan() {
		lineBytes := scanner.Bytes()
		lineLength := int64(len(lineBytes))

		// Scanner just read line N, move to next line
		byteOffset += lineLength + 1 // +1 for newline
		lineNumber++

		// Sparse recording: record every stepBytes
		if byteOffset >= nextRecordThreshold {
			lineIndex = append(lineIndex, []int64{int64(lineNumber), byteOffset})
			nextRecordThreshold = byteOffset + b.stepBytes
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, "", 0, fmt.Errorf("scanner error: %w", err)
	}

	// Scanner counts lines: each Scan() reads one line
	// Total lines = number of times Scan() returned true
	// But lineNumber is 1-indexed and gets incremented after each scan
	// So after last scan, lineNumber points to "next" line that doesn't exist
	// The actual total lines is lineNumber - 1
	totalLines := lineNumber - 1
	if totalLines < 1 {
		totalLines = 1 // Minimum 1 line for non-empty files
	}

	// Always record last line if not already recorded
	if len(lineIndex) > 0 && lineIndex[len(lineIndex)-1][0] != int64(totalLines) {
		lineIndex = append(lineIndex, []int64{int64(totalLines), byteOffset})
	}

	// Determine line ending (simplified - assume LF for now)
	lineEnding := models.LineEndingLF

	return lineIndex, lineEnding, totalLines, nil
}

// analyzeFile performs statistical analysis on the file
func (b *Builder) analyzeFile(r io.Reader, lineIndex [][]int64, lineEnding models.LineEnding) (*models.IndexAnalysis, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024*10)

	lineLengths := make([]int, 0, 10000)
	emptyLineCount := 0
	maxLength := 0
	maxLineNumber := 0
	maxByteOffset := int64(0)
	totalLength := 0
	lineNumber := 0
	byteOffset := int64(0)

	for scanner.Scan() {
		lineNumber++
		lineBytes := scanner.Bytes()
		length := len(lineBytes)

		lineLengths = append(lineLengths, length)
		totalLength += length

		if length == 0 {
			emptyLineCount++
		}

		if length > maxLength {
			maxLength = length
			maxLineNumber = lineNumber
			maxByteOffset = byteOffset
		}

		byteOffset += int64(length) + 1
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error during analysis: %w", err)
	}

	if len(lineLengths) == 0 {
		return &models.IndexAnalysis{
			LineCount:      0,
			EmptyLineCount: 0,
			LineEnding:     lineEnding,
		}, nil
	}

	// Calculate statistics
	avg := float64(totalLength) / float64(len(lineLengths))

	// Sort for percentiles
	sortedLengths := make([]int, len(lineLengths))
	copy(sortedLengths, lineLengths)
	sort.Ints(sortedLengths)

	median := float64(sortedLengths[len(sortedLengths)/2])
	p95 := float64(sortedLengths[int(float64(len(sortedLengths))*0.95)])
	p99 := float64(sortedLengths[int(float64(len(sortedLengths))*0.99)])

	// Standard deviation
	variance := 0.0
	for _, length := range lineLengths {
		diff := float64(length) - avg
		variance += diff * diff
	}
	stddev := math.Sqrt(variance / float64(len(lineLengths)))

	return &models.IndexAnalysis{
		LineCount:               len(lineLengths),
		EmptyLineCount:          emptyLineCount,
		LineLengthMax:           maxLength,
		LineLengthAvg:           avg,
		LineLengthMedian:        median,
		LineLengthP95:           p95,
		LineLengthP99:           p99,
		LineLengthStddev:        stddev,
		LineLengthMaxLineNumber: maxLineNumber,
		LineLengthMaxByteOffset: maxByteOffset,
		LineEnding:              lineEnding,
	}, nil
}

// FindLineNumber finds the line number for a given byte offset using binary search
func FindLineNumber(lineIndex [][]int64, offset int64) int {
	if len(lineIndex) == 0 {
		return -1
	}

	// Binary search for the largest offset <= target
	left, right := 0, len(lineIndex)-1
	result := 0

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

// FindByteOffset finds the byte offset for a given line number using binary search
func FindByteOffset(lineIndex [][]int64, lineNumber int) int64 {
	if len(lineIndex) == 0 {
		return -1
	}

	targetLine := int64(lineNumber)

	// Binary search for the largest line <= target
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
