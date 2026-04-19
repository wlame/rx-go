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
// Matches rx-python/src/rx/cli_command_builder.py. The returned string
// is rendered under the "cli_command" field of every API response so
// users can reproduce the API call from the terminal.
//
// subcommand is one of: "trace", "samples", "index_get", "index_post",
// "compress".
func BuildCLICommand(subcommand string, params map[string]any) string {
	parts := []string{"rx"}

	switch subcommand {
	case "trace":
		parts = append(parts, "trace")
		parts = appendStringSliceFlag(parts, "--regexp", params["regexp"])
		parts = appendIntPtrFlag(parts, "--max-results", params["max_results"])
		parts = appendStringSlicePositional(parts, params["path"])
	case "samples":
		parts = append(parts, "samples")
		parts = appendStringFlag(parts, "--offsets", params["offsets"])
		parts = appendStringFlag(parts, "--lines", params["lines"])
		parts = appendIntPtrFlag(parts, "--context", params["context"])
		parts = appendIntPtrFlag(parts, "--before-context", params["before_context"])
		parts = appendIntPtrFlag(parts, "--after-context", params["after_context"])
		parts = appendStringPositional(parts, params["path"])
	case "index_get":
		parts = append(parts, "index")
		parts = appendStringPositional(parts, params["path"])
	case "index_post":
		parts = append(parts, "index")
		if v, ok := params["force"].(bool); ok && v {
			parts = append(parts, "--force")
		}
		if v, ok := params["analyze"].(bool); ok && v {
			parts = append(parts, "--analyze")
		}
		parts = appendStringPositional(parts, params["path"])
	case "compress":
		parts = append(parts, "compress")
		parts = appendStringFlag(parts, "--output", params["output_path"])
		parts = appendStringFlag(parts, "--frame-size", params["frame_size"])
		parts = appendIntPtrFlag(parts, "--level", params["compression_level"])
		parts = appendStringPositional(parts, params["input_path"])
	}
	return strings.Join(parts, " ")
}

// appendStringSliceFlag appends "--name X --name Y ..." for every
// element of a []string value. No-op when the value is nil or empty.
func appendStringSliceFlag(parts []string, name string, v any) []string {
	s, ok := v.([]string)
	if !ok || len(s) == 0 {
		return parts
	}
	for _, item := range s {
		parts = append(parts, name, output.Quote(item))
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

// appendStringFlag appends "--name value" when v is a non-empty string
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
	return append(parts, name, output.Quote(s))
}

// appendIntPtrFlag appends "--name N" when v is a non-nil *int, a
// non-zero int, or a wrapped any holding same.
func appendIntPtrFlag(parts []string, name string, v any) []string {
	switch x := v.(type) {
	case *int:
		if x != nil {
			return append(parts, name, fmt.Sprintf("%d", *x))
		}
	case int:
		if x != 0 {
			return append(parts, name, fmt.Sprintf("%d", x))
		}
	}
	return parts
}
