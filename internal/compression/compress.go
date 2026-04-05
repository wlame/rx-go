// compress.go creates seekable zstd files from uncompressed input.
//
// The output file contains independently-decompressable zstd frames with newline-aligned
// boundaries, followed by a seek table as a skippable frame. This enables parallel
// decompression and search without full sequential decompression.
//
// Frame creation algorithm:
//  1. Read ~FrameSize bytes from input.
//  2. If the chunk does not end with '\n', read forward until a newline is found.
//  3. Compress the chunk as an independent zstd frame.
//  4. Write to output, track compressed/decompressed sizes.
//  5. After all frames, write the seek table (skippable frame + footer).
package compression

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"

	"github.com/klauspost/compress/zstd"
)

// DefaultFrameSize is the target decompressed size per frame (4 MB).
const DefaultFrameSize = 4 * 1024 * 1024

// DefaultCompressionLevel is the default zstd compression level (3 is a good balance
// of speed vs ratio).
const DefaultCompressionLevel = 3

// CompressOpts controls seekable zstd creation.
type CompressOpts struct {
	FrameSize        int // Target decompressed bytes per frame (default: 4 MB).
	CompressionLevel int // Zstd level 1-22 (default: 3).
}

// applyDefaults fills zero-value fields with defaults.
func (o *CompressOpts) applyDefaults() {
	if o.FrameSize <= 0 {
		o.FrameSize = DefaultFrameSize
	}
	if o.CompressionLevel <= 0 {
		o.CompressionLevel = DefaultCompressionLevel
	}
}

// CreateSeekableZstd reads uncompressed data from input, splits it into newline-aligned
// frames, compresses each frame independently with zstd, writes the compressed frames
// sequentially to output, and appends a seek table at the end.
//
// Returns the seek table describing the created file. The caller can use it to verify
// the output or for immediate indexing.
func CreateSeekableZstd(input io.Reader, output io.Writer, opts CompressOpts) (*SeekTable, error) {
	opts.applyDefaults()

	// Map zstd.SpeedDefault, etc. from our integer level.
	encLevel := zstd.EncoderLevel(opts.CompressionLevel)

	var frames []FrameEntry
	var compressedOffset, decompressedOffset int64

	// We read into a reusable buffer to avoid per-frame allocations.
	buf := make([]byte, opts.FrameSize)
	// Extra buffer for reading past the frame boundary to find a newline.
	extra := make([]byte, 4096)
	var leftover []byte // Data read past a newline that belongs to the next frame.

	for {
		// Assemble the chunk for this frame.
		chunk, eof, err := readFrameChunk(input, buf, extra, &leftover, opts.FrameSize)
		if err != nil {
			return nil, fmt.Errorf("read frame chunk: %w", err)
		}
		if len(chunk) == 0 {
			break
		}

		// Compress this chunk as a single independent zstd frame.
		compressed, err := compressFrame(chunk, encLevel)
		if err != nil {
			return nil, fmt.Errorf("compress frame %d: %w", len(frames), err)
		}

		// Write compressed data to output.
		if _, err := output.Write(compressed); err != nil {
			return nil, fmt.Errorf("write compressed frame %d: %w", len(frames), err)
		}

		frames = append(frames, FrameEntry{
			CompressedOffset:   compressedOffset,
			CompressedSize:     uint32(len(compressed)),
			DecompressedOffset: decompressedOffset,
			DecompressedSize:   uint32(len(chunk)),
		})

		compressedOffset += int64(len(compressed))
		decompressedOffset += int64(len(chunk))

		if eof {
			break
		}
	}

	// Write the seek table at end of file.
	if err := writeSeekTable(output, frames); err != nil {
		return nil, fmt.Errorf("write seek table: %w", err)
	}

	slog.Info("seekable zstd created",
		"frames", len(frames),
		"compressed_bytes", compressedOffset,
		"decompressed_bytes", decompressedOffset)

	return &SeekTable{Frames: frames}, nil
}

// readFrameChunk reads approximately frameSize bytes from input, aligned to a newline
// boundary. Returns the chunk data, whether EOF was reached, and any error.
//
// leftover is data that was read past the previous frame's newline boundary and needs
// to be included at the start of this frame.
func readFrameChunk(input io.Reader, buf, extra []byte, leftover *[]byte, frameSize int) ([]byte, bool, error) {
	var chunk []byte

	// Start with any leftover data from the previous frame.
	if len(*leftover) > 0 {
		chunk = append(chunk, *leftover...)
		*leftover = nil
	}

	// Read up to frameSize bytes.
	remaining := frameSize - len(chunk)
	if remaining > len(buf) {
		remaining = len(buf)
	}
	if remaining > 0 {
		n, err := io.ReadAtLeast(input, buf[:remaining], 1)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if n > 0 {
					chunk = append(chunk, buf[:n]...)
				}
				return chunk, true, nil
			}
			return nil, false, err
		}
		chunk = append(chunk, buf[:n]...)
	}

	// If the chunk already ends with a newline, we're done.
	if len(chunk) > 0 && chunk[len(chunk)-1] == '\n' {
		return chunk, false, nil
	}

	// Read forward in small increments to find the next newline.
	for {
		n, err := input.Read(extra)
		if n > 0 {
			// Search for newline in the extra bytes.
			for i := 0; i < n; i++ {
				if extra[i] == '\n' {
					// Include bytes up to and including the newline.
					chunk = append(chunk, extra[:i+1]...)
					// Save the rest as leftover for the next frame.
					if i+1 < n {
						*leftover = append([]byte{}, extra[i+1:n]...)
					}
					return chunk, false, nil
				}
			}
			// No newline found in this read — include all and keep looking.
			chunk = append(chunk, extra[:n]...)
		}
		if err != nil {
			// EOF without finding a newline — return what we have.
			return chunk, true, nil
		}
	}
}

// compressFrame compresses data as a single independent zstd frame.
func compressFrame(data []byte, level zstd.EncoderLevel) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
	if err != nil {
		return nil, err
	}
	defer enc.Close()

	return enc.EncodeAll(data, nil), nil
}

// writeSeekTable writes the seek table as a zstd skippable frame at the current
// position in the writer. The format matches the seekable zstd specification:
//
//	Skippable Frame Header (8 bytes):
//	  Magic:      0x184D2A5E  (LE u32)
//	  Frame Size: N           (LE u32)
//
//	Per-frame entries (8 bytes each):
//	  compressed_size:   LE u32
//	  decompressed_size: LE u32
//
//	Footer (9 bytes):
//	  Magic:      0x8F92EAB1  (LE u32)
//	  num_frames: LE u32
//	  flags:      u8 (0 = no checksums)
func writeSeekTable(w io.Writer, frames []FrameEntry) error {
	// Build seek table entries.
	entryData := make([]byte, len(frames)*8)
	for i, f := range frames {
		off := i * 8
		binary.LittleEndian.PutUint32(entryData[off:off+4], f.CompressedSize)
		binary.LittleEndian.PutUint32(entryData[off+4:off+8], f.DecompressedSize)
	}

	// Build footer.
	var footer [9]byte
	binary.LittleEndian.PutUint32(footer[0:4], SeekTableFooterMagic)
	binary.LittleEndian.PutUint32(footer[4:8], uint32(len(frames)))
	footer[8] = 0 // flags: no checksums

	// Total seek table content = entries + footer.
	tableContent := append(entryData, footer[:]...)

	// Skippable frame header.
	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], SeekableSkippableMagic)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(tableContent)))

	// Write header + content.
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("write skippable header: %w", err)
	}
	if _, err := w.Write(tableContent); err != nil {
		return fmt.Errorf("write seek table content: %w", err)
	}

	return nil
}
