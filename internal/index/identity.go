package index

import (
	"os"
)

// SourceIdentity is what a cache records about the file it was built
// from, so that a later run can tell whether the file on disk is still
// that file. The line index and the trace cache both stamp one and both
// check it through MatchesFile, so the two caches agree on when a file
// has changed.
//
// Size and mtime alone cannot tell a rewritten file from an untouched
// one: a copy made with `cp -p` restores the mtime, and an edit that
// swaps one byte for another keeps the byte count. The inode changes
// when a file is replaced by rename, the ctime moves on every write and
// cannot be set through utime, and the fingerprint (a digest of the
// size plus the first and last 64 KiB, see SourceFingerprint) catches a
// rewrite on a filesystem that reports neither faithfully.
//
// The pointer fields are nil when the value is not known: the platform
// reported no inode or ctime, the fingerprint could not be read, or the
// record predates the field. MatchesFile does not compare a nil field.
type SourceIdentity struct {
	// SizeBytes is the file size in bytes.
	SizeBytes int64
	// ModifiedAt is the mtime in the layout FormatMtime produces.
	ModifiedAt string
	// Inode is the inode number, or nil.
	Inode *uint64
	// ChangedAt is the inode-change time (ctime) in the FormatMtime
	// layout, or nil.
	ChangedAt *string
	// Fingerprint is SourceFingerprint of the file, or nil.
	Fingerprint *string
}

// IdentityFromInfo describes the file at path as the stat result info
// saw it. The fingerprint is read from path now, so a caller that wants
// the identity of a file at one moment stats it once and passes that
// result here, rather than stating again.
//
// If the file grows between the stat and the fingerprint read, the
// fingerprint covers the larger file while SizeBytes records the
// smaller one. MatchesFile then never accepts the identity, which costs
// a cache rebuild and never a wrong answer.
//
// A fingerprint that cannot be read is left nil rather than reported as
// an error; MatchesFile then falls back to the other fields.
func IdentityFromInfo(path string, info os.FileInfo) SourceIdentity {
	inode, changedAt := SourceIdentityFields(info)
	id := SourceIdentity{
		SizeBytes:  info.Size(),
		ModifiedAt: formatMtime(info.ModTime()),
		Inode:      inode,
		ChangedAt:  changedAt,
	}
	if fp, err := SourceFingerprint(path); err == nil {
		id.Fingerprint = &fp
	}
	return id
}

// MatchesFile reports whether the file at path is still the file id
// describes. A file that cannot be stated does not match.
//
// The cheap checks run first (size, mtime, inode, ctime come from one
// stat call) so the two 64 KiB fingerprint reads happen only for a file
// that passed them.
func (id SourceIdentity) MatchesFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.Size() != id.SizeBytes {
		return false
	}
	if formatMtime(info.ModTime()) != id.ModifiedAt {
		return false
	}
	if !id.matchesInodeAndCtime(info) {
		return false
	}
	return id.matchesFingerprint(path)
}

// matchesInodeAndCtime compares the recorded inode and ctime with the
// ones in info.
//
// A nil field means the record predates the check or the filesystem did
// not report one, so an absent value is not treated as a mismatch. On a
// platform where the stat result carries neither, there is nothing to
// compare and the check passes.
func (id SourceIdentity) matchesInodeAndCtime(info os.FileInfo) bool {
	inode, changed, ok := sourceIdentity(info)
	if !ok {
		return true
	}
	if id.Inode != nil && *id.Inode != inode {
		return false
	}
	if id.ChangedAt != nil && *id.ChangedAt != formatMtime(changed) {
		return false
	}
	return true
}

// matchesFingerprint re-reads the two 64 KiB windows the fingerprint
// covers and compares the digest.
//
// A nil fingerprint is not a mismatch, so a record written before the
// field existed still validates on the other fields. A read failure is
// a mismatch: if we cannot confirm the file is the one that was
// recorded, the record does not get used.
func (id SourceIdentity) matchesFingerprint(path string) bool {
	if id.Fingerprint == nil {
		return true
	}
	current, err := SourceFingerprint(path)
	if err != nil {
		return false
	}
	return current == *id.Fingerprint
}
