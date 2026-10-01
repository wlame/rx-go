package compressfile

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/seekable"
)

// logText is the text every fixture holds: lines that read "LINE <n>",
// so a wrong line or offset is visible at a glance.
func logText(lines int) []byte {
	var b bytes.Buffer
	for n := 1; n <= lines; n++ {
		fmt.Fprintf(&b, "LINE %d level=info message=request served in %d ms\n", n, n%97)
	}
	return b.Bytes()
}

// compressedCopy encodes text in one format. It returns nil when the
// format needs a tool the host lacks (bzip2: Go only decodes it).
func compressedCopy(t *testing.T, format string, text []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch format {
	case "gzip":
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(text)
		if err := w.Close(); err != nil {
			t.Fatalf("gzip: %v", err)
		}
	case "xz":
		w, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatalf("xz writer: %v", err)
		}
		_, _ = w.Write(text)
		if err := w.Close(); err != nil {
			t.Fatalf("xz: %v", err)
		}
	case "zstd":
		w, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		buf.Write(w.EncodeAll(text, nil))
		_ = w.Close()
	case "seekable zstd":
		enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 1024})
		if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &buf); err != nil {
			t.Fatalf("seekable: %v", err)
		}
	case "bzip2":
		if _, err := exec.LookPath("bzip2"); err != nil {
			return nil
		}
		cmd := exec.Command("bzip2", "-c")
		cmd.Stdin = bytes.NewReader(text)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("bzip2: %v", err)
		}
		buf.Write(out)
	default:
		t.Fatalf("unknown format %q", format)
	}
	return buf.Bytes()
}

// decompressedText reads a seekable zstd file back as text.
func decompressedText(t *testing.T, path string) []byte {
	t.Helper()
	if !seekable.IsSeekable(path) {
		t.Fatalf("%s is not a seekable zstd file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	r, err := compression.NewReader(f, compression.FormatSeekableZstd)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = r.Close() }()
	text, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	return text
}

func TestCompress_WritesTheTextOfACompressedInput(t *testing.T) {
	t.Parallel()
	text := logText(3000)
	cases := []struct {
		format string
		name   string
		want   compression.Format
	}{
		{"gzip", "app.log.gz", compression.FormatGzip},
		{"bzip2", "app.log.bz2", compression.FormatBz2},
		{"xz", "app.log.xz", compression.FormatXz},
		{"zstd", "app.log.zst", compression.FormatZstd},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			t.Parallel()
			body := compressedCopy(t, tc.format, text)
			if body == nil {
				t.Skipf("no %s binary on PATH to write the fixture", tc.format)
			}
			dir := t.TempDir()
			input := filepath.Join(dir, tc.name)
			if err := os.WriteFile(input, body, 0o600); err != nil {
				t.Fatalf("write input: %v", err)
			}
			output := filepath.Join(dir, "out.zst")

			result, err := Compress(context.Background(), Options{
				InputPath: input, OutputPath: output, FrameSize: 4096, Level: 3, Workers: 2,
			})
			if err != nil {
				t.Fatalf("Compress: %v", err)
			}

			if got := decompressedText(t, output); !bytes.Equal(got, text) {
				t.Errorf("output text differs from the input's text (%d vs %d bytes)", len(got), len(text))
			}
			if result.InputFormat != tc.want {
				t.Errorf("input format: got %q, want %q", result.InputFormat, tc.want)
			}
			if result.DecompressedSize != int64(len(text)) {
				t.Errorf("decompressed size: got %d, want %d", result.DecompressedSize, len(text))
			}
			if result.FrameCount < 2 {
				t.Errorf("frame count: got %d, want several", result.FrameCount)
			}
		})
	}
}

func TestCompress_PlainInputIsEncodedAsItIs(t *testing.T) {
	t.Parallel()
	text := logText(2000)
	dir := t.TempDir()
	input := filepath.Join(dir, "app.log")
	if err := os.WriteFile(input, text, 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	output := input + ".zst"

	result, err := Compress(context.Background(), Options{InputPath: input, OutputPath: output, FrameSize: 4096})
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if got := decompressedText(t, output); !bytes.Equal(got, text) {
		t.Error("output text differs from the input")
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if result.CompressedSize != info.Size() {
		t.Errorf("compressed size: got %d, want the output's %d", result.CompressedSize, info.Size())
	}
	if result.InputFormat != compression.FormatNone {
		t.Errorf("input format: got %q, want none", result.InputFormat)
	}
}

func TestCompress_SeekableInputIsRefusedUnlessReencodingIsAsked(t *testing.T) {
	t.Parallel()
	text := logText(2000)
	dir := t.TempDir()
	input := filepath.Join(dir, "app.log.zst")
	if err := os.WriteFile(input, compressedCopy(t, "seekable zstd", text), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	output := filepath.Join(dir, "reframed.zst")

	_, err := Compress(context.Background(), Options{InputPath: input, OutputPath: output})
	if !errors.Is(err, ErrAlreadySeekable) {
		t.Fatalf("got %v, want ErrAlreadySeekable", err)
	}
	if _, statErr := os.Stat(output); statErr == nil {
		t.Error("a refused input still created the output")
	}

	result, err := Compress(context.Background(), Options{
		InputPath: input, OutputPath: output, FrameSize: 8192, ReencodeSeekable: true,
	})
	if err != nil {
		t.Fatalf("Compress with re-encoding: %v", err)
	}
	if got := decompressedText(t, output); !bytes.Equal(got, text) {
		t.Error("re-encoded text differs from the input's text")
	}
	if result.DecompressedSize != int64(len(text)) {
		t.Errorf("decompressed size: got %d, want %d", result.DecompressedSize, len(text))
	}
}

func TestCheck_RefusesWhatCannotBeCompressed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plain := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, logText(10), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(plain, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	archive := filepath.Join(dir, "logs.tar.gz")
	if err := os.WriteFile(archive, compressedCopy(t, "gzip", []byte("not a tar")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cases := []struct {
		name          string
		input, output string
		want          error
	}{
		{"compound archive", archive, archive + ".zst", ErrCompoundArchive},
		{"output is the input", plain, plain, ErrOutputIsInput},
		{"output is the input by another name", plain, link, ErrOutputIsInput},
		{"plain input, new output", plain, plain + ".zst", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Check(tc.input, tc.output, false)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCompress_CorruptInputFailsWithTheDecoderError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	input := filepath.Join(dir, "app.log.gz")
	body := compressedCopy(t, "gzip", logText(2000))
	// Cut the stream short: the gzip reader reports an unexpected EOF.
	if err := os.WriteFile(input, body[:len(body)/2], 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}

	_, err := Compress(context.Background(), Options{InputPath: input, OutputPath: input + ".zst"})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want the decoder's unexpected EOF", err)
	}
}
