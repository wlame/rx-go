package findings

import (
	"time"

	"github.com/wlame/rx-go/internal/sigscan"
)

// CacheVersion is the format version of the findings cache. A
// directory whose meta.json carries another version counts as absent.
const CacheVersion = 1

// State says how much of the default set of signatures a file has
// current entries for.
type State string

// The three states of a file.
const (
	// StateFull: every signature of the default set has a current entry.
	StateFull State = "full"
	// StatePartial: at least one current entry, and not full.
	StatePartial State = "partial"
	// StateNone: no current entry.
	StateNone State = "none"
)

// ScanOptions are the settings of Scan, Read, Summary and Key. The
// commands and routes fill them once from the environment; the same
// options give the same keys, so a Read with the options a Scan used
// finds what that Scan wrote.
type ScanOptions struct {
	// Force rescans every selected signature, current entries included,
	// and replaces their entries.
	Force bool
	// NoCache reads and writes nothing: every selected signature is
	// scanned and nothing is stored.
	NoCache bool
	// Spooled marks piped stdin copied to a temporary file: never read
	// from nor written to the cache, as NoCache.
	Spooled bool
	// Progress, when set, replaces Scan.Progress: it is called as the
	// file is read with the bytes read so far and the file's size, both
	// as stored on disk. It is not called when every selected signature
	// is answered from the cache.
	Progress func(read, total int64)
	// Scan holds the scan's settings. LineBytes, Cap and LookbackBytes
	// enter every key; Workers does not change an answer and is not in
	// the key. Redactors is replaced by Catalogue.Redactors() of the
	// catalogue the call is given, the set every key hashes.
	Scan sigscan.Options
	// LogZone is the canonical value of RX_LOG_TZ (config.LogTZ().Name),
	// the zone the answers read a zone-less file's times in. The scan
	// does not use it; it enters every key, so entries written under
	// another zone are stale.
	LogZone string
	// Dir is the findings cache directory, config.GetFindingsCacheDir()
	// in production. Required unless NoCache or Spooled is set.
	Dir string
}

// usesCache reports whether the options let a call read and write the
// cache.
func (o ScanOptions) usesCache() bool { return !o.NoCache && !o.Spooled }

// Answer is the findings of a selection of signatures in one file.
type Answer struct {
	// State is measured against the default set over every signature
	// whose findings are known for the file after the call: its current
	// cache entries (none with NoCache or Spooled) and, for Scan, the
	// signatures just scanned.
	State State
	// Checked are the selected signatures the answer holds findings for,
	// in selection order; Missing are the selected signatures it holds
	// none for (Read only: a Scan answers every one).
	Checked, Missing []string
	// BySignature are the findings of each checked signature, from the
	// cache or from the scan, equal field by field either way.
	BySignature map[string]*sigscan.SignatureResult
	// ScannedAt is when each checked signature's findings were scanned,
	// in UTC to the second.
	ScannedAt map[string]time.Time
}

// FileSummary is how much of the default set a file has cached, for
// the file tree (Summary).
type FileSummary struct {
	// State is StateFull or StatePartial (a file with no current entry
	// has no FileSummary).
	State State
	// Count is the sum of the findings counts of every current entry,
	// opt-in signatures included.
	Count int64
}
