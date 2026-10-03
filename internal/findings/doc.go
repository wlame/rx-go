// Package findings keeps the findings of signature scans in a cache on
// disk, one directory per file and one entry per signature, and runs
// the scans the CLI and the HTTP routes ask for: Scan answers a
// selection of signatures for one file from the cache where it can and
// scans only the signatures the cache does not hold; Read answers from
// the cache alone; Summary says how much of the default set a file has
// cached, for the file tree.
//
// # Layout
//
// The cache lives in ScanOptions.Dir (<cache>/findings/, beside
// indexes/ and trace_cache/). Each scanned file has a directory named
// after its base name and a hash of its absolute path:
//
//	<dir>/<safe_basename>_<sha256(abs path)[:16]>/
//	    meta.json            the commit point: the file's identity, one summary per signature, the ranges' key
//	    meta.ranges.json     the file's masked ranges (the body lines of every redacting block and run) and the end of the text read
//	    <signature-id>.json  one entry: its key, its findings and its density series
//	    .lock                taken (flock) while a process writes the directory
//
// # Staleness
//
// An entry is current when meta.json records the file as it is now
// (index.SourceIdentity: size, mtime, inode, device, ctime and
// fingerprint, the check the line index uses) and its key equals Key
// for the signature as it is loaded now. The key hashes the
// signature's matching part, the engine and measure versions, the
// settings that change answers and the redacting set (the redacting
// signatures with masking on, none with it off: ScanOptions.MaskSecrets),
// so an entry of one mode is never current in the other; name,
// description, group and the other descriptive fields are not in it.
//
// The masked ranges have a key of their own (the engine version, the
// line view and the redacting set). Entries are used only
// together with current ranges, since the text of a cached finding is
// masked by them when it is read: without them every entry is missing.
//
// # Writing
//
// A scan writes each new entry first (a temporary file, then a rename),
// then the masked ranges, then meta.json (the same way). A reader trusts only what meta.json
// names, and checks each entry file's key against it, so a crash
// between the two steps leaves the old answer readable; the entry file
// meta.json does not name is removed at the next write. Before it
// writes meta.json a writer reads it again and keeps the entries of
// any other writer that still describe the same file, so two processes
// adding different signatures to one file keep both. The whole write
// holds an advisory lock on the directory's .lock file, which makes it
// one step for other processes.
//
// Every operation goes through the file's directory opened as an
// *os.Root, and nothing below ScanOptions.Dir is followed as a symbolic
// link: a directory, meta.json, entry or lock file that is a link (or a
// special file) is a miss, never read, written or deleted through.
// ScanOptions.Dir itself is followed, as the other caches' directories
// are: it is the operator's choice of where the cache lives.
//
// # Concurrency
//
// Within a process, one scan of a file runs at a time: a second caller
// waits for the first, then scans what is still missing. At most
// SetMaxScans scans read files at once; later ones wait in a queue, in
// the order they came. A scan answered wholly from the cache does not
// wait in that queue.
//
// The package reads no environment: every setting comes through
// ScanOptions, filled by the commands and routes.
package findings
