package compression

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"testing"

	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/testutil/xzfile"
)

// numberedText is a log of n lines, each naming its own number.
func numberedText(n int) []byte {
	var b bytes.Buffer
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "LINE %d 2025-12-10 07:00:00.000 INFO request served in %d ms\n", i, i%997)
	}
	return b.Bytes()
}

// readAllXz decodes body through an xz reader whose dictionary limit is
// dictionaryLimit, and returns the text and the first error.
func readAllXz(body []byte, dictionaryLimit uint64) ([]byte, error) {
	r, err := NewXzReader(bytes.NewReader(body), dictionaryLimit)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// allocatedBy returns how many bytes of heap f allocates. The tests
// that call it do not run in parallel, so the count is f's own.
func allocatedBy(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The reader decodes every shape of file the xz writer makes: one block
// or many, each check type, streams joined end to end, and the zero
// padding allowed between and after streams.
func TestNewXzReader_DecodesWhatTheXzWriterWrote(t *testing.T) {
	text := numberedText(20_000) // about 1.3 MB
	oneBlock := xzfile.Encode(t, text, xz.WriterConfig{})
	padding := make([]byte, 8)
	cases := map[string]struct {
		body []byte
		want []byte
	}{
		"one block":       {oneBlock, text},
		"many blocks":     {xzfile.Encode(t, text, xz.WriterConfig{BlockSize: 64 << 10}), text},
		"no check":        {xzfile.Encode(t, text, xz.WriterConfig{NoCheckSum: true}), text},
		"crc32 check":     {xzfile.Encode(t, text, xz.WriterConfig{CheckSum: xz.CRC32}), text},
		"sha256 check":    {xzfile.Encode(t, text, xz.WriterConfig{CheckSum: xz.SHA256}), text},
		"empty text":      {xzfile.Encode(t, nil, xz.WriterConfig{}), nil},
		"two streams":     {append(bytes.Clone(oneBlock), oneBlock...), append(bytes.Clone(text), text...)},
		"stream padding":  {bytes.Join([][]byte{oneBlock, padding, oneBlock, padding}, nil), append(bytes.Clone(text), text...)},
		"a 64-byte file":  {xzfile.Encode(t, []byte("hello\n"), xz.WriterConfig{}), []byte("hello\n")},
		"an empty stream": {append(xzfile.Encode(t, nil, xz.WriterConfig{}), oneBlock...), text},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := readAllXz(tc.body, WindowLimit)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("decoded %d bytes; want the %d bytes written", len(got), len(tc.want))
			}
		})
	}
}

// The reader decodes what the xz tool writes, at its presets, with
// several blocks (each of whose headers then holds both size fields),
// and with each check type.
func TestNewXzReader_DecodesWhatTheXzToolWrote(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz is not installed")
	}
	text := numberedText(30_000) // about 2 MB
	argsByName := map[string][]string{
		"-0":                {"-0"},
		"-6":                {"-6"},
		"-9":                {"-9"},
		"-9e":               {"-9e"},
		"blocks of 256 KiB": {"-6", "-T4", "--block-size=262144"},
		"check none":        {"--check=none"},
		"check crc32":       {"--check=crc32"},
		"check crc64":       {"--check=crc64"},
		"check sha256":      {"--check=sha256"},
	}
	for name, args := range argsByName {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("xz", append([]string{"-c"}, args...)...)
			cmd.Stdin = bytes.NewReader(text)
			body, err := cmd.Output()
			if err != nil {
				t.Fatalf("xz %v: %v", args, err)
			}
			got, err := readAllXz(body, WindowLimit)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !bytes.Equal(got, text) {
				t.Errorf("decoded %d bytes; want the %d bytes written", len(got), len(text))
			}
		})
	}
}

// A block header that declares a dictionary above the limit is refused
// before the decoder reserves it, whichever block declares it: the
// reader then costs the few kilobytes it holds, never the gigabytes the
// header asks for. The text of the blocks before it comes out first.
func TestNewXzReader_RefusesADictionaryAboveTheLimitBeforeReservingIt(t *testing.T) {
	small := xzfile.Encode(t, []byte("hello\n"), xz.WriterConfig{})
	text := numberedText(4_000)
	twoBlocks := xzfile.Encode(t, text, xz.WriterConfig{BlockSize: 128 << 10})
	if len(xzfile.BlockHeaders(twoBlocks)) < 2 {
		t.Fatalf("fixture has %d blocks; want two or more", len(xzfile.BlockHeaders(twoBlocks)))
	}
	cases := map[string]struct {
		body     []byte
		wantText []byte // what comes out before the refusal
	}{
		"192 MiB":                   {xzfile.WithDictionaryCode(t, small, 0, 31), nil},
		"1 GiB":                     {xzfile.WithDictionaryCode(t, small, 0, 36), nil},
		"2 GiB":                     {xzfile.WithDictionaryCode(t, small, 0, 38), nil},
		"4 GiB":                     {xzfile.WithDictionaryCode(t, small, 0, 40), nil},
		"2 GiB in the second block": {xzfile.WithDictionaryCode(t, twoBlocks, 1, 38), text[:128<<10]},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got []byte
			var err error
			allocated := allocatedBy(func() { got, err = readAllXz(tc.body, WindowLimit) })
			t.Logf("refused after %d KiB allocated: %v", allocated>>10, err)
			if !errors.Is(err, ErrDictionaryTooLarge) {
				t.Errorf("err = %v; want ErrDictionaryTooLarge", err)
			}
			if !errors.Is(err, ErrTooLargeToDecode) {
				t.Errorf("err = %v; want it to wrap ErrTooLargeToDecode", err)
			}
			if !bytes.Equal(got, tc.wantText) {
				t.Errorf("got %d bytes before the refusal; want %d", len(got), len(tc.wantText))
			}
			// The first block of the two-block file holds its own 8 MiB
			// dictionary; the refused one would have held 2 GiB.
			const budget = 12 << 20
			if allocated > budget {
				t.Errorf("allocated %d MiB; budget %d MiB", allocated>>20, budget>>20)
			}
		})
	}
}

// NewReader, which every command reads an xz file through, applies
// WindowLimit: an `xz -9` file (64 MiB) decodes, a 2 GiB one is
// refused.
func TestNewReader_AppliesTheDictionaryLimitToXz(t *testing.T) {
	small := xzfile.Encode(t, []byte("hello\n"), xz.WriterConfig{})
	r, err := NewReader(io.NopCloser(bytes.NewReader(xzfile.WithDictionaryCode(t, small, 0, 38))), FormatXz)
	if err == nil {
		_, err = io.ReadAll(r)
		_ = r.Close()
	}
	if !errors.Is(err, ErrDictionaryTooLarge) {
		t.Errorf("2 GiB dictionary: err = %v; want ErrDictionaryTooLarge", err)
	}

	sixtyFour := xzfile.WithDictionaryCode(t, small, 0, 28) // 64 MiB, what xz -9 declares
	r, err = NewReader(io.NopCloser(bytes.NewReader(sixtyFour)), FormatXz)
	if err != nil {
		t.Fatalf("64 MiB dictionary: NewReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "hello\n" {
		t.Errorf("64 MiB dictionary: got %q, %v; want \"hello\\n\"", got, err)
	}
}

// The limit is inclusive: a dictionary of exactly the limit decodes, the
// next size up is refused.
func TestNewXzReader_AcceptsADictionaryOfExactlyTheLimit(t *testing.T) {
	text := numberedText(1_000) // under 1 MiB, so a 1 MiB dictionary holds it all
	body := xzfile.Encode(t, text, xz.WriterConfig{})
	got, err := readAllXz(xzfile.WithDictionaryCode(t, body, 0, 16), 1<<20)
	if err != nil || !bytes.Equal(got, text) {
		t.Errorf("1 MiB dictionary, 1 MiB limit: %d bytes, %v; want the text", len(got), err)
	}
	_, err = readAllXz(xzfile.WithDictionaryCode(t, body, 0, 17), 1<<20)
	if !errors.Is(err, ErrDictionaryTooLarge) {
		t.Errorf("1.5 MiB dictionary, 1 MiB limit: err = %v; want ErrDictionaryTooLarge", err)
	}
}

// Every byte of an xz file is checked by something: a CRC32, the block's
// check, the index or the format. Changing any one byte, or cutting the
// file short anywhere, gives an error or the same text, never different
// text, and the reader agrees with the xz package's own reader on
// which, with one exception: that reader takes the end of the file where
// a block header should be for the end of the stream, so it answers a
// file cut before a block header, or one whose header size byte points
// past the end, with the text before it as if it were all of it. This
// reader reports io.ErrUnexpectedEOF.
func TestNewXzReader_AgreesWithTheXzPackageOnEveryDamagedByte(t *testing.T) {
	text := numberedText(60)
	body := xzfile.Encode(t, text, xz.WriterConfig{BlockSize: 1 << 10})
	theirRead := func(damaged []byte) ([]byte, error) {
		theirs, err := xz.NewReader(bytes.NewReader(damaged))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(theirs)
	}
	for i := range body {
		damaged := bytes.Clone(body)
		damaged[i] ^= 0x55
		got, err := readAllXz(damaged, WindowLimit)
		if err == nil && !bytes.Equal(got, text) {
			t.Fatalf("byte %d flipped: decoded different text without an error", i)
		}
		if !errors.Is(err, ErrDictionaryTooLarge) { // else the xz package would reserve it
			_, theirErr := theirRead(damaged)
			// A flipped header size or index indicator can send a reader
			// past the end of the file, which the xz package takes for
			// the end of the stream.
			endedEarly := errors.Is(err, io.ErrUnexpectedEOF) && theirErr == nil
			if (err == nil) != (theirErr == nil) && !endedEarly {
				t.Fatalf("byte %d flipped: rx err = %v, xz package err = %v", i, err, theirErr)
			}
		}

		cut := body[:i]
		if _, err := readAllXz(cut, WindowLimit); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut at %d: err = %v; want io.ErrUnexpectedEOF", i, err)
		}
	}
}

// FuzzNewXzReader feeds arbitrary bytes to the reader with a 1 MiB
// dictionary limit. It must never panic, and a file it decodes without
// an error must decode the same through the xz package.
func FuzzNewXzReader(f *testing.F) {
	text := numberedText(200)
	f.Add(xzfile.Encode(f, text, xz.WriterConfig{}))
	f.Add(xzfile.Encode(f, text, xz.WriterConfig{BlockSize: 2 << 10, CheckSum: xz.SHA256}))
	f.Add(xzfile.Encode(f, nil, xz.WriterConfig{NoCheckSum: true}))
	f.Fuzz(func(t *testing.T, body []byte) {
		r, err := NewXzReader(bytes.NewReader(body), 1<<20)
		if err != nil {
			return
		}
		var got bytes.Buffer
		if _, err := io.CopyN(&got, r, 16<<20); err != nil && !errors.Is(err, io.EOF) {
			return
		}
		theirs, err := xz.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("rx decoded %d bytes; the xz package refuses the file: %v", got.Len(), err)
		}
		want, err := io.ReadAll(io.LimitReader(theirs, 16<<20))
		if err != nil {
			t.Fatalf("rx decoded %d bytes; the xz package fails: %v", got.Len(), err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("rx decoded %d bytes, the xz package %d", got.Len(), len(want))
		}
	})
}
