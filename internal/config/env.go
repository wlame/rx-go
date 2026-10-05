// Package config centralizes environment-variable parsing and cache-directory
// resolution. It deliberately has NO dependencies on other internal/
// packages — any other package that needs an env value or a cache dir
// goes through here, giving us one place to patch for tests.
package config

import (
	"os"
	"strings"
)

// GetStringEnv returns the value of the named environment variable, or
// def if unset. An empty string is treated as "set" and returned as-is.
func GetStringEnv(name, def string) string {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	return v
}

// GetBoolEnv parses the env var as a boolean. Recognized truthy values:
// "true", "yes", "1", "on" (case-insensitive). Falsy: "false", "no", "0",
// "off". Anything else (including unset) falls back to def.
//
// Python parity: get_bool_env accepts true/false, yes/no, 1/0; the Go
// port adds "on"/"off" which match typical unix tooling idioms without
// breaking Python compat.
func GetBoolEnv(name string, def bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "true", "yes", "1", "on":
		return true
	case "false", "no", "0", "off":
		return false
	default:
		return def
	}
}

// GetPathSepEnv splits a PATH-style environment variable (colon-separated
// on Unix, semicolon-separated on Windows) into a non-empty list of paths.
// Used for RX_SEARCH_ROOTS which may hold multiple absolute paths.
func GetPathSepEnv(name string) []string {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, string(os.PathListSeparator))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
