package findings

import (
	"time"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
)

// CacheVersion is the format version of the findings cache. A
// directory whose meta.json carries another version counts as absent.
//
// 2: the entry key no longer hashes the keyword clauses derived from a
// signature's anchor, only the ones its author declared, so two
// implementations of rx that factor a regex differently share the
// cache. Every entry of version 1 is scanned again once.
const CacheVersion = 2

// State says how much of the default set of signatures a file has
// current entries for.
type State string

// The three states of a file.
const (
	// StateFull: every signature of the default set has a current entry.
	StateFull State = "full"
	// StatePartial: at least one current entry, opt-in signatures
	// included, and not full. A missing opt-in signature never keeps a
	// file from full.
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
	// MaskSecrets turns masking on (`--mask-secrets`, RX_MASK_SECRETS):
	// every loaded redacting signature of the catalogue a call is given
	// (Catalogue.Redactors) masks the excerpts the scan keeps and the
	// text of every finding. Off, no signature masks anything, and the
	// answers, the cache and the text show the file's text as it is.
	// The mode enters every key through the redacting set it hashes, so
	// masked and unmasked entries are never taken for each other.
	MaskSecrets bool
	// Scan holds the scan's settings. LineBytes, Cap and LookbackBytes
	// enter every key; Workers does not change an answer and is not in
	// the key. Redactors is replaced by the redacting set MaskSecrets
	// gives (redactorsOf), the set every key hashes.
	Scan sigscan.Options
	// LogZone is the canonical value of RX_LOG_TZ (config.LogTZ().Name),
	// the zone the answers read a zone-less file's times in. The scan
	// does not use it; it enters every key, so entries written under
	// another zone are stale.
	LogZone string
	// Dir is the findings cache directory, config.GetFindingsCacheDir()
	// in production. Required unless NoCache or Spooled is set.
	Dir string
	// MayWrite, when set, is asked once a scan has read the file and
	// before it writes anything to the cache: false writes nothing, and
	// Scan returns context.Canceled. It is the scan's point of no
	// return: a caller that can cancel the scan (a findings task) settles
	// there, so a cancel either comes before it and nothing is written,
	// or comes after it and is refused. It is not asked when the cache
	// answers every selected signature, nor with NoCache or Spooled.
	MayWrite func() bool
}

// usesCache reports whether the options let a call read and write the
// cache.
func (o ScanOptions) usesCache() bool { return !o.NoCache && !o.Spooled }

// redactorsOf is the redacting set of a call with options o over the
// catalogue cat: every loaded redacting signature of cat with masking
// on, none with masking off. It is the one place the mode is read: the
// scan masks by this set (sigscan.Options.Redactors), every key and the
// key of the masked ranges hash it, and the text of a finding is masked
// by it, so a scan given no redactors is what "masking off" means.
func redactorsOf(cat *signatures.Catalogue, o ScanOptions) []*signatures.Signature {
	if !o.MaskSecrets {
		return nil
	}
	return cat.Redactors()
}

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
	// MaskedRanges are the file's masked ranges (sigscan.Result's): the
	// bodies of every redacting block and run of the redacting set (none
	// with masking off), as [first, last] lines, sorted and disjoint.
	// Every carrier of the text of a finding (its lines, the lines
	// before and after it) shows a line in one of them as
	// sigscan.MaskedLineText of its length. A
	// fresh scan gives them; a cached answer reads them from the cache,
	// and an answer holds cached findings only together with their
	// ranges. Nil when the answer holds no findings.
	MaskedRanges [][2]int64
	// TextLines and TextBytes are the end of the text the findings and
	// the ranges describe (sigscan.Result's): no carrier shows a line
	// past it, of a file that grew since it was scanned. 0 when the
	// answer holds no findings.
	TextLines, TextBytes int64
	// Cached is how many of the selected signatures the answer took
	// from the cache rather than from a scan: every checked one for
	// Read, the ones with a current entry for Scan (none with Force,
	// NoCache or Spooled).
	Cached int
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
