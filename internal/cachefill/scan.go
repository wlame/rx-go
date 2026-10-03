package cachefill

import (
	"context"
	"errors"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/trace"
)

// ScanFunc is the signature scan of one file: findings.Scan, or a
// stand-in a test gives to hold a scan while it cancels it.
type ScanFunc func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue,
	sel []*signatures.Signature, opts findings.ScanOptions) (*findings.Answer, error)

// ScanSettings are the settings of a scan run.
type ScanSettings struct {
	// Catalogue is the loaded catalogue and Selection the signatures to
	// scan for, resolved from it.
	Catalogue *signatures.Catalogue
	Selection []*signatures.Signature
	// Options are the scan's and the cache's settings, read from the
	// environment by the command or the server (FindingsOptionsFromEnv),
	// with the masking mode and Force. Options.Progress, when set, is
	// called as each file is read.
	Options findings.ScanOptions
	// Scan is the scan; nil is findings.Scan.
	Scan ScanFunc
}

// ScanFile scans the entry's file for the selected signatures and caches
// the findings, exactly as POST /v1/findings scans one file, or decides
// not to:
//
//   - the file is pinned and classified as a search classifies it
//     (trace.ClassifyForSearch); one that cannot be is unreachable (a
//     skip when walked, a failure when named), and one whose text is
//     not text is skipped with the reason;
//   - findings.Scan answers every selected signature with a current
//     cache entry from the cache and scans the others. Without Force,
//     a file whose every selected signature was current is Current: it
//     was not read through a scan. Otherwise it is Filled;
//   - a file rx refuses to decompress is skipped with the reason a
//     search gives; any other scan error fails the file.
//
// The scan waits for a place in the scan queue (findings.SetMaxScans)
// and stops when ctx is canceled, inside the file too; a scan stopped
// that way writes nothing, and the result is Failed with ctx's error.
func ScanFile(ctx context.Context, e Entry, s ScanSettings) FileResult {
	src, err := paths.Pin(e.Path)
	if err != nil {
		return unreachable(e, err)
	}
	if !src.Info().Mode().IsRegular() {
		return unreachable(e, errNotRegular(e.Path))
	}
	file, err := trace.ClassifyForSearch(src)
	switch {
	case errors.Is(err, compression.ErrTooLargeToDecode):
		return skipped(e.Path, compression.TooLargeToDecodeReason)
	case err != nil && !e.Named:
		// The wording a search gives the same file (trace.SkipReason).
		return skipped(e.Path, trace.SkipReason(err))
	case err != nil:
		return unreachable(e, err)
	}
	if !file.Kind().IsText() {
		return skipped(e.Path, file.Kind().NotText)
	}
	scan := s.Scan
	if scan == nil {
		scan = findings.Scan
	}
	answer, err := scan(ctx, file, s.Catalogue, s.Selection, s.Options)
	if errors.Is(err, compression.ErrTooLargeToDecode) {
		return skipped(e.Path, compression.TooLargeToDecodeReason)
	}
	if err != nil {
		return failed(e.Path, err)
	}
	// Checked is every selected signature, once each, for a scan.
	outcome := Filled
	if !s.Options.Force && answer.Cached == len(answer.Checked) {
		outcome = Current
	}
	return FileResult{Path: e.Path, Outcome: outcome, Answer: answer}
}
