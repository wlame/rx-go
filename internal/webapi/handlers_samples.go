package webapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// toLegacyOffsets bridges the new samples.OffsetOrRange slice shape to
// the legacy offsetOrRange slice used by the compressed-file path (which
// hasn't been migrated to the shared resolver at v1 of the U rework).
// When the compressed path is ported this helper can be deleted.
func toLegacyOffsets(in []samples.OffsetOrRange) []offsetOrRange {
	out := make([]offsetOrRange, 0, len(in))
	for _, v := range in {
		if v.End == nil {
			out = append(out, offsetOrRange{Start: v.Start})
		} else {
			end := *v.End
			out = append(out, offsetOrRange{Start: v.Start, End: &end})
		}
	}
	return out
}

// samplesInput is the query-string shape for GET /v1/samples.
//
// offsets and lines are mutually exclusive. Exactly one must be set;
// both-set or neither-set returns 400.
//
// context is an alias that sets both before_context and after_context.
// Individual contexts override it.
// Negative one sentinels for "not provided" are used here because huma
// forbids pointer query params. -1 is outside the normal value range
// (context must be >= 0), so it's a safe signal for "default".
type samplesInput struct {
	Path          string `query:"path" required:"true" example:"/var/log/app.log" doc:"File path to read from"`
	Offsets       string `query:"offsets" example:"100,200,300" doc:"Comma-separated byte offsets or ranges"`
	Lines         string `query:"lines" example:"100,200-205,-1" doc:"Comma-separated 1-based line numbers or ranges"`
	Context       int    `query:"context" minimum:"-1" default:"-1" example:"3" doc:"Context lines before AND after each offset (-1 = default 3)"`
	BeforeContext int    `query:"before_context" minimum:"-1" default:"-1" doc:"Context lines before each offset (-1 = default 3)"`
	AfterContext  int    `query:"after_context" minimum:"-1" default:"-1" doc:"Context lines after each offset (-1 = default 3)"`
}

// samplesOutput wraps the SamplesResponse body.
type samplesOutput struct {
	Body rxtypes.SamplesResponse
}

// nilIfNegative returns *int pointing to n when n>=0; returns nil when
// n is the -1 "not provided" sentinel.
func nilIfNegative(n int) *int {
	if n < 0 {
		return nil
	}
	return &n
}

// registerSamplesHandlers mounts GET /v1/samples.
//
// Matches rx-python/src/rx/web.py:1076-1535. The endpoint is the CLI's
// `rx samples` over HTTP: seek to offsets (or line numbers) in a file
// and emit ±context lines of surrounding text.
//
// Compressed files: byte offsets are rejected (400); only line mode is
// supported. This matches Python's behavior because byte offsets in a
// .gz file have no stable meaning after partial decompression.
func registerSamplesHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "samples",
		Method:      http.MethodGet,
		Path:        "/v1/samples",
		Summary:     "Get context lines around byte offsets or line numbers",
		Description: "Use this endpoint to view actual content around matches from /v1/trace.",
		Tags:        []string{"Context"},
	}, func(_ context.Context, in *samplesInput) (*samplesOutput, error) {
		if s.cfg.RipgrepPath == "" {
			return nil, ErrServiceUnavailable("ripgrep is not available on this system")
		}

		// Sandbox.
		validated, err := paths.ValidatePathWithinRoots(in.Path)
		if err != nil {
			var perr *paths.ErrPathOutsideRoots
			if errors.As(err, &perr) {
				return nil, NewSandboxError(perr)
			}
			if !errors.Is(err, paths.ErrNoSearchRootsConfigured) {
				return nil, ErrForbidden(err.Error())
			}
			validated = in.Path
		}

		// Mutual exclusion on offsets/lines.
		if in.Offsets != "" && in.Lines != "" {
			return nil, ErrBadRequest("Cannot use both 'offsets' and 'lines'. Provide only one.")
		}
		if in.Offsets == "" && in.Lines == "" {
			return nil, ErrBadRequest("Must provide either 'offsets' or 'lines' parameter.")
		}

		// Compression check.
		compressed := compression.IsCompressed(validated)
		compFmt := compression.FormatNone
		if compressed {
			compFmt, _ = compression.DetectFromPath(validated)
		}
		if compressed && in.Offsets != "" {
			return nil, ErrBadRequest(
				"Byte offsets are not supported for compressed files. Use 'lines' parameter instead.",
			)
		}

		// Context defaults (-1 sentinel = "not provided").
		defaultCtx := 3
		before := defaultCtx
		after := defaultCtx
		if in.Context >= 0 {
			before = in.Context
			after = in.Context
		}
		if in.BeforeContext >= 0 {
			before = in.BeforeContext
		}
		if in.AfterContext >= 0 {
			after = in.AfterContext
		}

		// Parse the offsets/lines spec via the shared parser. Stage 9
		// Round 2 U rework: both CLI and HTTP delegate to
		// internal/samples.ParseCSV so the range syntax
		// (100,200-300,-5) stays identical across entry points.
		var (
			parsedOffsets []samples.OffsetOrRange
			parsedLines   []samples.OffsetOrRange
		)
		if in.Offsets != "" {
			parsedOffsets, err = samples.ParseCSV(in.Offsets)
			if err != nil {
				return nil, ErrBadRequest(fmt.Sprintf("Invalid offsets format: %s", err.Error()))
			}
		} else {
			parsedLines, err = samples.ParseCSV(in.Lines)
			if err != nil {
				return nil, ErrBadRequest(fmt.Sprintf("Invalid lines format: %s", err.Error()))
			}
		}

		// File existence / type check.
		stat, err := os.Stat(validated)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, ErrNotFound(fmt.Sprintf("File not found: %s", validated))
			}
			return nil, ErrForbidden(err.Error())
		}
		if stat.IsDir() {
			return nil, ErrBadRequest(fmt.Sprintf("Path is a directory, not a file: %s", validated))
		}

		// Compressed-file path still lives in the HTTP handler because
		// it streams through the decompressor; the shared resolver is
		// for uncompressed uncompressed files only at v1.
		var resp *rxtypes.SamplesResponse
		if compressed {
			// Only line mode for compressed files. Seeding a stub
			// response so the caller can populate the compressed-format
			// scan output the same way as before the U rework.
			resp = &rxtypes.SamplesResponse{
				Path:          validated,
				Offsets:       map[string]int64{},
				Lines:         map[string]int64{},
				BeforeContext: before,
				AfterContext:  after,
				Samples:       map[string][]string{},
				IsCompressed:  compressed,
			}
			// Bridge parsedLines → the existing legacy offsetOrRange
			// slice expected by sampleCompressedByLine until that path
			// is also migrated to the shared resolver.
			legacy := toLegacyOffsets(parsedLines)
			if compErr := sampleCompressedByLine(validated, compFmt, legacy, before, after, resp); compErr != nil {
				return nil, ErrInternal(compErr.Error())
			}
			fs := string(compFmt)
			resp.CompressionFormat = &fs
		} else {
			// Shared resolver for uncompressed byte-offset AND line mode.
			loader := func(path string) (*rxtypes.UnifiedFileIndex, error) {
				idx, loadErr := index.LoadForSource(path)
				if loadErr != nil {
					if errors.Is(loadErr, index.ErrIndexNotFound) {
						return nil, nil
					}
					return nil, loadErr
				}
				return idx, nil
			}
			req := samples.Request{
				Path:          validated,
				Offsets:       parsedOffsets,
				Lines:         parsedLines,
				BeforeContext: before,
				AfterContext:  after,
				IndexLoader:   loader,
			}
			resp, err = samples.Resolve(req)
			if err != nil {
				return nil, ErrInternal(err.Error())
			}
			resp.IsCompressed = false
			resp.CompressionFormat = nil
		}

		// cli_command equivalent. Use resolved values (after defaults).
		// CLICommand is *string per Stage 9 Round 2 S2 rule (null vs
		// empty string distinction); &cli wraps the builder output.
		cli := BuildCLICommand("samples", map[string]any{
			"path":           validated,
			"offsets":        in.Offsets,
			"lines":          in.Lines,
			"context":        nilIfNegative(in.Context),
			"before_context": nilIfNegative(in.BeforeContext),
			"after_context":  nilIfNegative(in.AfterContext),
		})
		resp.CLICommand = &cli

		return &samplesOutput{Body: *resp}, nil
	})
}

// ============================================================================
// Legacy parsing types (compressed-file path only)
// ============================================================================

// offsetOrRange is the bridge type for the compressed-file sampling
// path, which at v1 of the Stage 9 Round 2 U rework has NOT been
// migrated to the shared internal/samples resolver (streams through
// a decompressor). toLegacyOffsets above converts from the shared
// samples.OffsetOrRange slice into this type.
type offsetOrRange struct {
	Start int64
	End   *int64 // nil = single value
}

// ============================================================================
// Compressed-file sampling (line mode only)
// ============================================================================

// sampleCompressedByLine streams the decompressed content and captures
// lines matching requested line numbers / ranges. No index is consulted;
// the compressed files supported here (gzip/xz/bz2) don't have random
// access, so a streaming scan is the simplest correct approach.
//
// For seekable zstd files, a full frame-indexed fast path is a follow-up;
// M5's scope is feature parity over raw performance.
func sampleCompressedByLine(path string, format compression.Format, values []offsetOrRange, before, after int, out *rxtypes.SamplesResponse) error {
	// Build the set of line numbers to keep.
	wantRange := map[int64][2]int64{} // rangeKey start → [start, end]

	needTotalLines := false
	for _, v := range values {
		if v.End == nil && v.Start < 0 {
			needTotalLines = true
			break
		}
	}

	// For compressed files we don't know totalLines without streaming
	// once. If any negative line is requested, pay the cost.
	var totalLines int64
	if needTotalLines {
		n, err := streamCountLines(path, format)
		if err != nil {
			return err
		}
		totalLines = n
	}

	// Resolve negative single values.
	resolved := make([]offsetOrRange, len(values))
	for i, v := range values {
		if v.End == nil && v.Start < 0 {
			start := totalLines + v.Start + 1
			if start < 1 {
				start = 1
			}
			resolved[i] = offsetOrRange{Start: start}
		} else {
			resolved[i] = v
		}
	}

	// Compute desired windows.
	for _, v := range resolved {
		var startLine, endLine int64
		if v.End == nil {
			startLine = v.Start - int64(before)
			if startLine < 1 {
				startLine = 1
			}
			endLine = v.Start + int64(after)
		} else {
			startLine = v.Start
			endLine = *v.End
		}
		key := ""
		if v.End == nil {
			key = strconv.FormatInt(v.Start, 10)
		} else {
			key = fmt.Sprintf("%d-%d", v.Start, *v.End)
		}
		wantRange[v.Start] = [2]int64{startLine, endLine}
		// Pre-populate with empty slice so a miss still appears.
		out.Samples[key] = []string{}
		out.Lines[key] = -1
	}

	// Stream scan.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec, err := newCompressedReader(f, format)
	if err != nil {
		return err
	}
	defer func() {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
	}()

	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)

	var lineNum int64 = 0
	for sc.Scan() {
		lineNum++
		for _, v := range resolved {
			window, ok := wantRange[v.Start]
			if !ok {
				continue
			}
			if lineNum >= window[0] && lineNum <= window[1] {
				key := ""
				if v.End == nil {
					key = strconv.FormatInt(v.Start, 10)
				} else {
					key = fmt.Sprintf("%d-%d", v.Start, *v.End)
				}
				out.Samples[key] = append(out.Samples[key], sc.Text())
			}
		}
	}
	return sc.Err()
}

// streamCountLines decompresses and counts lines without keeping content.
func streamCountLines(path string, format compression.Format) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	dec, err := newCompressedReader(f, format)
	if err != nil {
		return 0, err
	}
	defer func() {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
	}()

	br := bufio.NewReader(dec)
	var n int64
	buf := make([]byte, 64*1024)
	for {
		read, err := br.Read(buf)
		if read > 0 {
			n += int64(bytes.Count(buf[:read], []byte{'\n'}))
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, err
		}
	}
	return n, nil
}
