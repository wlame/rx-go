package webapi

import (
	"slices"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/internal/output"
)

// BuildCLICommand returns the rx command that does what an HTTP request
// did, for the `cli_command` member of its answer. The string is meant
// to be pasted into a terminal: a value that needs shell quoting is
// quoted with output.Quote (Python-shlex-compatible).
//
// operation names a row of cliCommandTable: "trace", "samples",
// "time_range", "index_get", "index_post" or "compress"; an unknown
// name renders "".
// params holds the request's values keyed by request field name; CLIArg
// lists the Go types a value may have.
//
// The rendering is `rx <subcommand> <positionals...> <--long=value...>`,
// with the flags in table order. One rule decides whether a flag is
// written: it appears when the request gave a value and that value is
// not what rx uses without the flag (CLIArg.Absent). A field the request
// left out is never written, because every HTTP default is the CLI
// default; a value equal to the default is left out too, so the command
// is the shortest one that does the same thing.
//
// The cases are in testdata/cli-commands.json, which rx-python holds a
// copy of at tests/data/cli-commands.json.
func BuildCLICommand(operation string, params map[string]any) string {
	op, ok := cliCommandTable[operation]
	if !ok {
		return ""
	}
	words := []string{"rx", op.Subcommand}
	for _, arg := range op.Args {
		if arg.Kind == ArgPositional {
			words = append(words, positionalWords(params[arg.Field])...)
		}
	}
	for _, arg := range op.Args {
		if arg.Kind != ArgPositional {
			words = append(words, flagWords(op, arg, params)...)
		}
	}
	words = append(words, op.FixedFlags...)
	return strings.Join(words, " ")
}

// CLICommandOperation is one row of the table BuildCLICommand renders
// from: the rx subcommand an HTTP operation corresponds to, and how each
// request field becomes part of the command.
type CLICommandOperation struct {
	// Subcommand is the rx subcommand, such as "samples".
	Subcommand string
	// Args lists the request fields in the order they are rendered.
	Args []CLIArg
	// FixedFlags are written after every rendered flag, whatever the
	// request held: `--info --json` makes `rx index` read an index
	// instead of building one.
	FixedFlags []string
}

// ArgKind says how one request field becomes words of an rx command.
type ArgKind int

const (
	// ArgPositional writes the value, or each element of a []string, as
	// a positional argument.
	ArgPositional ArgKind = iota
	// ArgFlag writes --name=value: once for a string or a number, once
	// per element for a []string. A bool is written as the bare --name
	// when true and as --name=false when false.
	ArgFlag
	// ArgSwitchList takes a []string of long flag names and writes
	// --<name> for each: flags that are on by being present, such as the
	// ripgrep matching flags of trace.MatchingFlags.
	ArgSwitchList
)

// CLIArg maps one request field to an rx positional or flag.
//
// The value in params may be a string, *string, int, *int, bool, *bool
// or []string. nil, a nil pointer, "" and an empty slice mean the
// request did not give the field, and nothing is written for it.
type CLIArg struct {
	// Field is the request field: the key in the params map.
	Field string
	// Flag is the long flag name without dashes. Empty for a positional
	// and for a switch list.
	Flag string
	// Kind says how the value is written.
	Kind ArgKind
	// Absent is what rx uses when the flag is left out.
	Absent AbsentValue
}

// AbsentValue is what rx uses when a flag is left out. A request value
// equal to it is not written. Exactly one of the three members applies.
type AbsentValue struct {
	// Value is the flag's default as cobra prints it (pflag's DefValue):
	// "3", "false", "4M". A request value is written the same way before
	// the two are compared.
	Value string
	// SameAs names another field of the same operation whose value rx
	// uses instead: `rx samples` takes --before from --context when
	// --before is left out. That field's request value, or its own
	// default when the request left it out, is the default here.
	SameAs string
	// NoValue says that leaving the flag out means something no value
	// spells, such as "read RX_LARGE_FILE_MB" for `rx index --threshold`.
	// Every value the request gives is then written, zero included.
	NoValue bool
}

// defaultValue, sameAsField and noValueDefault build the three kinds of
// AbsentValue, so the table below reads one line per field.
func defaultValue(value string) AbsentValue { return AbsentValue{Value: value} }
func sameAsField(field string) AbsentValue  { return AbsentValue{SameAs: field} }
func noValueDefault() AbsentValue           { return AbsentValue{NoValue: true} }

// cliCommandTable is the one description of how each HTTP operation
// renders its rx command. The defaults are those of the cobra flags in
// internal/clicommand; a test in cmd/rx compares them with the real
// command tree and parses every command the server renders.
var cliCommandTable = map[string]CLICommandOperation{
	"trace": {
		Subcommand: "trace",
		Args: []CLIArg{
			{Field: "path", Kind: ArgPositional},
			{Field: "regexp", Flag: "regexp", Kind: ArgFlag, Absent: defaultValue("[]")},
			{Field: "matching_flags", Kind: ArgSwitchList},
			{Field: "max_results", Flag: "max-results", Kind: ArgFlag, Absent: defaultValue("0")},
			{Field: "context", Flag: "context", Kind: ArgFlag, Absent: defaultValue("0")},
			{Field: "before_context", Flag: "before", Kind: ArgFlag, Absent: sameAsField("context")},
			{Field: "after_context", Flag: "after", Kind: ArgFlag, Absent: sameAsField("context")},
			{Field: "no_cache", Flag: "no-cache", Kind: ArgFlag, Absent: defaultValue("false")},
			{Field: "no_index", Flag: "no-index", Kind: ArgFlag, Absent: defaultValue("false")},
			{Field: "no_recursive", Flag: "no-recursive", Kind: ArgFlag, Absent: defaultValue("false")},
			{Field: "request_id", Flag: "request-id", Kind: ArgFlag, Absent: defaultValue("")},
			{Field: "hook_on_file", Flag: "hook-on-file", Kind: ArgFlag, Absent: defaultValue("")},
			{Field: "hook_on_match", Flag: "hook-on-match", Kind: ArgFlag, Absent: defaultValue("")},
			{Field: "hook_on_complete", Flag: "hook-on-complete", Kind: ArgFlag, Absent: defaultValue("")},
		},
	},
	"samples": {
		Subcommand: "samples",
		Args: []CLIArg{
			{Field: "path", Kind: ArgPositional},
			{Field: "offsets", Flag: "offsets", Kind: ArgFlag, Absent: defaultValue("[]")},
			{Field: "lines", Flag: "lines", Kind: ArgFlag, Absent: defaultValue("[]")},
			{Field: "timestamps", Flag: "timestamps", Kind: ArgFlag, Absent: defaultValue("[]")},
			{Field: "file_tz", Flag: "file-tz", Kind: ArgFlag, Absent: defaultValue("")},
			{Field: "context", Flag: "context", Kind: ArgFlag, Absent: defaultValue("3")},
			{Field: "before_context", Flag: "before", Kind: ArgFlag, Absent: sameAsField("context")},
			{Field: "after_context", Flag: "after", Kind: ArgFlag, Absent: sameAsField("context")},
		},
	},
	"time_range": {
		Subcommand: "time-range",
		Args: []CLIArg{
			{Field: "path", Kind: ArgPositional},
			{Field: "file_tz", Flag: "file-tz", Kind: ArgFlag, Absent: defaultValue("")},
		},
	},
	"index_get": {
		Subcommand: "index",
		Args:       []CLIArg{{Field: "path", Kind: ArgPositional}},
		FixedFlags: []string{"--info", "--json"},
	},
	"index_post": {
		Subcommand: "index",
		Args: []CLIArg{
			{Field: "path", Kind: ArgPositional},
			{Field: "force", Flag: "force", Kind: ArgFlag, Absent: defaultValue("false")},
			{Field: "analyze", Flag: "analyze", Kind: ArgFlag, Absent: defaultValue("false")},
			{Field: "threshold", Flag: "threshold", Kind: ArgFlag, Absent: noValueDefault()},
			{Field: "analyze_window_lines", Flag: "analyze-window-lines", Kind: ArgFlag, Absent: defaultValue("0")},
		},
	},
	// The commands that read one log chain: GET /v1/logs/chain is
	// `rx logs show`, and `rx logs time-range` renders its own command
	// from the same table. Subcommand holds two words.
	"log_chain": {
		Subcommand: "logs show",
		Args: []CLIArg{
			{Field: "path", Kind: ArgPositional},
			{Field: "file_tz", Flag: "file-tz", Kind: ArgFlag, Absent: defaultValue("")},
			{Field: "fingerprint", Flag: "fingerprint", Kind: ArgFlag, Absent: defaultValue("")},
		},
	},
	"logs_time_range": {
		Subcommand: "logs time-range",
		Args: []CLIArg{
			{Field: "path", Kind: ArgPositional},
			{Field: "file_tz", Flag: "file-tz", Kind: ArgFlag, Absent: defaultValue("")},
		},
	},
	// The index task of a log chain is `rx logs index`.
	"logs_index": {
		Subcommand: "logs index",
		Args: []CLIArg{
			{Field: "path", Kind: ArgPositional},
			{Field: "force", Flag: "force", Kind: ArgFlag, Absent: defaultValue("false")},
		},
	},
	"compress": {
		Subcommand: "compress",
		Args: []CLIArg{
			{Field: "input_path", Kind: ArgPositional},
			{Field: "output_path", Flag: "output", Kind: ArgFlag, Absent: defaultValue("")},
			{Field: "frame_size", Flag: "frame-size", Kind: ArgFlag, Absent: defaultValue("4M")},
			{Field: "compression_level", Flag: "level", Kind: ArgFlag, Absent: defaultValue("3")},
			{Field: "build_index", Flag: "build-index", Kind: ArgFlag, Absent: defaultValue("true")},
			{Field: "force", Flag: "force", Kind: ArgFlag, Absent: defaultValue("false")},
		},
	},
}

// CLICommandOperations returns a copy of the table BuildCLICommand
// renders from, keyed by operation name, for a caller that checks it
// against the CLI's command tree.
func CLICommandOperations() map[string]CLICommandOperation {
	out := make(map[string]CLICommandOperation, len(cliCommandTable))
	for name, op := range cliCommandTable {
		op.Args = slices.Clone(op.Args)
		op.FixedFlags = slices.Clone(op.FixedFlags)
		out[name] = op
	}
	return out
}

// positionalWords quotes a string, or each element of a []string, as
// positional arguments.
func positionalWords(value any) []string {
	words := []string{}
	for _, item := range valueStrings(value) {
		words = append(words, output.Quote(item))
	}
	return words
}

// flagWords renders one non-positional field of op. It renders nothing
// when the request did not give the field, or gave the value rx uses
// without the flag.
func flagWords(op CLICommandOperation, arg CLIArg, params map[string]any) []string {
	values := valueStrings(params[arg.Field])
	if len(values) == 0 {
		return nil
	}
	if arg.Kind == ArgSwitchList {
		words := make([]string, 0, len(values))
		for _, name := range values {
			words = append(words, "--"+name)
		}
		return words
	}
	if isDefault(op, arg, values, params) {
		return nil
	}
	words := make([]string, 0, len(values))
	for _, value := range values {
		words = append(words, flagWord(arg.Flag, value, isBool(params[arg.Field])))
	}
	return words
}

// isDefault reports whether values, the written form of a request value,
// is what rx uses for arg when its flag is left out.
func isDefault(op CLICommandOperation, arg CLIArg, values []string, params map[string]any) bool {
	if arg.Absent.NoValue || len(values) != 1 {
		return false
	}
	return values[0] == absentString(op, arg, params)
}

// absentString is what rx uses for arg when its flag is left out,
// written the way valueStrings writes a request value.
func absentString(op CLICommandOperation, arg CLIArg, params map[string]any) string {
	if arg.Absent.SameAs == "" {
		return arg.Absent.Value
	}
	if given := valueStrings(params[arg.Absent.SameAs]); len(given) == 1 {
		return given[0]
	}
	for _, other := range op.Args {
		if other.Field == arg.Absent.SameAs {
			return absentString(op, other, params)
		}
	}
	return ""
}

// flagWord writes one --name=value. A true bool is the bare --name,
// which is how a person types a switch.
func flagWord(name, value string, isBoolValue bool) string {
	if isBoolValue && value == "true" {
		return "--" + name
	}
	return "--" + name + "=" + output.Quote(value)
}

// valueStrings writes a request value as strings: none when the request
// did not give it, one for a scalar, one per element for a []string.
func valueStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			return []string{typed}
		}
	case *string:
		if typed != nil && *typed != "" {
			return []string{*typed}
		}
	case int:
		return []string{strconv.Itoa(typed)}
	case *int:
		if typed != nil {
			return []string{strconv.Itoa(*typed)}
		}
	case bool:
		return []string{strconv.FormatBool(typed)}
	case *bool:
		if typed != nil {
			return []string{strconv.FormatBool(*typed)}
		}
	case []string:
		return typed
	}
	return nil
}

// isBool reports whether a request value is a bool or a *bool.
func isBool(value any) bool {
	switch value.(type) {
	case bool, *bool:
		return true
	}
	return false
}
