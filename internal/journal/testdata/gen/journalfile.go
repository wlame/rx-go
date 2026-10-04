package main

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// Offsets of the journal header fields this program reads or writes. A
// journal stores every integer little-endian, and every offset in it
// counts from the start of the file.
const (
	signature               = "LPKSHHRH"
	incompatibleFlagsOffset = 12  // uint32
	headerSizeOffset        = 88  // uint64
	arenaSizeOffset         = 96  // uint64: the bytes after the header the file may use
	tailObjectOffsetOffset  = 136 // uint64: the offset of the last object written
	minHeaderSize           = 208 // the header of the oldest format rx reads
)

// Object layout: every object starts with its type (1 byte), its flags
// (1 byte), 6 reserved bytes and its size (uint64, the header included).
// Objects start on 8-byte boundaries.
const (
	objectHeaderSize = 16
	objectSizeOffset = 8
	dataObjectType   = 1
)

// The incompatible header flags systemd 257 writes.
const (
	flagCompressedXZ   = 1 << 0
	flagCompressedLZ4  = 1 << 1
	flagKeyedHash      = 1 << 2
	flagCompressedZSTD = 1 << 3
	flagCompact        = 1 << 4
)

// compressionFlag ties a compression's name to the header flag that says
// a journal may hold values compressed with it, and to the flag a DATA
// object so compressed carries.
type compressionFlag struct {
	name       string
	headerFlag uint32
	objectFlag uint8
}

// compressionFlags lists the compressions a journal can use.
var compressionFlags = []compressionFlag{
	{"none", 0, 0},
	{"xz", flagCompressedXZ, 1},
	{"lz4", flagCompressedLZ4, 2},
	{"zstd", flagCompressedZSTD, 4},
}

// compressionByName returns the compression called name.
func compressionByName(name string) (compressionFlag, error) {
	i := slices.IndexFunc(compressionFlags, func(c compressionFlag) bool { return c.name == name })
	if i < 0 {
		return compressionFlag{}, fmt.Errorf("unknown compression %q", name)
	}
	return compressionFlags[i], nil
}

// journalHeader is the part of a journal's header this program uses.
type journalHeader struct {
	incompatibleFlags uint32
	headerSize        uint64
	tailObjectOffset  uint64
}

// readHeader checks the signature and reads the header fields of data.
func readHeader(data []byte) (journalHeader, error) {
	if len(data) < minHeaderSize || string(data[:len(signature)]) != signature {
		return journalHeader{}, fmt.Errorf("not a journal: %d bytes without the %s header", len(data), signature)
	}
	h := journalHeader{
		incompatibleFlags: binary.LittleEndian.Uint32(data[incompatibleFlagsOffset:]),
		headerSize:        binary.LittleEndian.Uint64(data[headerSizeOffset:]),
		tailObjectOffset:  binary.LittleEndian.Uint64(data[tailObjectOffsetOffset:]),
	}
	if h.headerSize < minHeaderSize || h.headerSize > uint64(len(data)) {
		return journalHeader{}, fmt.Errorf("header size %d outside the file of %d bytes", h.headerSize, len(data))
	}
	return h, nil
}

// compression returns the compression the header's flags name. A
// journal names at most one.
func (h journalHeader) compression() (compressionFlag, error) {
	known := uint32(flagCompressedXZ | flagCompressedLZ4 | flagKeyedHash | flagCompressedZSTD | flagCompact)
	if unknown := h.incompatibleFlags &^ known; unknown != 0 {
		return compressionFlag{}, fmt.Errorf("unknown incompatible flags %#x", unknown)
	}
	found := compressionFlags[0]
	for _, c := range compressionFlags[1:] {
		if h.incompatibleFlags&c.headerFlag == 0 {
			continue
		}
		if found.headerFlag != 0 {
			return compressionFlag{}, fmt.Errorf("incompatible flags %#x name two compressions", h.incompatibleFlags)
		}
		found = c
	}
	return found, nil
}

// compact reports whether the journal is in compact mode.
func (h journalHeader) compact() bool { return h.incompatibleFlags&flagCompact != 0 }

// usedEnd returns the offset just after the last object: the tail
// object's end, rounded up to the next 8-byte boundary.
func usedEnd(data []byte, h journalHeader) (uint64, error) {
	tail := h.tailObjectOffset
	if tail < h.headerSize || tail > uint64(len(data))-objectHeaderSize {
		return 0, fmt.Errorf("tail object at %d outside the file of %d bytes", tail, len(data))
	}
	size := binary.LittleEndian.Uint64(data[tail+objectSizeOffset:])
	end := align8(tail + size)
	if size < objectHeaderSize || end > uint64(len(data)) {
		return 0, fmt.Errorf("tail object at %d of %d bytes does not fit the file of %d bytes", tail, size, len(data))
	}
	return end, nil
}

// align8 rounds n up to a multiple of 8.
func align8(n uint64) uint64 { return (n + 7) &^ 7 }

// dataObjectFlags returns the flags of every DATA object, walking the
// objects from the end of the header to the tail object.
func dataObjectFlags(data []byte, h journalHeader) ([]uint8, error) {
	end, err := usedEnd(data, h)
	if err != nil {
		return nil, err
	}
	var flags []uint8
	for offset := h.headerSize; offset < end; {
		if offset+objectHeaderSize > end {
			return nil, fmt.Errorf("object header at %d runs past the tail object", offset)
		}
		size := binary.LittleEndian.Uint64(data[offset+objectSizeOffset:])
		if size < objectHeaderSize || offset+size > end {
			return nil, fmt.Errorf("object at %d has size %d", offset, size)
		}
		if data[offset] == dataObjectType {
			flags = append(flags, data[offset+1])
		}
		offset += align8(size)
	}
	return flags, nil
}
