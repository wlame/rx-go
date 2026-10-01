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
//	    meta.json            the commit point: the file's identity and one summary per signature
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
// settings that change answers and the redacting signatures; name,
// description, group and the other descriptive fields are not in it.
//
// # Writing
//
// A scan writes each new entry first (a temporary file, then a rename),
// then meta.json (the same way). A reader trusts only what meta.json
// names, and checks each entry file's key against it, so a crash
// between the two steps leaves the old answer readable; the entry file
// meta.json does not name is removed at the next write. Before it
// writes meta.json a writer reads it again and keeps the entries of
// any other writer that still describe the same file, so two processes
// adding different signatures to one file keep both. The whole write
// holds an advisory lock on the directory's .lock file, which makes it
// one step for other processes.
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
