package filekind

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// numberedLog is n lines that read `LINE <i> …`, about 40 bytes each.
func numberedLog(n int) []byte {
	var buf bytes.Buffer
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&buf, "LINE %d request served in %d ms\n", i, i%311)
	}
	return buf.Bytes()
}

// ReadTextHead gives the first bytes of the file's text, whatever the
// file is stored in: the bytes of a plain file, the decompressed stream
// of a compressed one. A text shorter than the limit comes back whole.
func TestReadTextHead_ReturnsTheStartOfTheText(t *testing.T) {
	text := numberedLog(2000)
	stored := map[string][]byte{
		"plain":    text,
		"gzip":     gzipOf(t, text),
		"zstd":     zstdOf(t, text),
		"seekable": seekableOf(t, text),
	}
	for name, body := range stored {
		for _, limit := range []int{0, 1, 100, len(text), len(text) + 100} {
			t.Run(fmt.Sprintf("%s limit %d", name, limit), func(t *testing.T) {
				r := bytes.NewReader(body)
				kind := Of(r, int64(len(body)))
				head, err := ReadTextHead(r, int64(len(body)), kind, limit)
				if err != nil {
					t.Fatalf("ReadTextHead: %v", err)
				}
				want := text[:min(limit, len(text))]
				if !bytes.Equal(head, want) {
					t.Errorf("head is %d bytes, want the first %d bytes of the text", len(head), len(want))
				}
			})
		}
	}
}

// A stream that breaks inside the head is an error, not a short head:
// a reader that took the bytes before the break for the whole text
// would describe a file that does not exist.
func TestReadTextHead_ReportsADamagedStream(t *testing.T) {
	text := numberedLog(2000)
	body := gzipOf(t, text)
	cut := body[:len(body)/2]
	r := bytes.NewReader(cut)
	kind := Of(r, int64(len(cut)))
	head, err := ReadTextHead(r, int64(len(cut)), kind, len(text))
	if err == nil {
		t.Fatalf("ReadTextHead of a cut gzip stream gave %d bytes and no error", len(head))
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("error %v is io.EOF; a cut stream must not read as the end of the text", err)
	}
	if !bytes.HasPrefix(text, head) {
		t.Errorf("the bytes read before the break are not the start of the text")
	}
}

// The head read is bounded: a plain file is read no further than the
// limit, and a compressed one no further than its decoder needs to
// produce that much text.
func TestReadTextHead_ReadsNoMoreThanTheHead(t *testing.T) {
	const limit = 1 << 20
	text := numberedLog(120_000) // about 4.5 MiB
	if len(text) < 4*limit {
		t.Fatalf("fixture is %d bytes; want at least %d", len(text), 4*limit)
	}
	cases := []struct {
		name   string
		body   []byte
		budget int64
	}{
		{"plain", text, limit},
		// A compressed head costs the compressed bytes of about that
		// much text plus the decoder's read-ahead; the whole file is
		// several times more.
		{"gzip", gzipOf(t, text), int64(len(gzipOf(t, text)) / 2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind := Of(bytes.NewReader(tc.body), int64(len(tc.body)))
			counter := counting.NewReaderAt(bytes.NewReader(tc.body))
			head, err := ReadTextHead(counter, int64(len(tc.body)), kind, limit)
			if err != nil {
				t.Fatalf("ReadTextHead: %v", err)
			}
			if len(head) != limit {
				t.Fatalf("head is %d bytes, want %d", len(head), limit)
			}
			if got := counter.Load(); got > tc.budget {
				t.Errorf("read %d bytes of a %d-byte file for a %d-byte head; budget %d",
					got, len(tc.body), limit, tc.budget)
			}
		})
	}
}

// A seekable file is read frame by frame through its seek table, so a
// damaged frame inside the head is reported as seekable.ErrDamagedFrame
// naming the frame, the error trace, samples and index give for it.
func TestReadTextHead_NamesADamagedSeekableFrame(t *testing.T) {
	text := numberedLog(2000)
	path := filepath.Join(t.TempDir(), "app.zst")
	seekablefile.Write(t, path, seekablefile.SplitEvery(text, 4096))
	seekablefile.DamageFrame(t, path, 2)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	r := bytes.NewReader(body)
	kind := Of(r, int64(len(body)))
	if !kind.IsSeekable() {
		t.Fatalf("fixture is %+v; want a seekable file", kind)
	}
	head, err := ReadTextHead(r, int64(len(body)), kind, len(text))
	if !errors.Is(err, seekable.ErrDamagedFrame) {
		t.Fatalf("err = %v; want seekable.ErrDamagedFrame", err)
	}
	if !bytes.Equal(head, text[:2*4096]) {
		t.Errorf("head is %d bytes; want the %d bytes of the two frames before the damage", len(head), 2*4096)
	}
}
