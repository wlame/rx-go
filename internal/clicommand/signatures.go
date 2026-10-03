package clicommand

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/webapi"
)

// ExamplesChecker runs a signature's examples and says why they fail;
// signatures.LoadOptions.CheckExamples takes it. Nil means this build
// does not run examples.
type ExamplesChecker = func(*signatures.Signature) error

// NewSignaturesCommand builds `rx signatures`, which lists, shows and
// checks the signatures rx knows: the built-in ones and those of the
// user files on the search path (RX_SIGNATURES_PATH, else
// $XDG_CONFIG_HOME/rx/signatures/, else ~/.config/rx/signatures/).
//
//	rx signatures [--group=G] [--tag=T] [--json]   # list, grouped by group
//	rx signatures show ID [--json]                 # one definition
//	rx signatures check [FILE...]                  # load and verify, for CI
//	rx signatures template                         # a commented example file
//
// checkExamples runs each signature's examples while loading; nil skips
// them, and the commands say so. The signature files are read only
// when one of these commands runs.
func NewSignaturesCommand(out io.Writer, checkExamples ExamplesChecker) *cobra.Command {
	var opts signaturesListOptions
	cmd := &cobra.Command{
		Use:   "signatures",
		Short: "List, show and check the signatures rx can find in files",
		Long: "A signature is a named definition of something worth noticing in a long file: a traceback, " +
			"a secret, a run of blank lines. rx has built-in signatures and reads user signature files " +
			"(*.yml, *.yaml) from RX_SIGNATURES_PATH (entries separated by \":\"), else from " +
			"$XDG_CONFIG_HOME/rx/signatures/, else from ~/.config/rx/signatures/. A signature marked masked " +
			"(redact: true) masks what it finds in the text of every finding when masking is on: " +
			"rx trace --mask-secrets, rx serve --mask-secrets, or RX_MASK_SECRETS=1. Masking is off by default.",
		// NoArgs makes a word that names no subcommand a usage error
		// ("unknown command") instead of a list.
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cat := signatures.Load(signatures.LoadOptions{CheckExamples: checkExamples})
			return runSignaturesList(out, cat, opts)
		},
	}
	cmd.Flags().StringVar(&opts.group, "group", "", "List only the signatures of this group")
	cmd.Flags().StringVar(&opts.tag, "tag", "", "List only the signatures with this tag")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output JSON: signatures, groups, levels, errors, search path and files")
	cmd.AddCommand(newSignaturesShowCommand(out, checkExamples), newSignaturesCheckCommand(out, checkExamples),
		newSignaturesTemplateCommand(out))
	return cmd
}

// signaturesListOptions are the flags of `rx signatures`.
type signaturesListOptions struct {
	group, tag string
	json       bool
}

// runSignaturesList prints the catalogue, narrowed by --group and
// --tag. An unknown group or tag is a usage error with close names.
func runSignaturesList(out io.Writer, cat *signatures.Catalogue, opts signaturesListOptions) error {
	if opts.group != "" && !hasGroup(cat, opts.group) {
		return exitWithError(os.Stderr, ExitUsageError, "unknown group %q%s", opts.group,
			closeNamesText(opts.group, groupNames(cat)))
	}
	if opts.tag != "" && !hasTag(cat, opts.tag) {
		return exitWithError(os.Stderr, ExitUsageError, "unknown tag %q%s", opts.tag, closeNamesText(opts.tag, tagNames(cat)))
	}
	listed := webapi.ListedSignatures(cat, opts.group, opts.tag)
	if opts.json {
		return writeJSON(out, webapi.SignaturesView(cat, listed, opts.group))
	}
	writeSignatureList(out, cat, listed, opts.group)
	return nil
}

// listColumns are the widths of the list's columns, measured over every
// row printed, so the rows of all groups line up.
type listColumns struct{ id, level, kind, source int }

// writeSignatureList prints the human list: per group in order, a line
// with its count (and "opt-in" when every active signature of it is),
// then a row per signature; the errors; the search path; the masking
// mode (maskingLine).
//
//	traceback  1 signature
//	  python-traceback  high  block  built-in
func writeSignatureList(out io.Writer, cat *signatures.Catalogue, listed []*signatures.Signature, onlyGroup string) {
	var cols listColumns
	for _, s := range listed {
		cols.id = max(cols.id, len(s.ID))
		cols.level = max(cols.level, len(s.Level))
		cols.kind = max(cols.kind, len(s.Kind))
		cols.source = max(cols.source, len(output.Printable(s.Source.String())))
	}
	for _, g := range cat.Groups() {
		var rows []*signatures.Signature
		for _, s := range listed {
			if s.Group == g.Name {
				rows = append(rows, s)
			}
		}
		if len(rows) == 0 && g.Name != onlyGroup {
			continue
		}
		_, _ = fmt.Fprintln(out, groupHeader(cat, g))
		for _, s := range rows {
			_, _ = fmt.Fprintln(out, signatureRow(s, cols))
		}
	}
	if errs := cat.Errors(); len(errs) > 0 {
		_, _ = fmt.Fprintf(out, "\nErrors (%d)\n", len(errs))
		for _, e := range errs {
			_, _ = fmt.Fprintf(out, "  %s\n", output.PrintableMessage(e.Error()))
		}
	}
	_, _ = fmt.Fprintf(out, "\nSearch path: %s (%s)\n", searchPathText(cat.Paths()), countNoun(len(cat.Files()), "file"))
	_, _ = fmt.Fprintln(out, maskingLine(config.MaskSecrets()))
}

// maskingLine says what the masked mark means now: whether
// RX_MASK_SECRETS turns masking on for `rx trace` and `rx serve` run
// without --mask-secrets, and how to turn it on when it does not.
func maskingLine(maskSecrets bool) string {
	if maskSecrets {
		return "Secrets: masked by the signatures marked masked (" + config.MaskingEnv + ")"
	}
	return "Secrets: shown; --mask-secrets or " + config.MaskingEnv + "=1 masks what the signatures marked masked find"
}

// groupHeader is a group's line in the list: its name, how many active
// signatures it has, and "opt-in" when none of them is in the default
// set.
func groupHeader(cat *signatures.Catalogue, g signatures.Group) string {
	active, optIn := 0, 0
	for _, s := range cat.Signatures() {
		if s.Group == g.Name {
			active++
			if !s.Default {
				optIn++
			}
		}
	}
	header := g.Name + "  " + countNoun(active, "signature")
	if active > 0 && optIn == active {
		header += ", opt-in"
	}
	return header
}

// signatureRow is a signature's row in the list: id, level, kind,
// source and marks, padded to the columns, with no trailing spaces.
func signatureRow(s *signatures.Signature, cols listColumns) string {
	row := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %s", cols.id, s.ID, cols.level, s.Level, cols.kind, s.Kind,
		cols.source, output.Printable(s.Source.String()), strings.Join(signatureMarks(s), ", "))
	return strings.TrimRight(row, " ")
}

// signatureMarks are the marks of a row: overrides, masked (redact:
// true, which masks only when masking is on), opt-in, disabled by
// file:line.
func signatureMarks(s *signatures.Signature) []string {
	var marks []string
	if s.Overrides {
		marks = append(marks, "overrides")
	}
	if s.Redact {
		marks = append(marks, "masked")
	}
	if !s.Default {
		marks = append(marks, "opt-in")
	}
	if s.DisabledBy != nil {
		marks = append(marks, "disabled by "+output.Printable(s.DisabledBy.String()))
	}
	return marks
}

// newSignaturesShowCommand builds `rx signatures show ID`.
func newSignaturesShowCommand(out io.Writer, checkExamples ExamplesChecker) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show ID",
		Short: "Show one signature: its definition with every default, source, keywords, line pass and cache key",
		Long: "Prints the signature's definition as YAML with every default filled in, after comment lines " +
			"that give its source, its keyword clauses (derived from the regex or declared), whether it " +
			"runs in the line pass, and the key its findings are cached under with the current settings " +
			"(RX_SIGNATURES_LINE_BYTES, RX_FINDINGS_MAX_PER_SIGNATURE, RX_TIMESTAMP_LOOKBACK_KB, RX_LOG_TZ " +
			"and, with RX_MASK_SECRETS on, the redacting signatures). Exit 2 for an id no signature has or one " +
			"whose definition was refused.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cat := signatures.Load(signatures.LoadOptions{CheckExamples: checkExamples})
			return runSignaturesShow(out, cat, args[0], jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the signature as JSON, as rx signatures --json lists it")
	return cmd
}

// runSignaturesShow prints one signature, active or disabled. An id no
// signature has, and one whose every definition was refused, are usage
// errors, as in a selection.
func runSignaturesShow(out io.Writer, cat *signatures.Catalogue, id string, jsonOutput bool) error {
	sig, ok := cat.Lookup(id)
	if !ok {
		sig, ok = cat.LookupDisabled(id)
	}
	if !ok {
		if refused := cat.Refused(id); len(refused) > 0 {
			texts := make([]string, len(refused))
			for i, e := range refused {
				texts[i] = e.Error()
			}
			return exitWithError(os.Stderr, ExitUsageError, "signature %s was not loaded: %s", id, strings.Join(texts, "; "))
		}
		return exitWithError(os.Stderr, ExitUsageError, "unknown signature %q%s", id, closeNamesText(id, signatureIDs(cat)))
	}
	if jsonOutput {
		return writeJSON(out, webapi.SignatureView(sig))
	}
	for _, line := range showHeader(cat, sig) {
		_, _ = fmt.Fprintln(out, "# "+output.Printable(line))
	}
	_, _ = out.Write(sig.DefinitionYAML())
	return nil
}

// showHeader is what `show` prints above the definition, as YAML
// comments so the whole output stays a valid definition. The cache key
// is the one a scan with the environment's settings stores the
// signature's findings under (findings.Key).
func showHeader(cat *signatures.Catalogue, sig *signatures.Signature) []string {
	lines := []string{"source: " + sig.Source.String()}
	if sig.Overrides {
		lines = append(lines, "overrides the built-in signature "+sig.ID)
	}
	if sig.DisabledBy != nil {
		lines = append(lines, "disabled by "+sig.DisabledBy.String())
	}
	lines = append(lines, keywordsLine(sig), "line pass: "+yesNo(sig.LinePass()),
		"cache key: "+findings.Key(sig, cat, findingsScanOptions()), examplesLine(cat, sig))
	return lines
}

// keywordsLine describes the keyword clauses: "keywords (derived):
// [delete] AND [from]", members of one clause separated by commas.
func keywordsLine(sig *signatures.Signature) string {
	if len(sig.Clauses) == 0 {
		return "keywords: none"
	}
	origin := "derived"
	if sig.KeywordsDeclared {
		origin = "declared"
	}
	clauses := make([]string, len(sig.Clauses))
	for i, c := range sig.Clauses {
		clauses[i] = "[" + strings.Join(c, ", ") + "]"
	}
	return "keywords (" + origin + "): " + strings.Join(clauses, " AND ")
}

// examplesLine says whether the signature's examples were run.
func examplesLine(cat *signatures.Catalogue, sig *signatures.Signature) string {
	switch {
	case !cat.ExamplesChecked():
		return examplesNotChecked
	case len(sig.Examples.Match)+len(sig.Examples.NoMatch) == 0:
		return "examples: none"
	case sig.DisabledBy != nil:
		return "examples: not run (disabled)"
	}
	return fmt.Sprintf("examples: %d match, %d no_match, all pass", len(sig.Examples.Match), len(sig.Examples.NoMatch))
}

// examplesNotChecked is what the commands print when the build has no
// examples checker.
const examplesNotChecked = "examples: not checked in this build"

// newSignaturesCheckCommand builds `rx signatures check [FILE...]`.
func newSignaturesCheckCommand(out io.Writer, checkExamples ExamplesChecker) *cobra.Command {
	return &cobra.Command{
		Use:   "check [FILE...]",
		Short: "Load signature files and report their errors and warnings (exit 1 on an error)",
		Long: "With FILE arguments (files or directories), loads only those, plus the built-ins for overrides " +
			"and groups; without, loads the search path. Prints one line per error and warning, then a " +
			"summary. Exit 0 when there is no error, 1 when there is one.",
		RunE: func(_ *cobra.Command, args []string) error {
			return runSignaturesCheck(out, args, checkExamples)
		},
	}
}

// runSignaturesCheck loads the files (or the search path) and prints
// every error, every warning and a summary. A file named that does not
// exist is an error too: a typo must not pass a CI check.
func runSignaturesCheck(out io.Writer, files []string, checkExamples ExamplesChecker) error {
	var paths []string
	problems := 0
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			_, _ = fmt.Fprintf(out, "%s: error: %s\n", output.Printable(f), statProblem(err))
			problems++
			continue
		}
		paths = append(paths, f)
	}
	// No FILE argument loads the search path (Paths nil); FILE
	// arguments that all failed load the built-ins alone (an empty,
	// non-nil Paths).
	switch {
	case len(files) == 0:
		paths = nil
	case paths == nil:
		paths = []string{}
	}
	cat := signatures.Load(signatures.LoadOptions{Paths: paths, CheckExamples: checkExamples})
	for _, e := range cat.Errors() {
		_, _ = fmt.Fprintln(out, output.PrintableMessage(problemLine(e, "error")))
		problems++
	}
	warnings := 0
	userSignatures := 0
	for _, s := range cat.Signatures() {
		if s.Source.Builtin {
			continue
		}
		userSignatures++
		if s.Anchor() != nil && s.LinePass() {
			e := signatures.LoadError{File: s.Source.File, Line: s.Source.Line, ID: s.ID, Message: "no keywords: runs in the line pass"}
			_, _ = fmt.Fprintln(out, output.Printable(problemLine(e, "warning")))
			warnings++
		}
	}
	if cat.ExamplesChecked() {
		_, _ = fmt.Fprintln(out, "examples: checked")
	} else {
		_, _ = fmt.Fprintln(out, examplesNotChecked)
	}
	_, _ = fmt.Fprintf(out, "%s, %s: %s, %s\n", countNoun(len(cat.Files()), "file"), countNoun(userSignatures, "signature"),
		countNoun(problems, "error"), countNoun(warnings, "warning"))
	if problems > 0 {
		return exitWithError(os.Stderr, ExitGenericError, "the signature files have %s", countNoun(problems, "error"))
	}
	return nil
}

// statProblem words why a file named to check cannot be read.
func statProblem(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "no such file or directory"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return err.Error()
}

// problemLine formats an error or warning the way compilers do:
// "file:line: id: kind: message", leaving out a line of 0 and an empty
// id.
func problemLine(e signatures.LoadError, kind string) string {
	place := e.File
	if e.Line > 0 {
		place += ":" + strconv.Itoa(e.Line)
	}
	if e.ID != "" {
		place += ": " + e.ID
	}
	return place + ": " + kind + ": " + e.Message
}

// newSignaturesTemplateCommand builds `rx signatures template`.
func newSignaturesTemplateCommand(out io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "template",
		Short: "Print a commented signature file with one signature of each kind",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_, err := out.Write(signatures.Template())
			return err
		},
	}
}

// writeJSON prints v as indented JSON.
func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// countNoun writes "1 file", "2 files".
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// yesNo writes a flag as "yes" or "no".
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// searchPathText joins the search path for the list's last line.
func searchPathText(paths []string) string {
	if len(paths) == 0 {
		return "(none)"
	}
	printable := make([]string, len(paths))
	for i, p := range paths {
		printable[i] = output.Printable(p)
	}
	return strings.Join(printable, string(os.PathListSeparator))
}

// closeNamesText is the "; close names: …" tail of an error, or "".
func closeNamesText(word string, names []string) string {
	near := signatures.CloseNames(word, names)
	if len(near) == 0 {
		return ""
	}
	return "; close names: " + strings.Join(near, ", ")
}

// hasGroup reports whether the catalogue declares the group.
func hasGroup(cat *signatures.Catalogue, name string) bool {
	return slices.Contains(groupNames(cat), name)
}

// hasTag reports whether an active or disabled signature has the tag.
func hasTag(cat *signatures.Catalogue, tag string) bool {
	return slices.Contains(tagNames(cat), tag)
}

// groupNames lists the group names in order.
func groupNames(cat *signatures.Catalogue) []string {
	var names []string
	for _, g := range cat.Groups() {
		names = append(names, g.Name)
	}
	return names
}

// tagNames lists the tags of the active and disabled signatures.
func tagNames(cat *signatures.Catalogue) []string {
	var tags []string
	for _, list := range [][]*signatures.Signature{cat.Signatures(), cat.Disabled()} {
		for _, s := range list {
			for _, t := range s.Tags {
				if !slices.Contains(tags, t) {
					tags = append(tags, t)
				}
			}
		}
	}
	return tags
}

// signatureIDs lists the ids of the active and disabled signatures.
func signatureIDs(cat *signatures.Catalogue) []string {
	var ids []string
	for _, list := range [][]*signatures.Signature{cat.Signatures(), cat.Disabled()} {
		for _, s := range list {
			ids = append(ids, s.ID)
		}
	}
	return ids
}
