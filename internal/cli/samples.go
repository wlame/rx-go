package cli

import (
	"fmt"
	"io"
	"strconv"

	json "github.com/goccy/go-json"
	"github.com/spf13/cobra"

	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/fileutil"
	"github.com/wlame/rx/internal/models"
)

// newSamplesCommand creates the "samples" subcommand for extracting file content
// around byte offsets or line numbers. This is the Go equivalent of Python's
// rx samples command.
func newSamplesCommand() *cobra.Command {
	var (
		byteOffsets   []int64
		lineNumbers   []int
		beforeContext int
		afterContext  int
		contextLines  int
		outputJSON    bool
		noColor       bool
	)

	cmd := &cobra.Command{
		Use:   "samples PATH",
		Short: "Get file content around byte offsets or line numbers",
		Long: `Get file content around specified byte offsets or line numbers.

Reads lines of context around one or more byte offsets or line numbers
in a text file. Useful for examining specific locations in large files.

Use -b/--byte-offsets for byte offsets, or -l/--lines for line numbers.

Examples:
  rx samples /var/log/app.log -b 1234
  rx samples /var/log/app.log -b 1234 -b 5678
  rx samples /var/log/app.log -l 100 -l 200
  rx samples /var/log/app.log -l 100 -B 2 -A 10
  rx samples /var/log/app.log -b 1234 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			return runSamples(cmd, path, samplesFlags{
				byteOffsets:   byteOffsets,
				lineNumbers:   lineNumbers,
				beforeContext: beforeContext,
				afterContext:  afterContext,
				contextLines:  contextLines,
				outputJSON:    outputJSON,
				noColor:       noColor,
			})
		},
	}

	cmd.Flags().Int64SliceVarP(&byteOffsets, "byte-offsets", "b", nil, "Byte offset(s) to extract context around (repeatable)")
	cmd.Flags().IntSliceVarP(&lineNumbers, "lines", "l", nil, "Line number(s) to extract context around (repeatable)")
	cmd.Flags().IntVarP(&beforeContext, "before-context", "B", -1, "Lines of context before")
	cmd.Flags().IntVarP(&afterContext, "after-context", "A", -1, "Lines of context after")
	cmd.Flags().IntVarP(&contextLines, "context", "c", -1, "Lines of context both sides (default: 3)")
	cmd.Flags().BoolVar(&outputJSON, "json", false, "Output in JSON format")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colored output")

	return cmd
}

// samplesFlags groups all samples flag values.
type samplesFlags struct {
	byteOffsets   []int64
	lineNumbers   []int
	beforeContext int
	afterContext  int
	contextLines  int
	outputJSON    bool
	noColor       bool
}

// runSamples executes the samples extraction.
func runSamples(cmd *cobra.Command, path string, flags samplesFlags) error {
	if len(flags.byteOffsets) == 0 && len(flags.lineNumbers) == 0 {
		return fmt.Errorf("must provide either --byte-offsets (-b) or --lines (-l)")
	}
	if len(flags.byteOffsets) > 0 && len(flags.lineNumbers) > 0 {
		return fmt.Errorf("cannot use both --byte-offsets and --lines; choose one")
	}

	before := resolveContext(flags.beforeContext, flags.contextLines, 3)
	after := resolveContext(flags.afterContext, flags.contextLines, 3)

	resp := models.NewSamplesResponse(path, before, after)

	if len(flags.byteOffsets) > 0 {
		// Byte offset mode.
		for _, offset := range flags.byteOffsets {
			lines, err := fileutil.GetContext(path, offset, before, after)
			if err != nil {
				return fmt.Errorf("get context at byte offset %d: %w", offset, err)
			}
			key := strconv.FormatInt(offset, 10)
			resp.Offsets[key] = -1
			resp.Samples[key] = lines
		}
	} else {
		// Line number mode.
		for _, lineNum := range flags.lineNumbers {
			lines, err := fileutil.GetContextByLines(path, lineNum, before, after)
			if err != nil {
				return fmt.Errorf("get context at line %d: %w", lineNum, err)
			}
			key := strconv.Itoa(lineNum)
			resp.Lines[key] = -1
			resp.Samples[key] = lines
		}
	}

	w := cmd.OutOrStdout()
	if flags.outputJSON {
		return outputSamplesJSON(w, &resp)
	}
	outputSamplesCLI(w, &resp, shouldColorize(flags.noColor))
	return nil
}

// outputSamplesJSON marshals the response as indented JSON.
func outputSamplesJSON(w io.Writer, resp *models.SamplesResponse) error {
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}

// outputSamplesCLI writes human-readable samples output.
func outputSamplesCLI(w io.Writer, resp *models.SamplesResponse, colorOn bool) {
	cfg := config.Load()

	fmt.Fprintf(w, "Samples from %s (context: %d before, %d after):\n\n",
		formatFilePath(resp.Path, colorOn), resp.BeforeCtx, resp.AfterCtx)

	for key, lines := range resp.Samples {
		fmt.Fprintf(w, "--- %s ---\n", colorize(key, ansiYellow, colorOn))
		for _, line := range lines {
			line = processNewlineSymbol(line, cfg.NewlineSymbol)
			fmt.Fprintln(w, line)
		}
		fmt.Fprintln(w)
	}
}
