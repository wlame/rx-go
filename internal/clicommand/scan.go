package clicommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/cachefill"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// NewScanCommand builds the `rx scan` cobra command: it scans files for
// signatures and keeps their findings in the findings cache, the command
// POST /v1/findings is the equivalent of.
//
//	rx scan PATH [PATH ...]            # the default set
//	rx scan --signatures=traceback,sql PATH
//	rx scan DIR                        # the files directly inside DIR
//	rx scan --recursive DIR            # every file the tree lists under DIR
//	rx scan --force PATH               # scan current signatures again
//	rx scan --json PATH                # per-file state and counts, and totals
//
// A directory is scanned as `rx index` indexes one: the files directly
// inside it, and with --recursive every file below it. Files are scanned
// one at a time, in the order they are named and, under a directory, in
// the walk's order (the order `rx index` and, with --recursive,
// POST /v1/findings on the directory take them in). The exit codes are
// those of `rx index`.
func NewScanCommand(out io.Writer) *cobra.Command {
	var (
		selection  string
		force      bool
		recursive  bool
		jsonOutput bool
	)
	cmd := &cobra.Command{
		Use:   "scan PATH [PATH ...]",
		Short: "Scan files for signatures and keep their findings in the cache",
		Long: "Scan files for signatures and keep their findings in the findings cache, so that " +
			"rx trace --signatures, GET /v1/findings and the viewer answer from it. A file whose findings " +
			"are current for every selected signature is not read again unless --force is given. " +
			"For a directory, the files the file tree lists directly inside it are scanned, and with " +
			"--recursive every file below it, one at a time (hidden entries only with --hidden); a file " +
			"that is not text is skipped.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScan(cmd.Context(), out, scanParams{
				paths:       args,
				selection:   selection,
				force:       force,
				recursive:   recursive,
				jsonOutput:  jsonOutput,
				maskSecrets: maskSecretsMode(cmd),
			})
		},
	}
	cmd.Flags().StringVar(&selection, signaturesFlag, "",
		"The signatures to scan for: ids, groups, tags, all, default, -word to remove "+
			"(--signatures=traceback,sql); left out, or a bare --signatures, is the default set")
	// Go note: NoOptDefVal is the value pflag gives a flag written without
	// `=value`, so a bare --signatures reads "default", as in rx trace.
	cmd.Flags().Lookup(signaturesFlag).NoOptDefVal = defaultSelection
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Scan every selected signature again, those with current findings too")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false,
		"Scan every file below a directory, in its subdirectories too; without it, the files directly inside it")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	bindMaskSecretsFlag(cmd)
	return cmd
}

// scanParams are the values of one `rx scan` command line.
type scanParams struct {
	paths      []string
	selection  string
	force      bool
	recursive  bool
	jsonOutput bool
	// maskSecrets is the masking mode: --mask-secrets, else
	// RX_MASK_SECRETS (maskSecretsMode).
	maskSecrets bool
}

// scanResult is the --json answer of `rx scan`. Every list is [] rather
// than null when empty.
type scanResult struct {
	// Signatures is the selection as the run resolved it ("default" for
	// none).
	Signatures string `json:"signatures"`
	// Files are the files whose findings the run knows: scanned now, or
	// found current. In the order they were reached.
	Files []scannedFile `json:"files"`
	// Skipped are the files passed over, each with the reason.
	Skipped []rxtypes.SkippedFile `json:"skipped"`
	// Errors are the files that failed, each with its error.
	Errors    []indexErrorItem `json:"errors"`
	Totals    scanTotals       `json:"totals"`
	TotalTime float64          `json:"total_time"`
}

// scannedFile is one file of scanResult.Files.
type scannedFile struct {
	Path string `json:"path"`
	// Outcome is "scanned" (read and cached by this run) or "current"
	// (every selected signature's findings were current, nothing read).
	Outcome string `json:"outcome"`
	// State is the file's state against the default set: full, partial
	// or none.
	State string `json:"state"`
	// Counts are the findings of each selected signature that found
	// something in the file, those past the per-signature cap included;
	// every selected signature was checked, and one that found nothing
	// is left out.
	Counts map[string]int64 `json:"counts"`
	// Findings is the sum of Counts.
	Findings int64 `json:"findings"`
}

// scanTotals are the run's counts (cachefill.Counts: files_total, done,
// skipped, current, failed) and the findings of every file in Files.
type scanTotals struct {
	cachefill.Counts
	Findings int64 `json:"findings"`
}

// scanOutcomeWords is how scanResult names the two outcomes of a file
// whose findings are known.
var scanOutcomeWords = map[cachefill.Outcome]string{
	cachefill.Filled:  "scanned",
	cachefill.Current: "current",
}

// add records one file's result.
func (r *scanResult) add(res cachefill.FileResult) {
	r.Totals.Add(res)
	switch res.Outcome {
	case cachefill.Filled, cachefill.Current:
		file := scannedFileOf(res)
		r.Totals.Findings += file.Findings
		r.Files = append(r.Files, file)
	case cachefill.Skipped:
		r.Skipped = append(r.Skipped, rxtypes.SkippedFile{Path: res.Path, Reason: res.Reason})
	default:
		r.Errors = append(r.Errors, fillFailure(res))
	}
}

// fail records a named path that could not be stated, with the exit
// code its error gives.
func (r *scanResult) fail(path string, err error) {
	r.Totals.Add(cachefill.FileResult{Outcome: cachefill.Failed})
	r.Errors = append(r.Errors, indexErrorItem{Path: path, Error: err.Error(), exitCode: exitCodeForPathError(err)})
}

// scannedFileOf is the entry of a file whose findings the run knows.
func scannedFileOf(res cachefill.FileResult) scannedFile {
	ans := res.Answer
	file := scannedFile{
		Path: res.Path, Outcome: scanOutcomeWords[res.Outcome], State: string(ans.State),
		Counts: map[string]int64{},
	}
	for _, id := range ans.Checked {
		if count := ans.BySignature[id].Count; count > 0 {
			file.Counts[id] = count
			file.Findings += count
		}
	}
	return file
}

// failureCodes are the exit codes of the files that failed, one each.
func (r *scanResult) failureCodes() []int {
	codes := make([]int, len(r.Errors))
	for i, item := range r.Errors {
		codes[i] = item.exitCode
	}
	return codes
}

// runScan runs one `rx scan`: every path checked against the search
// roots (exit 4), the selection loaded (exit 2 for one that names an
// unknown or refused signature), the paths stated and each directory
// listed (recursively with --recursive), then every file scanned in turn
// (cachefill.ScanFile) and the answer printed. A
// file that fails is reported and the rest are scanned; the exit code is
// then the one its failures share, as for `rx index`. An interrupt
// (ctx canceled) stops the run between files or inside one, and nothing
// more is written.
//
// The scan's settings are those `rx trace --signatures` honors:
// RX_WORKERS, RX_SIGNATURES_LINE_BYTES, RX_FINDINGS_MAX_PER_SIGNATURE,
// RX_TIMESTAMP_LOOKBACK_KB, RX_LOG_TZ and the cache directory
// (findingsScanOptions), RX_MAX_SIGNATURE_SCANS, and the masking mode.
func runScan(ctx context.Context, out io.Writer, p scanParams) error {
	start := time.Now()
	for _, path := range p.paths {
		if _, err := paths.ValidatePathWithinRoots(path); err != nil && !errors.Is(err, paths.ErrNoSearchRootsConfigured) {
			return exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
		}
	}
	sel, err := loadSignatureSelection(p.selection)
	if err != nil {
		return err
	}
	result := &scanResult{
		Signatures: sel.expr, Files: []scannedFile{}, Skipped: []rxtypes.SkippedFile{}, Errors: []indexErrorItem{},
	}
	entries := scanEntries(p, sel.catalog, result)

	findings.SetMaxScans(config.MaxSignatureScans())
	opts := findingsScanOptions()
	opts.MaskSecrets = p.maskSecrets
	opts.Force = p.force
	settings := cachefill.ScanSettings{Catalogue: sel.catalog, Selection: sel.sigs, Options: opts}
	err = cachefill.Each(ctx, entries, &cachefill.Progress{}, func(ctx context.Context, e cachefill.Entry) cachefill.FileResult {
		return cachefill.ScanFile(ctx, e, settings)
	}, result.add)
	if err != nil {
		return err
	}
	result.TotalTime = time.Since(start).Seconds()

	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
	} else {
		writeScanHuman(out, result)
	}
	return multiPathFailure(result.failureCodes(), "one or more files failed to scan")
}

// scanEntries turns the paths of a run into the files to scan, in the
// order the paths were given: a file as named, a directory listed by the
// tree's rules (listDirectory): the files directly inside it, or with
// --recursive every file below it, as `rx index` lists a directory. A
// path that cannot be stated, or a directory that cannot be listed, is
// recorded in result as a failure; a missing path that reads as a
// selection of cat gets the hint of selectionHint on stderr.
func scanEntries(p scanParams, cat *signatures.Catalogue, result *scanResult) []cachefill.Entry {
	var entries []cachefill.Entry
	for _, path := range p.paths {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				if hint := selectionHint(cat, path); hint != "" {
					_, _ = fmt.Fprintln(os.Stderr, output.PrintableMessage(hint))
				}
			}
			result.fail(path, err)
			continue
		}
		if !info.IsDir() {
			entries = append(entries, cachefill.Entry{Path: path, Named: true})
			continue
		}
		listed, err := listDirectory(path, p.recursive)
		if err != nil {
			result.fail(path, err)
			continue
		}
		entries = append(entries, listed...)
	}
	return entries
}

// writeScanHuman prints one line per file whose findings are known (its
// state, its findings and their counts by signature, and "current" when
// nothing was read), the skipped files with their reasons, the errors on
// stderr, and a total line.
func writeScanHuman(out io.Writer, r *scanResult) {
	for _, file := range r.Files {
		_, _ = fmt.Fprintf(out, "  %s: %s, %s%s\n", output.Printable(file.Path), file.State,
			findingsText(file), currentSuffix(file))
	}
	writeSkippedHuman(out, r.Skipped, indexUnitFiles)
	for _, e := range r.Errors {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %s: %s\n", output.Printable(e.Path), output.Printable(e.Error))
	}
	t := r.Totals
	_, _ = fmt.Fprintf(out, "Total: %d files, %d scanned, %d skipped (%d current), %d failed, %s findings in %.1fs\n",
		t.FilesTotal, t.Done, t.Skipped, t.Current, t.Failed, output.Thousands(t.Findings), r.TotalTime)
}

// findingsText is "N findings" and, when there are any, the count of
// each signature that found something, sorted by id.
func findingsText(file scannedFile) string {
	text := output.Thousands(file.Findings) + " findings"
	if file.Findings == 1 {
		text = "1 finding"
	}
	if file.Findings == 0 {
		return text
	}
	var parts []string
	for _, id := range sortedCountIDs(file) {
		parts = append(parts, fmt.Sprintf("%s %s", id, output.Thousands(file.Counts[id])))
	}
	return text + " (" + strings.Join(parts, ", ") + ")"
}

// sortedCountIDs are the ids of the signatures that found something in
// the file, sorted by id: the order a reader looks a signature up in.
func sortedCountIDs(file scannedFile) []string {
	ids := make([]string, 0, len(file.Counts))
	for id := range file.Counts {
		ids = append(ids, id)
	}
	// Go note: map iteration order is random, so the ids are sorted
	// before they are printed.
	slices.Sort(ids)
	return ids
}

// currentSuffix is ", current" for a file whose findings were all
// current, so the run read nothing of it.
func currentSuffix(file scannedFile) string {
	if file.Outcome == scanOutcomeWords[cachefill.Current] {
		return ", current"
	}
	return ""
}
