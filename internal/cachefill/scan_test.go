package cachefill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/trace"
)

// A first scan fills the findings cache, whose read then answers the
// same findings; a second scan finds every selected signature current
// and reads no file through a scan (Progress is never called); force
// scans it again.
func TestScanFile_FillsTheCacheThenFindsItCurrent(t *testing.T) {
	cat := loadTestCatalogue(t)
	settings := scanSettings(t, cat, "default")
	log := filepath.Join(folder(t), "app.log")
	ctx := context.Background()

	read := 0
	settings.Options.Progress = func(int64, int64) { read++ }
	first := ScanFile(ctx, walked(t, log), settings)
	if first.Outcome != Filled || first.Answer == nil || first.Answer.State != findings.StateFull {
		t.Fatalf("first scan: %+v", first)
	}
	if got := first.Answer.BySignature["err-line"].Count; got != 2 {
		t.Errorf("err-line count %d, want 2", got)
	}
	if read == 0 {
		t.Error("the first scan reported no bytes read")
	}
	file, err := trace.ClassifyForSearch(pin(t, log))
	if err != nil {
		t.Fatal(err)
	}
	cached, err := findings.Read(file, cat, settings.Selection, settings.Options)
	if err != nil || cached.State != findings.StateFull || len(cached.Missing) != 0 {
		t.Fatalf("the cache after the scan: %+v %v", cached, err)
	}

	read = 0
	again := ScanFile(ctx, walked(t, log), settings)
	if again.Outcome != Current || again.Answer == nil || again.Answer.Cached != len(settings.Selection) || read != 0 {
		t.Errorf("second scan: %+v, %d progress calls", again, read)
	}
	settings.Options.Force = true
	if forced := ScanFile(ctx, walked(t, log), settings); forced.Outcome != Filled || read == 0 {
		t.Errorf("forced scan: %+v, %d progress calls", forced, read)
	}
}

// A file whose text is not text is a skip with the reason; a file that
// cannot be reached is a skip when a walk found it and a failure when
// the caller named it.
func TestScanFile_SkipsWhatTheSearchSkips(t *testing.T) {
	cat := loadTestCatalogue(t)
	settings := scanSettings(t, cat, "default")
	dir := folder(t)
	ctx := context.Background()
	if got := ScanFile(ctx, walked(t, filepath.Join(dir, "blob.bin")), settings); got.Outcome != Skipped ||
		!strings.HasPrefix(got.Reason, filekind.NotTextPrefix) {
		t.Errorf("binary: %+v", got)
	}
	gone := filepath.Join(dir, "gone.log")
	if got := ScanFile(ctx, Entry{Path: gone}, settings); got.Outcome != Skipped ||
		got.Reason != paths.FailureReason(os.ErrNotExist) {
		t.Errorf("walked, gone: %+v", got)
	}
	if got := ScanFile(ctx, Entry{Path: gone, Named: true}, settings); got.Outcome != Failed || !got.Unreadable {
		t.Errorf("named, gone: %+v", got)
	}
}

// A scan that fails fails the file with its error; one that meets a
// file rx refuses to decompress skips it with the reason a search
// gives; a canceled scan reports the cancel.
func TestScanFile_FailuresAndRefusals(t *testing.T) {
	cat := loadTestCatalogue(t)
	log := filepath.Join(folder(t), "app.log")
	for _, c := range []struct {
		name    string
		err     error
		outcome Outcome
		reason  string
	}{
		{"a failing scan", errors.New("app.log: the scan broke"), Failed, "app.log: the scan broke"},
		{"too large to decode", compression.ErrTooLargeToDecode, Skipped, compression.TooLargeToDecodeReason},
		{"canceled", context.Canceled, Failed, context.Canceled.Error()},
	} {
		settings := scanSettings(t, cat, "default")
		settings.Scan = func(context.Context, trace.SearchFile, *signatures.Catalogue, []*signatures.Signature,
			findings.ScanOptions) (*findings.Answer, error) {
			return nil, c.err
		}
		got := ScanFile(context.Background(), walked(t, log), settings)
		if got.Outcome != c.outcome || got.Reason != c.reason || got.Unreadable {
			t.Errorf("%s: %+v", c.name, got)
		}
		if c.outcome == Failed && !errors.Is(got.Err, c.err) {
			t.Errorf("%s: error %v", c.name, got.Err)
		}
	}
}
