// Package analyze defines the anomaly detector interface, registry, and file analyzer skeleton.
//
// This package provides the plugin system for anomaly detection in log files.
// Detectors are registered at init time and invoked by the FileAnalyzer during
// line-by-line scanning. In the current phase, only the interface and registry
// are defined -- no concrete detector implementations exist yet.
package analyze

import "time"

// LineContext provides the current line and surrounding context to each detector.
// Detectors use this to make informed decisions about whether a line is anomalous.
type LineContext struct {
	// Line is the current line content (without trailing newline).
	Line string

	// LineNumber is the 1-based line number in the file.
	LineNumber int

	// ByteOffset is the byte offset of this line's start within the file.
	ByteOffset int64

	// FilePath is the absolute path to the file being analyzed.
	FilePath string

	// PreviousLines is a sliding window of the N most recent lines before this one.
	// The most recent previous line is last in the slice.
	PreviousLines []string

	// LineLengths is a sliding window of line lengths (matching PreviousLines).
	LineLengths []int

	// AvgLineLength is the running average line length across the window.
	AvgLineLength float64

	// StddevLineLength is the running standard deviation of line lengths.
	StddevLineLength float64
}

// PrescanPattern pairs a regex pattern string with the detector that wants it.
// Prescan lets large-file analysis run ripgrep first to identify regions of interest
// before doing the full line-by-line scan.
type PrescanPattern struct {
	// Pattern is the regex string suitable for ripgrep.
	Pattern string

	// DetectorName identifies which detector registered this pattern.
	DetectorName string
}

// AnomalyResult describes a single anomaly (or merged range of anomalous lines)
// found by a detector.
type AnomalyResult struct {
	// Detector is the name of the detector that found this anomaly.
	Detector string `json:"detector"`

	// Category groups related detectors (e.g. "error", "format", "security").
	Category string `json:"category"`

	// Severity is a score from 0.0 (informational) to 1.0 (critical).
	Severity float64 `json:"severity"`

	// StartLine is the first line of the anomaly (1-based, inclusive).
	StartLine int `json:"start_line"`

	// EndLine is the last line of the anomaly (1-based, inclusive).
	EndLine int `json:"end_line"`

	// StartOffset is the byte offset of the anomaly's first line.
	StartOffset int64 `json:"start_offset"`

	// EndOffset is the byte offset of the anomaly's last line.
	EndOffset int64 `json:"end_offset"`

	// Description is a human-readable explanation of the anomaly.
	Description string `json:"description"`

	// Metadata holds detector-specific key-value data (e.g. matched keyword,
	// entropy score). May be nil if the detector provides no extra metadata.
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// AnalysisResult holds the output of analyzing a single file.
type AnalysisResult struct {
	// FilePath is the absolute path to the analyzed file.
	FilePath string `json:"file_path"`

	// Anomalies is the list of detected anomalies, sorted by StartLine.
	Anomalies []AnomalyResult `json:"anomalies"`

	// LinesScanned is the total number of lines processed.
	LinesScanned int `json:"lines_scanned"`

	// Duration is how long the analysis took.
	Duration time.Duration `json:"duration"`
}
