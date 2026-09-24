package webapi

import (
	"slices"
)

// featureTable names what this build serves beyond the contract's
// major version, one row per feature a client may look for before it
// shows a control that needs it. GET /health lists them as `features`;
// docs/api/endpoints/health.md describes each, and a test keeps the
// two in step.
//
// A name is listed while the build serves it. A client asks "is the name
// listed", never compares versions, so a row is added with the feature
// and removed with it.
var featureTable = []string{
	// trace_matching_flags: GET /v1/trace takes ripgrep's matching
	// flags (ignore_case, word_regexp, …).
	"trace_matching_flags",
	// trace_context_and_switches: GET /v1/trace takes context lines and
	// no_cache, no_index and no_recursive.
	"trace_context_and_switches",
	// samples_index_build: GET /v1/samples builds a large or compressed
	// file's line index in a background task and may answer 202, and
	// GET /v1/tasks/{task_id} reports the build's progress.
	"samples_index_build",
	// samples_timestamps: GET /v1/samples takes `timestamps`.
	"samples_timestamps",
	// line_timestamps: every samples answer carries line_timestamps.
	"line_timestamps",
	// time_range: GET /v1/time-range gives a file's time range.
	"time_range",
	// file_tz: GET /v1/samples and GET /v1/time-range take `file_tz`,
	// which reads a file's timestamps in a chosen zone.
	"file_tz",
	// log_chains: GET /v1/logs/chains lists the log chains of a
	// directory (rotated logs found by their file names).
	"log_chains",
}

// Features returns the names of featureTable, sorted.
func Features() []string {
	return slices.Sorted(slices.Values(featureTable))
}
