package findings

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// SearchRequest is a trace with signatures: the search of the
// patterns over the paths, then a scan of every file it covered.
type SearchRequest struct {
	// Paths are the paths as the engine sees them; Patterns the
	// patterns, none allowed.
	Paths, Patterns []string
	// Search are the trace engine's options for the search.
	Search trace.Options
	// Spooled lists the temporary files piped stdin was copied to, as
	// they appear in Paths: never read from nor written to the cache.
	Spooled []string
	// Catalogue is the loaded catalogue, and Selection the signatures
	// to scan for.
	Catalogue *signatures.Catalogue
	Selection []*signatures.Signature
	// Findings are the options of the scans and of the findings' text
	// (Trace's).
	Findings TraceOptions
}

// Search runs a trace with signatures, the answer of `rx trace
// --signatures` and of GET /v1/trace with signatures: the search of
// req.Patterns over req.Paths (none when there is no pattern), then a
// scan of every file the search covered for req.Selection, through the
// findings cache (Trace).
//
// The files are the search's own, walked and classified once
// (trace.ExpandPaths), so a finding's file id is the id the answer's
// files map gives the file, and a file that is not text or cannot be
// read is skipped with its reason, once. The answer's time covers the
// search and the scans; its cli_command is left to the caller.
func Search(ctx context.Context, engine *trace.Engine, req SearchRequest) (*rxtypes.TraceResponse, error) {
	start := time.Now()
	// The resolver runs inside RunResolved, after the patterns are
	// checked, as for a search without signatures; the plan it makes is
	// kept for the scan.
	//
	// Go note: the closure assigns the variable plan declared outside
	// it, so once RunResolved has called it (once, before any file is
	// read) plan holds the files the search covered.
	var plan *trace.SearchPlan
	resolve := func(context.Context) (*trace.SearchPlan, error) {
		plan = trace.ExpandPaths(req.Paths, !req.Search.NoRecursive)
		return plan, nil
	}
	resp, err := engine.RunResolved(ctx, resolve, req.Patterns, req.Search)
	if err != nil {
		return nil, err
	}

	files := make([]TraceFile, len(plan.Files))
	for i, file := range plan.Files {
		files[i] = TraceFile{
			ID: "f" + strconv.Itoa(i+1), File: file, Spooled: slices.Contains(req.Spooled, file.Path()),
		}
	}
	answer, err := Trace(ctx, files, req.Catalogue, req.Selection, req.Findings)
	if err != nil {
		return nil, err
	}
	resp.Signatures = answer.Signatures
	resp.Findings = answer.Findings
	resp.FindingSummary = answer.FindingSummary
	addSkips(resp, answer.Skipped)
	resp.Time = time.Since(start).Seconds()
	return resp, nil
}

// addSkips appends to the answer's skipped files the files the scan
// passed over, each path once: a file the search already lists keeps
// the search's reason.
func addSkips(resp *rxtypes.TraceResponse, skipped []rxtypes.SkippedFile) {
	for _, item := range skipped {
		if slices.Contains(resp.SkippedFiles, item.Path) {
			continue
		}
		resp.SkippedFiles = append(resp.SkippedFiles, item.Path)
		resp.SkipReasons = append(resp.SkipReasons, item)
	}
}
