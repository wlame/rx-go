package clicommand

import (
	"context"
	"errors"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/internal/webapi"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// signaturesFlag is the flag of `rx trace` that scans the files for
// signatures as well.
const signaturesFlag = "signatures"

// defaultSelection is what a bare --signatures selects: the default
// set.
const defaultSelection = "default"

// bindSignaturesFlag declares --signatures[=LIST] on cmd, bound to
// selection.
//
// Go note: NoOptDefVal is the value pflag gives a flag written without
// `=value`. So `--signatures` alone reads "default", `--signatures=a,b`
// reads "a,b", and `--signatures a,b` reads "default" followed by the
// positional "a,b": the list needs `=`, as for every long flag of rx.
// `rx logs trace` shares the other trace flags and not this one: log
// chains are not scanned for signatures.
func bindSignaturesFlag(cmd *cobra.Command, selection *string) {
	cmd.Flags().StringVar(selection, signaturesFlag, "",
		"Scan the files for signatures too: ids, groups, tags, all, default, -word to remove "+
			"(--signatures=traceback,sql); a bare --signatures is the default set. "+
			"With it every positional is a path and a pattern goes with -e")
	cmd.Flags().Lookup(signaturesFlag).NoOptDefVal = defaultSelection
}

// signatureSelection is the loaded catalogue and the signatures a
// --signatures value selected from it.
type signatureSelection struct {
	expr    string
	catalog *signatures.Catalogue
	sigs    []*signatures.Signature
}

// order is the ids of the selection in catalogue order; nil for no
// selection.
func (s *signatureSelection) order() []string {
	if s == nil {
		return nil
	}
	ids := make([]string, len(s.sigs))
	for i, sig := range s.sigs {
		ids[i] = sig.ID
	}
	return ids
}

// loadSignatureSelection loads the signatures (built-ins and the user's
// files, every example checked by the scan engine, as `rx signatures`
// loads them) and resolves expr. An empty expr is the default set, as
// a bare flag is. A selection that names an unknown or refused
// signature is a usage error (exit 2) with the selection's message: the
// word, the close names, the load errors.
func loadSignatureSelection(expr string) (*signatureSelection, error) {
	if expr == "" {
		expr = defaultSelection
	}
	cat := signatures.Load(signatures.LoadOptions{CheckExamples: sigscan.CheckExamples})
	sigs, err := cat.Resolve(expr)
	if err != nil {
		var selErr *signatures.SelectionError
		if errors.As(err, &selErr) {
			return nil, exitWithError(os.Stderr, ExitUsageError, "--signatures=%s: %s", expr, selErr.Error())
		}
		return nil, exitWithError(os.Stderr, ExitUsageError, "--signatures=%s: %s", expr, err.Error())
	}
	return &signatureSelection{expr: expr, catalog: cat, sigs: sigs}, nil
}

// signatureTrace is one `rx trace --signatures` run after its paths
// were checked.
type signatureTrace struct {
	// paths are the paths as the engine sees them, and patterns the
	// patterns (none is allowed).
	paths, patterns []string
	opts            trace.Options
	params          traceParams
	selection       *signatureSelection
	// spooled lists the temporary file piped stdin was copied to, as it
	// appears in paths (never cached).
	spooled []string
}

// runSignatureTrace runs a trace with signatures (findings.Search): the
// search of the patterns over the paths (none when there is no
// pattern), then a scan of every file the search covered for the
// selection, through the findings cache, and the answer's cli_command.
//
// Every setting reaches the scan and the cache through their options:
// RX_WORKERS, RX_SIGNATURES_LINE_BYTES, RX_FINDINGS_MAX_PER_SIGNATURE,
// RX_TIMESTAMP_LOOKBACK_KB and RX_LOG_TZ (findingsScanOptions),
// RX_MAX_LINE_TEXT_BYTES, RX_FINDINGS_TEXT_MAX_BYTES and
// RX_MAX_SIGNATURE_SCANS here. --no-cache also skips the findings
// cache, --no-index the line index the text reads may use,
// --max-results caps the matches only, and the context window
// (-B, -A, --context) adds lines around each finding too.
func runSignatureTrace(ctx context.Context, engine *trace.Engine, run signatureTrace) (*rxtypes.TraceResponse, error) {
	findings.SetMaxScans(config.MaxSignatureScans())
	sel := run.selection
	resp, err := findings.Search(ctx, engine, findings.SearchRequest{
		Paths: run.paths, Patterns: run.patterns, Search: run.opts, Spooled: run.spooled,
		Catalogue: sel.catalog, Selection: sel.sigs, Findings: signatureTraceOptions(run.params, sel.catalog),
	})
	if err != nil {
		return nil, err
	}
	cli := webapi.BuildCLICommand("trace", signatureCLIParams(run))
	resp.CLICommand = &cli
	return resp, nil
}

// signatureTraceOptions reads the settings of the findings of a trace
// from the environment (webapi.FindingsOptionsFromEnv) and the flags.
func signatureTraceOptions(p traceParams, cat *signatures.Catalogue) findings.TraceOptions {
	opts := webapi.FindingsOptionsFromEnv()
	opts.Scan.NoCache = p.noCache
	// Every loaded redacting signature masks every excerpt and line, the
	// selected ones and the others alike.
	opts.Scan.Scan.Redactors = cat.Redactors()
	opts.IndexLoader = samples.StoredIndex
	if p.noIndex {
		opts.IndexLoader = samples.NoIndex
	}
	opts.Before = resolveBefore(p)
	opts.After = resolveAfter(p)
	return opts
}

// signatureCLIParams are the values the equivalent command of a trace
// with signatures is written from (cli_command): the paths (piped stdin
// as "-"), the patterns with --regexp, the context window as the run
// resolved it, the flags the user gave, and the selection spelled out
// (a bare flag as --signatures=default). A generated request id and the
// RX_HOOK_* fallbacks are left out, as the HTTP API leaves them out.
func signatureCLIParams(run signatureTrace) map[string]any {
	p := run.params
	paths := make([]string, len(run.paths))
	for i, path := range run.paths {
		paths[i] = path
		if slices.Contains(run.spooled, path) {
			paths[i] = "-"
		}
	}
	before, after := resolveBefore(p), resolveAfter(p)
	return map[string]any{
		"path":             paths,
		"regexp":           run.patterns,
		"matching_flags":   p.matchingFlagNames,
		"max_results":      run.opts.MaxResults,
		"before_context":   &before,
		"after_context":    &after,
		"no_cache":         p.noCache,
		"no_index":         p.noIndex,
		"no_recursive":     p.noRecursive,
		"request_id":       p.requestID,
		"hook_on_file":     p.hookOnFile,
		"hook_on_match":    p.hookOnMatch,
		"hook_on_complete": p.hookOnComplete,
		"signatures":       run.selection.expr,
	}
}
