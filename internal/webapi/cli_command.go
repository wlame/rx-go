package webapi

import (
	"fmt"
	"strings"

	"github.com/wlame/rx-go/internal/output"
)

// BuildCLICommand returns the equivalent `rx ...` CLI invocation for a
// given endpoint + params. The returned string is suitable to paste
// into a terminal: it uses output.Quote (Python-shlex-compatible) to
// quote arguments that contain shell metacharacters.
//
// The rendering is `rx <subcommand> <positionals...> <--long=value...>`.
// Long flags take `=`, which is the project's own CLI convention, and a
// value that needs shell quoting is quoted after the `=`.
//
// rx-python renders the same string from the same request. The cases are
// in testdata/cli-commands.json, which rx-python holds a copy of at
// tests/data/cli-commands.json — the two files are edited together, so a
// change made on one side and not the other fails there.
//
// subcommand is one of: "trace", "samples", "index_get", "index_post",
// "compress".
func BuildCLICommand(subcommand string, params map[string]any) string {
	parts := []string{"rx"}

	switch subcommand {
	case "trace":
		parts = append(parts, "trace")
		parts = appendStringSlicePositional(parts, params["path"])
		parts = appendStringSliceFlag(parts, "--regexp", params["regexp"])
		parts = appendIntPtrFlag(parts, "--max-results", params["max_results"])
	case "samples":
		parts = append(parts, "samples")
		parts = appendStringPositional(parts, params["path"])
		parts = appendStringFlag(parts, "--offsets", params["offsets"])
		parts = appendStringFlag(parts, "--lines", params["lines"])
		parts = appendIntPtrFlag(parts, "--context", params["context"])
		parts = appendIntPtrFlag(parts, "--before-context", params["before_context"])
		parts = appendIntPtrFlag(parts, "--after-context", params["after_context"])
	case "index_get":
		parts = append(parts, "index")
		parts = appendStringPositional(parts, params["path"])
		// A GET reads the index and answers in JSON; the CLI equivalent
		// has to say so or it builds one instead of reading it.
		parts = append(parts, "--info", "--json")
	case "index_post":
		parts = append(parts, "index")
		parts = appendStringPositional(parts, params["path"])
		if v, ok := params["force"].(bool); ok && v {
			parts = append(parts, "--force")
		}
		if v, ok := params["analyze"].(bool); ok && v {
			parts = append(parts, "--analyze")
		}
	case "compress":
		parts = append(parts, "compress")
		parts = appendStringPositional(parts, params["input_path"])
		parts = appendStringFlag(parts, "--output", params["output_path"])
		parts = appendStringFlag(parts, "--frame-size", params["frame_size"])
		parts = appendIntPtrFlag(parts, "--level", params["compression_level"])
	}
	return strings.Join(parts, " ")
}

// appendStringSliceFlag appends "--name=X --name=Y ..." for every
// element of a []string value. No-op when the value is nil or empty.
func appendStringSliceFlag(parts []string, name string, v any) []string {
	s, ok := v.([]string)
	if !ok || len(s) == 0 {
		return parts
	}
	for _, item := range s {
		parts = append(parts, name+"="+output.Quote(item))
	}
	return parts
}

// appendStringSlicePositional appends each string in v as a positional.
func appendStringSlicePositional(parts []string, v any) []string {
	s, ok := v.([]string)
	if !ok {
		return parts
	}
	for _, item := range s {
		parts = append(parts, output.Quote(item))
	}
	return parts
}

// appendStringPositional appends v (if non-empty) as a single positional.
func appendStringPositional(parts []string, v any) []string {
	s, ok := v.(string)
	if !ok || s == "" {
		return parts
	}
	return append(parts, output.Quote(s))
}

// appendStringFlag appends "--name=value" when v is a non-empty string
// or *string pointing to a non-empty value.
func appendStringFlag(parts []string, name string, v any) []string {
	s := ""
	switch x := v.(type) {
	case string:
		s = x
	case *string:
		if x != nil {
			s = *x
		}
	}
	if s == "" {
		return parts
	}
	return append(parts, name+"="+output.Quote(s))
}

// appendIntPtrFlag appends "--name=N" when v is a non-nil *int or a
// non-zero int.
//
// A *int distinguishes "not supplied" from zero, so an explicit
// --context=0 is rendered rather than dropped: it is a request for no
// context lines, which is different from not asking. A bare int has no
// way to say that, so zero there still means "not supplied".
func appendIntPtrFlag(parts []string, name string, v any) []string {
	switch x := v.(type) {
	case *int:
		if x != nil {
			return append(parts, fmt.Sprintf("%s=%d", name, *x))
		}
	case int:
		if x != 0 {
			return append(parts, fmt.Sprintf("%s=%d", name, x))
		}
	}
	return parts
}
