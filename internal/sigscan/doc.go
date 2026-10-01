// Package sigscan scans one file's text for a chosen set of signatures
// in one parallel Go pass and returns their findings, with exact line
// numbers and offsets.
//
// A scan (Scan) reads the text once:
//
//   - a plain file, or a seekable zstd file, is cut into line-aligned
//     chunks (about eight per worker, at least 4 MiB each) that workers
//     read in parallel;
//   - a gzip, bzip2, xz or plain zstd file is decoded by one goroutine,
//     which hands batches of whole lines (about 4 MiB) to the workers.
//
// Each worker walks the lines of its chunk or batch. For every line it
// builds the line view (the line without its ending, a UTF-8 byte-order
// mark at the start of the text left out, cut at Options.LineBytes),
// runs one Aho-Corasick pass over the view that finds every keyword of
// every selected signature (ac.go), and tests a signature's anchor only
// when the line holds a member of every one of its keyword clauses (the
// gate, gate.go), or when the line holds the Kelvin sign or the long s,
// which a case-insensitive k or s matches. Signatures without clauses
// are tested on every line.
// Regex signatures give one finding per line (matchers.go); blocks are a
// state machine over the following lines (block.go); runs track
// consecutive qualifying lines (run.go). Measures, Go code with
// parameters, run on the machinery their findings resemble or on state
// of their own joined by the merge (measures.go).
//
// Results come back to one merge (merge.go), which takes them in text
// order, numbers their lines, joins runs that cross a chunk edge, and
// finishes a block that crosses an edge by walking the next chunk's
// lines again from its start until that walk and the worker's walk of
// the chunk agree. The per-signature cap is applied there, after the
// order is known, so the findings kept are the first ones by line and
// the count stays exact. The answer is the same for any number of
// workers, any chunk size, and any storage of the text.
//
// Every finding has the time of its first line, or of the nearest
// earlier stamped line within Options.LookbackBytes (times.go). The
// scan reads the file's timestamp format and each line's timestamp with
// the index's own functions, never a stored index; the merge carries the
// last stamped line from chunk to chunk to settle the findings whose
// chunk had no stamp before them.
//
// The package never reads the environment: every setting comes through
// Options.
package sigscan
