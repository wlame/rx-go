package trace

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// numberedLog is a log whose line n reads "LINE <n> ...", n from 1 to
// lines, with the word NEEDLE on every line whose number is a multiple
// of needleEvery.
func numberedLog(lines, needleEvery int) []byte {
	var b strings.Builder
	for n := 1; n <= lines; n++ {
		fmt.Fprintf(&b, "LINE %d INFO worker=%d", n, n%7)
		if n%needleEvery == 0 {
			b.WriteString(" NEEDLE")
		}
		b.WriteString(" padding padding padding\n")
	}
	return []byte(b.String())
}

// A frame whose bytes cannot be read fails the scan, whatever ripgrep
// made of the frames before it. ripgrep exits 1 when the text it was
// given holds no match, and that exit used to hide the read error: the
// scan came back complete with no matches.
func TestProcessSeekable_ReadErrorFailsTheScanWhenNothingMatched(t *testing.T) {
	requireRipgrep(t)
	path := writeSeekableZstdFile(t, numberedLog(3000, 1000), 8*1024)
	tbl, err := readSeekTable(pinForTest(t, path))
	if err != nil {
		t.Fatalf("readSeekTable: %v", err)
	}
	if tbl.NumFrames < 6 {
		t.Fatalf("fixture has %d frames, want at least 6", tbl.NumFrames)
	}

	failing := tbl.Frames[3]
	readErr := errors.New("input/output error")
	original := decompressFrameForBatch
	decompressFrameForBatch = func(dec *frameDecoder, frame seekable.FrameInfo) ([]byte, error) {
		if frame.Index == failing.Index {
			return nil, fmt.Errorf("read frame at %d: %w", frame.CompressedOffset, readErr)
		}
		return original(dec, frame)
	}
	defer func() { decompressFrameForBatch = original }()

	_, _, _, err = ProcessSeekable(
		context.Background(), pinForTest(t, path),
		map[string]string{"p1": "NOTHING MATCHES THIS"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if !errors.Is(err, readErr) {
		t.Fatalf("ProcessSeekable err = %v, want the read error of frame %d", err, failing.Index)
	}
}

// readableAround reports whether line lies wholly in frames that can be
// read: it overlaps no damaged frame, and it does not hold the first
// byte after one. That byte's line may have started in the damaged
// frame, so it cannot be read whole either.
func readableAround(line textLine, damaged []seekable.FrameInfo) bool {
	end := line.offset + int64(len(line.text)) + 1 // past the line break
	for _, frame := range damaged {
		if line.offset <= frame.DecompressedEnd() && end > frame.DecompressedOffset {
			return false
		}
	}
	return true
}

// A damaged frame costs the lines that touch it and no others. Each
// batch of frames is searched around the damage: the matches before it
// keep their line numbers, the matches after it in the same batch and
// in the other batches are found with their line numbers unknown (the
// damaged frame's line count is lost), and the call reports the damage
// beside them. No line is reported from a fragment, as a match or as
// a context line.
func TestProcessSeekable_SearchesAroundDamagedFrames(t *testing.T) {
	requireRipgrep(t)
	text := numberedLog(6000, 37)
	lines := linesOf(text)
	// Cuts mid-line, so a damaged frame always cuts lines at both ends.
	frames := seekablefile.SplitEvery(text, 1200)
	if len(frames) <= 2*framesPerBatch {
		t.Fatalf("fixture has %d frames, want more than %d for three batches", len(frames), 2*framesPerBatch)
	}

	cases := map[string][]int{
		"inside the first batch":              {40},
		"the last frame of a batch":           {framesPerBatch - 1},
		"the first frame of a batch":          {framesPerBatch},
		"the first frame of the file":         {0},
		"in several batches":                  {40, framesPerBatch, framesPerBatch + 50, 2*framesPerBatch + 3},
		"two frames in a row":                 {60, 61},
		"the frame before the last of a file": {len(frames) - 2},
	}
	for name, damagedFrames := range cases {
		for _, window := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/context %d", name, window), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "damaged.log.zst")
				seekablefile.Write(t, path, frames)
				for _, f := range damagedFrames {
					seekablefile.DamageFrame(t, path, f)
				}
				tbl, err := readSeekTable(pinForTest(t, path))
				if err != nil {
					t.Fatalf("readSeekTable: %v", err)
				}
				var damaged []seekable.FrameInfo
				for _, f := range damagedFrames {
					damaged = append(damaged, tbl.Frames[f])
				}
				firstDamage := damaged[0].DecompressedOffset

				matches, contexts, _, err := ProcessSeekable(
					context.Background(), pinForTest(t, path),
					map[string]string{"p1": "NEEDLE"}, []string{"p1"},
					nil, window, window, nil,
				)
				if !errors.Is(err, seekable.ErrDamagedFrame) {
					t.Fatalf("err = %v, want one wrapping seekable.ErrDamagedFrame", err)
				}
				for _, f := range damagedFrames {
					if !strings.Contains(err.Error(), strconv.Itoa(f)) {
						t.Errorf("err %q does not name damaged frame %d", err, f)
					}
				}

				var want []textLine
				for _, line := range lines {
					if strings.Contains(line.text, "NEEDLE") && readableAround(line, damaged) {
						want = append(want, line)
					}
				}
				if len(matches) != len(want) {
					t.Fatalf("got %d matches, want %d", len(matches), len(want))
				}
				for i, m := range matches {
					w := want[i]
					if m.Offset != w.offset || m.LineText != w.text {
						t.Fatalf("match %d: got byte %d %q, want byte %d %q", i, m.Offset, m.LineText, w.offset, w.text)
					}
					wantLine := 0 // unknown past the first damaged frame
					if m.Offset < firstDamage {
						wantLine = w.number
					}
					if m.AbsoluteLine != wantLine {
						t.Errorf("match at byte %d: line %d, want %d", m.Offset, m.AbsoluteLine, wantLine)
					}
				}

				byOffset := map[int64]textLine{}
				for _, line := range lines {
					byOffset[line.offset] = line
				}
				for _, c := range contexts {
					line, ok := byOffset[c.Offset]
					if !ok || c.LineText != line.text || !readableAround(line, damaged) {
						t.Errorf("context at byte %d %q is not a readable line of the text", c.Offset, c.LineText)
						continue
					}
					if c.AbsoluteLine != 0 && c.AbsoluteLine != line.number {
						t.Errorf("context at byte %d: line %d, want %d", c.Offset, c.AbsoluteLine, line.number)
					}
				}
			})
		}
	}
}

// The start of a line that runs into a damaged frame is not a line. Fed
// to ripgrep as its last input, it would match a pattern anchored at
// the end of a line, which the whole line does not.
func TestProcessSeekable_LineCutByDamageDoesNotMatchAsALine(t *testing.T) {
	requireRipgrep(t)
	var b strings.Builder
	for n := 1; n <= 200; n++ {
		fmt.Fprintf(&b, "LINE %d head-tail\n", n)
	}
	text := []byte(b.String())
	// Frame 1 ends inside line 100, just after "head"; frame 2 holds
	// the rest of that line and is damaged.
	line100 := bytes.Index(text, []byte("LINE 100 "))
	cut := line100 + len("LINE 100 head")
	path := filepath.Join(t.TempDir(), "cut.log.zst")
	seekablefile.Write(t, path, seekablefile.SplitAt(text, line100/2, cut, cut+600))
	seekablefile.DamageFrame(t, path, 2)

	matches, contexts, _, err := ProcessSeekable(
		context.Background(), pinForTest(t, path),
		map[string]string{"p1": "head$"}, []string{"p1"},
		nil, 1, 1, nil,
	)
	if !errors.Is(err, seekable.ErrDamagedFrame) {
		t.Fatalf("err = %v, want one wrapping seekable.ErrDamagedFrame", err)
	}
	if len(matches) != 0 {
		t.Errorf("got %d matches, want 0: %+v", len(matches), matches)
	}
	for _, c := range contexts {
		if c.Offset >= int64(line100) && c.Offset < int64(cut) {
			t.Errorf("context line from the cut line: byte %d %q", c.Offset, c.LineText)
		}
	}
}

// incompressibleLog is a log of about size bytes whose line n reads
// "line <n> filler <random hex>", with NEEDLE for filler on every
// hundredth line. The hex keeps its seekable form above the size from
// which a seekable trace is cached.
func incompressibleLog(size int) []byte {
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic fixture, not crypto
	var b strings.Builder
	for line := 1; b.Len() < size; line++ {
		kind := "filler"
		if line%100 == 0 {
			kind = "NEEDLE"
		}
		fmt.Fprintf(&b, "line %d %s %016x%016x%016x%016x\n",
			line, kind, rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64())
	}
	return []byte(b.String())
}

// A trace of a seekable file with a damaged frame answers the matches
// of every line the damage does not touch, before it and after it,
// names the file in skipped_files as not searched in full, and writes
// no trace-cache entry: a second trace scans again and answers the
// same, rather than serving an answer that outlives the damage.
func TestTraceOfDamagedSeekableFileKeepsTheRestAndIsNotCached(t *testing.T) {
	largeFileCacheEnv(t)
	text := incompressibleLog(4 << 20)
	path := writeSeekableZstdFile(t, text, 512<<10)
	const damagedFrame = 2
	seekablefile.DamageFrame(t, path, damagedFrame)
	tbl, err := readSeekTable(pinForTest(t, path))
	if err != nil {
		t.Fatalf("readSeekTable: %v", err)
	}
	if tbl.NumFrames < damagedFrame+3 {
		t.Fatalf("fixture has %d frames, want frames after the damaged one", tbl.NumFrames)
	}
	damage := tbl.Frames[damagedFrame]
	patterns := []string{"NEEDLE"}

	var want []textLine
	for _, line := range linesOf(text) {
		if strings.Contains(line.text, "NEEDLE") && readableAround(line, []seekable.FrameInfo{damage}) {
			want = append(want, line)
		}
	}

	for _, run := range []string{"first trace", "second trace"} {
		resp := traceOnce(t, path, patterns, Options{})
		if !slices.Equal(resp.SkippedFiles, []string{path}) {
			t.Errorf("%s: skipped_files = %v, want the damaged file", run, resp.SkippedFiles)
		}
		if len(resp.Matches) != len(want) {
			t.Fatalf("%s: got %d matches, want %d", run, len(resp.Matches), len(want))
		}
		for i, m := range resp.Matches {
			wantLine := -1 // the damaged frame's line count is lost
			if m.Offset < damage.DecompressedOffset {
				wantLine = want[i].number
			}
			if m.Offset != want[i].offset || m.AbsoluteLineNumber != wantLine {
				t.Fatalf("%s: match %d at byte %d line %d, want byte %d line %d",
					run, i, m.Offset, m.AbsoluteLineNumber, want[i].offset, wantLine)
			}
		}
		if _, err := LoadCache(CachePath(path, patterns, nil)); err == nil {
			t.Fatalf("%s: a trace-cache entry was written for a scan around a damaged frame", run)
		}
	}
}

// encodeSeekableBytes returns text as rx writes it in seekable form, in
// frames of about frameSize bytes.
func encodeSeekableBytes(t *testing.T, text []byte, frameSize int) []byte {
	t.Helper()
	data, err := os.ReadFile(writeSeekableZstdFile(t, text, frameSize))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// A .zst whose seek table does not describe it is read as the plain
// zstd stream it still is, so a trace answers what the text holds: as
// one stream (file_chunks 1), not skipped. Two seekable files joined
// with `cat` end with the second file's table alone, which used to be
// read as the table of the whole file, and a damaged table placed the
// frames wrongly; both answered matches the text does not hold, or
// none.
func TestTraceReadsAZstWhoseSeekTableDoesNotDescribeItAsPlainZstd(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	first, second := numberedLog(2000, 37), numberedLog(2500, 41)
	text := append(bytes.Clone(first), second...)
	valid := encodeSeekableBytes(t, text, 8<<10)
	frames := int(binary.LittleEndian.Uint32(valid[len(valid)-5:]))
	entry := len(valid) - seekable.FooterSize - frames*seekable.EntrySize

	files := map[string][]byte{
		"two seekable files joined": append(encodeSeekableBytes(t, first, 8<<10), encodeSeekableBytes(t, second, 8<<10)...),
		"a damaged seek table": func() []byte {
			damaged := bytes.Clone(valid)
			damaged[entry+3*seekable.EntrySize] ^= 0x10 // a compressed size
			return damaged
		}(),
	}
	var want []textLine
	for _, line := range linesOf(text) {
		if strings.Contains(line.text, "NEEDLE") {
			want = append(want, line)
		}
	}
	for name, data := range files {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "joined.log.zst")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := traceOnce(t, path, []string{"NEEDLE"}, Options{})
			if len(resp.SkippedFiles) != 0 {
				t.Errorf("skipped_files = %v, want none", resp.SkippedFiles)
			}
			if chunks := resp.FileChunks["f1"]; chunks != 1 {
				t.Errorf("file_chunks = %d, want 1 (one plain zstd stream)", chunks)
			}
			if len(resp.Matches) != len(want) {
				t.Fatalf("got %d matches, want %d", len(resp.Matches), len(want))
			}
			for i, m := range resp.Matches {
				if m.Offset != want[i].offset || m.AbsoluteLineNumber != want[i].number || m.LineText == nil || *m.LineText != want[i].text {
					t.Fatalf("match %d: byte %d line %d %q, want byte %d line %d %q", i,
						m.Offset, m.AbsoluteLineNumber, derefText(m.LineText), want[i].offset, want[i].number, want[i].text)
				}
			}
		})
	}
}

// samplesLine asks samples for line n of the file at path, as
// `rx samples --lines=n` does without an index, and returns its text.
func samplesLine(t *testing.T, path string, n int64) (string, error) {
	t.Helper()
	resp, err := samples.Resolve(samples.Request{
		Path:   path,
		Source: pinForTest(t, path),
		Lines:  []samples.OffsetOrRange{{Start: n}},
	})
	if err != nil {
		return "", err
	}
	key := strconv.FormatInt(n, 10)
	if len(resp.Samples[key]) != 1 {
		return "", fmt.Errorf("samples for line %d: %v", n, resp.Samples[key])
	}
	return resp.Samples[key][0], nil
}

// trace, samples and index agree on what of a seekable file can be
// read. With a damaged frame, each answers what lies before it, none
// answers as if the file were whole, and each names the damage: trace
// lists the file in skipped_files (and keeps the rest, see above),
// samples refuses a line past the damage, index refuses the file, both
// with seekable.ErrDamagedFrame. Two seekable files joined with `cat`
// are read as plain zstd by all three, and each answers the text.
func TestTraceSamplesAndIndexAgreeOnADamagedOrJoinedSeekableFile(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	first, second := numberedLog(2000, 37), numberedLog(2500, 41)
	text := append(bytes.Clone(first), second...)
	lines := linesOf(text)

	t.Run("a damaged frame", func(t *testing.T) {
		path := writeSeekableZstdFile(t, text, 16<<10)
		const damagedFrame = 3
		seekablefile.DamageFrame(t, path, damagedFrame)
		tbl, err := readSeekTable(pinForTest(t, path))
		if err != nil {
			t.Fatalf("readSeekTable: %v", err)
		}
		damage := tbl.Frames[damagedFrame]
		var before, after textLine
		for _, line := range lines {
			if line.offset+int64(len(line.text)) < damage.DecompressedOffset {
				before = line
			}
			if after.number == 0 && line.offset > damage.DecompressedEnd() {
				after = line
			}
		}

		resp := traceOnce(t, path, []string{"NEEDLE"}, Options{})
		if !slices.Equal(resp.SkippedFiles, []string{path}) {
			t.Errorf("trace: skipped_files = %v, want the damaged file", resp.SkippedFiles)
		}
		if got, err := samplesLine(t, path, int64(before.number)); err != nil || got != before.text {
			t.Errorf("samples line %d before the damage = %q, %v; want %q", before.number, got, err, before.text)
		}
		if _, err := samplesLine(t, path, int64(after.number)); !errors.Is(err, seekable.ErrDamagedFrame) {
			t.Errorf("samples line %d past the damage: err = %v, want seekable.ErrDamagedFrame", after.number, err)
		}
		if _, err := index.Build(path, index.BuildOptions{}); !errors.Is(err, seekable.ErrDamagedFrame) {
			t.Errorf("index: err = %v, want seekable.ErrDamagedFrame", err)
		}
	})

	t.Run("two seekable files joined", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "joined.log.zst")
		joined := append(encodeSeekableBytes(t, first, 16<<10), encodeSeekableBytes(t, second, 16<<10)...)
		if err := os.WriteFile(path, joined, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp := traceOnce(t, path, []string{"NEEDLE"}, Options{})
		if len(resp.SkippedFiles) != 0 || resp.FileChunks["f1"] != 1 {
			t.Errorf("trace: skipped_files %v, file_chunks %d; want none and 1", resp.SkippedFiles, resp.FileChunks["f1"])
		}
		late := lines[len(lines)-10]
		if got, err := samplesLine(t, path, int64(late.number)); err != nil || got != late.text {
			t.Errorf("samples line %d = %q, %v; want %q", late.number, got, err, late.text)
		}
		idx, err := index.Build(path, index.BuildOptions{})
		if err != nil {
			t.Fatalf("index: %v", err)
		}
		if idx.LineCount == nil || *idx.LineCount != int64(len(lines)) || idx.Frames != nil {
			t.Errorf("index: line_count %v, frames %v; want %d lines and no frame table", idx.LineCount, idx.Frames, len(lines))
		}
	})
}
