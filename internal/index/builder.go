package index

import (
	"bufio"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wlame/rx/internal/compression"
	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/models"
)

// DefaultIndexStepDivisor controls index density: stepBytes = largeFileThreshold / divisor.
// With 50 MB threshold and divisor 50, we get ~1 MB between checkpoints.
const DefaultIndexStepDivisor = 50

// LineSampleInterval is how often we record line index entries for compressed files.
// Every N-th line gets a [line_number, decompressed_offset] entry.
const LineSampleInterval = 1000

// SeekableLineSampleInterval is how often we record entries within large seekable zstd frames.
const SeekableLineSampleInterval = 10000

// BuildIndex auto-detects the file type and dispatches to the appropriate index builder.
// Returns a fully populated FileIndex ready for caching.
func BuildIndex(path string, cfg *config.Config) (*models.FileIndex, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute path %s: %w", path, err)
	}

	format, err := compression.Detect(absPath)
	if err != nil {
		// If detection fails, try building as a regular text file.
		slog.Debug("compression detection failed, treating as text", "path", absPath, "error", err)
		format = compression.FormatNone
	}

	switch format {
	case compression.FormatNone:
		return buildRegularIndex(absPath, cfg)
	case compression.FormatSeekableZstd:
		return buildSeekableZstdIndex(absPath)
	default:
		// Gzip, XZ, BZ2, non-seekable Zstd — all use the compressed stream builder.
		return buildCompressedIndex(absPath, format)
	}
}

// buildRegularIndex scans a plain text file, recording line-offset checkpoints at
// regular byte intervals and computing analysis statistics.
func buildRegularIndex(absPath string, cfg *config.Config) (*models.FileIndex, error) {
	startTime := time.Now()

	stat, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", absPath, err)
	}

	stepBytes := int64(cfg.LargeFileMB) * 1024 * 1024 / DefaultIndexStepDivisor
	if stepBytes <= 0 {
		stepBytes = 1024 * 1024 // fallback to 1 MB
	}

	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", absPath, err)
	}
	defer f.Close()

	// First line always starts at offset 0.
	lineIndex := [][]int{{1, 0}}
	var currentLine int = 1
	nextCheckpoint := stepBytes

	// Statistics accumulators.
	var lineLengths []int
	var emptyLineCount int
	var maxLineLength int
	var maxLineNumber int
	var maxLineOffset int64

	// Line ending detection sample from the first lines.
	var lineEndingSample []byte
	sampleCollected := false

	// Use bufio.Reader instead of Scanner to track exact byte offsets.
	// Scanner strips newlines, making offset tracking approximate.
	// Reader.ReadLine gives us the raw line content and we track offsets explicitly.
	reader := bufio.NewReaderSize(f, 1024*1024)
	var currentOffset int64

	for {
		// ReadLine returns the line content WITHOUT the trailing \n or \r\n.
		// isPrefix=true means the line was too long for the buffer — we assemble it.
		line, isPrefix, err := reader.ReadLine()
		if err != nil {
			break // EOF or error
		}

		// Assemble full line if it was split across buffer boundaries.
		var fullLine []byte
		rawLen := len(line) // bytes read so far (without delimiter)
		if isPrefix {
			fullLine = append(fullLine, line...)
			for isPrefix {
				line, isPrefix, err = reader.ReadLine()
				if err != nil {
					break
				}
				rawLen += len(line)
				fullLine = append(fullLine, line...)
			}
		} else {
			fullLine = line
		}

		lineLen := len(fullLine) // content bytes (no newline)

		// Collect sample for line ending detection.
		// We need to know whether the file uses LF or CRLF, so peek at the
		// byte just after the line content in the file.
		if !sampleCollected && currentOffset+int64(lineLen) < stat.Size() {
			// Read the delimiter byte(s) by checking the file at the known position.
			// The reader already consumed them, so we record what we know:
			// ReadLine strips \n and \r\n, so the actual bytes on disk are content + delimiter.
			// We'll detect CRLF by checking if the raw content ended with \r before stripping.
			lineEndingSample = append(lineEndingSample, fullLine...)
			if len(lineEndingSample) > 64*1024 {
				sampleCollected = true
			}
		}

		// Track statistics.
		stripped := len(trimWhitespace(fullLine))
		if stripped > 0 {
			lineLengths = append(lineLengths, lineLen)
			if lineLen > maxLineLength {
				maxLineLength = lineLen
				maxLineNumber = currentLine
				maxLineOffset = currentOffset
			}
		} else {
			emptyLineCount++
		}

		// Advance offset: content length + 1 for \n.
		// For CRLF files this would be +2, but we detect that below and correct.
		// For most log files (LF), +1 is exact.
		delimLen := 1
		currentOffset += int64(lineLen) + int64(delimLen)
		currentLine++

		// Record checkpoint at the START of the next line.
		if currentOffset >= nextCheckpoint {
			lineIndex = append(lineIndex, []int{currentLine, int(currentOffset)})
			nextCheckpoint = currentOffset + stepBytes
		}
	}

	// Adjust currentLine to be the count (it's now 1 past the last line).
	totalLines := currentLine - 1

	// Detect line endings by re-reading first 64 KB of the file.
	lineEnding := detectLineEnding(absPath)
	_ = lineEndingSample // ending detection done by detectLineEnding
	_ = sampleCollected

	// Compute statistics.
	analysis := computeAnalysis(lineLengths, emptyLineCount, totalLines,
		maxLineLength, maxLineNumber, int(maxLineOffset), lineEnding)

	buildTime := time.Since(startTime).Seconds()
	stepBytesInt := int(stepBytes)

	idx := models.NewFileIndex(
		UnifiedIndexVersion,
		models.IndexTypeRegular,
		absPath,
		stat.ModTime().Format(time.RFC3339Nano),
		int(stat.Size()),
	)
	idx.CreatedAt = time.Now().Format(time.RFC3339Nano)
	idx.BuildTimeSeconds = &buildTime
	idx.IndexStepBytes = &stepBytesInt
	idx.LineIndex = lineIndex
	idx.Analysis = analysis

	slog.Info("regular index built",
		"path", absPath,
		"lines", totalLines,
		"checkpoints", len(lineIndex),
		"build_time_s", fmt.Sprintf("%.3f", buildTime))

	return &idx, nil
}

// buildCompressedIndex streams a compressed file through native decompression,
// counting newlines in a single pass without materializing the full content.
func buildCompressedIndex(absPath string, format compression.CompressionFormat) (*models.FileIndex, error) {
	startTime := time.Now()

	stat, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", absPath, err)
	}

	reader, _, err := compression.NewReader(absPath)
	if err != nil {
		return nil, fmt.Errorf("open compressed reader %s: %w", absPath, err)
	}
	defer reader.Close()

	// Single-pass: read decompressed stream in chunks, count newlines.
	lineIndex := [][]int{{1, 0}}
	var currentLine int
	var currentOffset int64
	var partialLineLen int

	chunk := make([]byte, 64*1024) // 64 KB read buffer

	for {
		n, readErr := reader.Read(chunk)
		if n > 0 {
			data := chunk[:n]
			chunkPos := 0

			for chunkPos < len(data) {
				nlPos := indexOf(data[chunkPos:], '\n')
				if nlPos == -1 {
					// No more newlines in this chunk.
					partialLineLen += len(data) - chunkPos
					break
				}

				// Found a newline — complete this line.
				currentLine++
				partialLineLen = 0

				// First line checkpoint.
				if currentLine == 1 {
					// Already recorded at init.
				} else if currentLine%LineSampleInterval == 0 {
					newlineAbsOffset := currentOffset + int64(chunkPos+nlPos) + 1
					lineIndex = append(lineIndex, []int{currentLine, int(newlineAbsOffset)})
				}

				chunkPos += nlPos + 1
			}

			currentOffset += int64(n)
		}

		if readErr != nil {
			break
		}
	}

	// Handle final line without trailing newline.
	if partialLineLen > 0 {
		currentLine++
	}

	buildTime := time.Since(startTime).Seconds()
	sampleInterval := LineSampleInterval
	formatStr := format.String()
	decompSize := int(currentOffset)

	idx := models.NewFileIndex(
		UnifiedIndexVersion,
		models.IndexTypeCompressed,
		absPath,
		stat.ModTime().Format(time.RFC3339Nano),
		int(stat.Size()),
	)
	idx.CreatedAt = time.Now().Format(time.RFC3339Nano)
	idx.BuildTimeSeconds = &buildTime
	idx.LineIndex = lineIndex
	idx.CompressionFormat = &formatStr
	idx.DecompressedSizeBytes = &decompSize
	idx.TotalLines = &currentLine
	idx.LineSampleInterval = &sampleInterval

	slog.Info("compressed index built",
		"path", absPath,
		"format", formatStr,
		"lines", currentLine,
		"decompressed_bytes", currentOffset,
		"checkpoints", len(lineIndex),
		"build_time_s", fmt.Sprintf("%.3f", buildTime))

	return &idx, nil
}

// buildSeekableZstdIndex decompresses each frame to count lines and build
// the frame-to-line mapping required for parallel search offset adjustment.
func buildSeekableZstdIndex(absPath string) (*models.FileIndex, error) {
	startTime := time.Now()

	stat, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", absPath, err)
	}

	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", absPath, err)
	}
	defer f.Close()

	table, err := compression.ReadSeekTable(f)
	if err != nil {
		return nil, fmt.Errorf("read seek table %s: %w", absPath, err)
	}

	// Build frame line info by decompressing each frame.
	var frameInfos []models.FrameLineInfo
	var lineIndex [][]int
	currentLine := 1
	totalLines := 0

	for i, frame := range table.Frames {
		data, err := compression.DecompressFrame(f, frame)
		if err != nil {
			return nil, fmt.Errorf("decompress frame %d of %s: %w", i, absPath, err)
		}

		// Count newlines in the decompressed frame.
		linesInFrame := countNewlines(data)
		isLastFrame := i == len(table.Frames)-1
		if isLastFrame && len(data) > 0 && data[len(data)-1] != '\n' {
			linesInFrame++ // Partial line at end of file counts as a line.
		}

		firstLine := currentLine
		lastLine := currentLine + linesInFrame - 1
		if linesInFrame == 0 {
			lastLine = currentLine
		}

		frameInfos = append(frameInfos, models.FrameLineInfo{
			Index:              i,
			CompressedOffset:   int(frame.CompressedOffset),
			CompressedSize:     int(frame.CompressedSize),
			DecompressedOffset: int(frame.DecompressedOffset),
			DecompressedSize:   int(frame.DecompressedSize),
			FirstLine:          firstLine,
			LastLine:           lastLine,
			LineCount:          linesInFrame,
		})

		// Record line index entry for the first line of each frame (3-element format).
		lineIndex = append(lineIndex, []int{firstLine, int(frame.DecompressedOffset), i})

		// Add intermediate sampled entries for large frames.
		if linesInFrame > SeekableLineSampleInterval {
			byteOff := 0
			lineNum := firstLine
			for j := 0; j < len(data); j++ {
				if data[j] == '\n' {
					lineNum++
					if lineNum > firstLine && (lineNum-firstLine)%SeekableLineSampleInterval == 0 {
						decompOff := int(frame.DecompressedOffset) + j + 1
						lineIndex = append(lineIndex, []int{lineNum, decompOff, i})
					}
				}
			}
			_ = byteOff
		}

		currentLine = lastLine + 1
		totalLines += linesInFrame
	}

	buildTime := time.Since(startTime).Seconds()
	formatStr := "zstd"
	decompSize := int(table.TotalDecompressedSize())
	frameCount := len(table.Frames)
	frames := frameInfos

	idx := models.NewFileIndex(
		UnifiedIndexVersion,
		models.IndexTypeSeekableZstd,
		absPath,
		stat.ModTime().Format(time.RFC3339Nano),
		int(stat.Size()),
	)
	idx.CreatedAt = time.Now().Format(time.RFC3339Nano)
	idx.BuildTimeSeconds = &buildTime
	idx.LineIndex = lineIndex
	idx.CompressionFormat = &formatStr
	idx.DecompressedSizeBytes = &decompSize
	idx.TotalLines = &totalLines
	idx.FrameCount = &frameCount
	idx.Frames = &frames

	slog.Info("seekable zstd index built",
		"path", absPath,
		"frames", frameCount,
		"lines", totalLines,
		"checkpoints", len(lineIndex),
		"build_time_s", fmt.Sprintf("%.3f", buildTime))

	return &idx, nil
}

// computeAnalysis calculates line length statistics from the collected data.
func computeAnalysis(
	lineLengths []int,
	emptyLineCount, lineCount, maxLineLength, maxLineNumber, maxLineOffset int,
	lineEnding string,
) *models.IndexAnalysis {
	a := &models.IndexAnalysis{
		LineCount:            &lineCount,
		EmptyLineCount:       &emptyLineCount,
		LineLengthMax:        &maxLineLength,
		LineLengthMaxLineNum: &maxLineNumber,
		LineLengthMaxOffset:  &maxLineOffset,
		LineEnding:           &lineEnding,
	}

	if len(lineLengths) == 0 {
		zero := 0.0
		a.LineLengthAvg = &zero
		a.LineLengthMedian = &zero
		a.LineLengthP95 = &zero
		a.LineLengthP99 = &zero
		a.LineLengthStddev = &zero
		return a
	}

	// Sort for percentile calculations.
	sorted := make([]int, len(lineLengths))
	copy(sorted, lineLengths)
	sort.Ints(sorted)

	avg := mean(lineLengths)
	med := percentile(sorted, 50)
	p95 := percentile(sorted, 95)
	p99 := percentile(sorted, 99)
	sd := stddev(lineLengths, avg)

	a.LineLengthAvg = &avg
	a.LineLengthMedian = &med
	a.LineLengthP95 = &p95
	a.LineLengthP99 = &p99
	a.LineLengthStddev = &sd

	return a
}

// detectLineEnding reads the first 64 KB of a file and determines the line ending style.
func detectLineEnding(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "LF"
	}
	defer f.Close()

	buf := make([]byte, 64*1024)
	n, _ := f.Read(buf)
	if n == 0 {
		return "LF"
	}
	sample := buf[:n]

	crlfCount := 0
	for i := 0; i < len(sample)-1; i++ {
		if sample[i] == '\r' && sample[i+1] == '\n' {
			crlfCount++
		}
	}
	crCount := 0
	for _, b := range sample {
		if b == '\r' {
			crCount++
		}
	}
	crCount -= crlfCount // Subtract CRLFs from CR count.

	lfCount := 0
	for _, b := range sample {
		if b == '\n' {
			lfCount++
		}
	}
	lfCount -= crlfCount // Subtract CRLFs from LF count.

	var endings []string
	if crlfCount > 0 {
		endings = append(endings, "CRLF")
	}
	if lfCount > 0 {
		endings = append(endings, "LF")
	}
	if crCount > 0 {
		endings = append(endings, "CR")
	}

	switch len(endings) {
	case 0:
		return "LF"
	case 1:
		return endings[0]
	default:
		return "mixed"
	}
}

// --- math helpers ---

func mean(data []int) float64 {
	if len(data) == 0 {
		return 0
	}
	var sum float64
	for _, v := range data {
		sum += float64(v)
	}
	return sum / float64(len(data))
}

func percentile(sorted []int, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	n := len(sorted)
	k := float64(n-1) * p / 100.0
	f := int(k)
	c := f + 1
	if c >= n {
		c = f
	}
	return float64(sorted[f]) + (k-float64(f))*(float64(sorted[c])-float64(sorted[f]))
}

func stddev(data []int, avg float64) float64 {
	if len(data) <= 1 {
		return 0
	}
	var sumSq float64
	for _, v := range data {
		diff := float64(v) - avg
		sumSq += diff * diff
	}
	return math.Sqrt(sumSq / float64(len(data)-1))
}

// --- byte helpers ---

func countNewlines(data []byte) int {
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

func indexOf(data []byte, b byte) int {
	for i, v := range data {
		if v == b {
			return i
		}
	}
	return -1
}

func trimWhitespace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}
