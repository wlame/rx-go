package logchain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"

	"github.com/wlame/rx-go/internal/index"
)

// fingerprintHexDigits is how many hex digits of the SHA-256 a
// fingerprint keeps: 64 bits, far more than the parts of one chain's
// history can collide in.
const fingerprintHexDigits = 16

// Fingerprint is a short hash that changes when the files of a chain
// change: the first 16 hex digits of a SHA-256 over its parts, in the
// provisional order, each part as the encoding the chain reads (its
// duplicates do not count).
//
// A frozen part counts with its name, device, inode, size and
// modification time in nanoseconds, so a rename, a compression, a
// deletion, a new part and a write to a frozen part all change it. The
// active part counts with its name, device and inode only, so the log
// growing does not change it; replacing the active file (a rotation
// creates a new one) does.
//
// The values come from the stat the listing took (Part.Info); nothing
// is read. On a platform that gives no inode and device, both count as
// 0 and the other fields still change.
func Fingerprint(c Candidate) string {
	h := sha256.New()
	for _, p := range c.Parts {
		writeFingerprintPart(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))[:fingerprintHexDigits]
}

// writeFingerprintPart writes one part's fields to h, each name length
// first, so no two different lists of parts write the same bytes.
//
// Go note: a hash.Hash's Write never returns an error (the interface
// says so), and binary.Write of fixed-size integers to it neither, so
// the errors are not checked.
func writeFingerprintPart(h hash.Hash, p Part) {
	inode, device, _ := index.InodeAndDevice(p.Info)
	kind := byte('f')
	if p.IsActive {
		kind = 'a'
	}
	_, _ = h.Write([]byte{kind})
	_ = binary.Write(h, binary.LittleEndian, uint64(len(p.Name)))
	_, _ = h.Write([]byte(p.Name))
	_ = binary.Write(h, binary.LittleEndian, [2]uint64{device, inode})
	if !p.IsActive {
		_ = binary.Write(h, binary.LittleEndian, [2]int64{p.Info.Size(), p.Info.ModTime().UnixNano()})
	}
}
