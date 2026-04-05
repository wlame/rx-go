package analyze

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"time"
)

// Analyze scans a file line by line, running each detector's CheckLine on every line,
// and collects anomaly results. This is the skeleton orchestrator -- it handles the
// zero-detectors fast path and basic line iteration, but does not yet implement
// prescan optimization or advanced merge logic.
//
// With zero detectors the function returns an empty AnalysisResult immediately.
func Analyze(ctx context.Context, path string, detectors []Detector) (*AnalysisResult, error) {
	result := &AnalysisResult{
		FilePath:  path,
		Anomalies: []AnomalyResult{},
	}

	// Fast path: nothing to do when no detectors are provided.
	if len(detectors) == 0 {
		return result, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("analyze %s: %w", path, err)
	}
	defer f.Close()

	start := time.Now()

	// prevSeverity tracks the last severity each detector produced so that
	// ShouldMerge can decide whether to extend the previous anomaly range.
	// A value of -1 means "no active anomaly for this detector".
	prevSeverity := make(map[string]float64, len(detectors))
	// activeAnomaly tracks the in-progress anomaly for each detector (by name).
	activeAnomaly := make(map[string]*AnomalyResult, len(detectors))

	for _, d := range detectors {
		prevSeverity[d.Name()] = -1
	}

	scanner := bufio.NewScanner(f)
	lineNumber := 0
	var byteOffset int64

	// previousLines is a simple sliding window of the last N lines.
	const windowSize = 20
	var previousLines []string

	for scanner.Scan() {
		// Respect context cancellation so long-running analysis can be stopped.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("analyze %s: %w", path, err)
		}

		lineNumber++
		lineText := scanner.Text()

		lctx := LineContext{
			Line:          lineText,
			LineNumber:    lineNumber,
			ByteOffset:    byteOffset,
			FilePath:      path,
			PreviousLines: previousLines,
		}

		for _, d := range detectors {
			name := d.Name()
			severity := d.CheckLine(lctx)

			if severity >= 0 {
				// Line is anomalous.
				prev := prevSeverity[name]
				if prev >= 0 && d.ShouldMerge(lctx, prev) {
					// Extend the existing anomaly range.
					active := activeAnomaly[name]
					active.EndLine = lineNumber
					active.EndOffset = byteOffset
					if severity > active.Severity {
						active.Severity = severity
					}
				} else {
					// Flush any previous anomaly for this detector.
					if a, ok := activeAnomaly[name]; ok {
						result.Anomalies = append(result.Anomalies, *a)
					}
					// Start a new anomaly.
					activeAnomaly[name] = &AnomalyResult{
						Detector:    name,
						Category:    d.Category(),
						Severity:    severity,
						StartLine:   lineNumber,
						EndLine:     lineNumber,
						StartOffset: byteOffset,
						EndOffset:   byteOffset,
						Description: fmt.Sprintf("Detected by %s", name),
					}
				}
				prevSeverity[name] = severity
			} else {
				// Line is not anomalous -- flush any active anomaly for this detector.
				if a, ok := activeAnomaly[name]; ok {
					result.Anomalies = append(result.Anomalies, *a)
					delete(activeAnomaly, name)
				}
				prevSeverity[name] = -1
			}
		}

		// Advance byte offset past this line (text + newline).
		byteOffset += int64(len(scanner.Bytes())) + 1

		// Maintain the sliding window of previous lines.
		previousLines = append(previousLines, lineText)
		if len(previousLines) > windowSize {
			previousLines = previousLines[1:]
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("analyze %s: scan error: %w", path, err)
	}

	// Flush any remaining active anomalies.
	for _, a := range activeAnomaly {
		result.Anomalies = append(result.Anomalies, *a)
	}

	result.LinesScanned = lineNumber
	result.Duration = time.Since(start)

	return result, nil
}
