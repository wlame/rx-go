package analyzer

// This file implements cross-worker anomaly deduplication.
//
// Background: the index builder shards the file across K workers, and
// each worker scans its range plus a W-line overlap into the next
// chunk. That overlap is what guarantees detectors with multi-line
// cues (e.g. a traceback whose open line lands at the seam) see the
// whole pattern — at the cost of detecting it on BOTH sides of the
// boundary. We de-dup those exact repeats here.
//
// Design points:
//
//   - Dedup key is (detector name, start_offset, end_offset). Offsets
//     are absolute byte positions in the source file; if two workers
//     emit an anomaly for the same detector at the same byte range, we
//     keep only the first.
//
//   - The Anomaly.DetectorName field stores the detector identifier
//     stamped by the coordinator's Finalize. It is independent of
//     Anomaly.Category (which is the semantic bucket the detector
//     chose) — distinct detectors that share a category won't collapse.
//
//   - Order preservation: within one input group, anomalies come out in
//     the same order they went in. Across groups, groups are processed
//     in the order the slice was passed — so for the same input, the
//     output is byte-identical run-to-run (important for snapshot-style
//     tests elsewhere in the repo).
//
//   - Allocation profile: one map keyed by a small struct + one output
//     slice. No per-anomaly allocation beyond the map entry itself. The
//     benchmark BenchmarkDeduplicate_1000x10 guards against regressions.

// dedupKey is the composite key used to collapse duplicates. Keeping it
// local (unexported) means callers can't accidentally couple to the key
// shape — if we ever switch to a true detector_name field, only this
// file changes.
type dedupKey struct {
	detector    string
	startOffset int64
	endOffset   int64
}

// Deduplicate merges per-worker anomaly slices into a single flat list,
// collapsing exact duplicates that arise from the W-line chunk overlap
// used by the index builder.
//
// The key is (Anomaly.DetectorName, StartOffset, EndOffset).
// DetectorName is stamped by the coordinator's Finalize path. Two
// anomalies with identical keys are considered the same event; the
// first occurrence wins — subsequent matches are discarded verbatim
// (we do NOT merge fields, because a duplicate should be bitwise-
// identical by construction).
//
// Order: within each input group, the output preserves input order; the
// groups themselves are processed left-to-right. Given the same input,
// the output is deterministic — essential for reproducible index files
// and for tests that snapshot the anomaly list.
//
// Empty input (nil or all-nil-inner) returns nil so the caller can
// distinguish "no anomalies" from "one empty anomaly slice" without
// special-casing.
func Deduplicate(groups [][]Anomaly) []Anomaly {
	// Early out: nothing to merge. Returning nil (rather than an empty
	// slice) keeps JSON encoding compact — omitempty drops the field.
	if len(groups) == 0 {
		return nil
	}

	// First pass: count to pre-size both the output and the dedup set.
	// The upper bound is the sum of all group sizes; actual output will
	// be smaller when overlaps collapse. Pre-sizing avoids grow cycles
	// in the common case where dedup is a small percentage of the total.
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	if total == 0 {
		return nil
	}

	// Map acts as the "seen set". Value type is struct{} to make the
	// map entry as compact as possible — we only care about presence.
	seen := make(map[dedupKey]struct{}, total)
	out := make([]Anomaly, 0, total)

	// Second pass: emit each anomaly the first time its key appears.
	// Using a plain range over the outer slice preserves group order;
	// the inner range preserves order within a group.
	for _, group := range groups {
		for _, a := range group {
			k := dedupKey{
				detector:    a.DetectorName,
				startOffset: a.StartOffset,
				endOffset:   a.EndOffset,
			}
			if _, ok := seen[k]; ok {
				// Seen before. By construction from the chunk-overlap
				// policy the two anomalies are bitwise-identical, so
				// throwing away the second loses no information.
				continue
			}
			seen[k] = struct{}{}
			out = append(out, a)
		}
	}

	// Return nil instead of an empty slice when every anomaly was a
	// duplicate (pathologically unlikely but possible). Keeps behavior
	// aligned with the early-return above and lets JSON omitempty drop
	// the field consistently.
	if len(out) == 0 {
		return nil
	}
	return out
}
