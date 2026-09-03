package clicommand

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// TestServe_RefusesInternalHookURLFromEnv asserts an env-configured
// webhook aimed at an internal address stops the server at startup.
// Failing fast beats starting a server whose every trace request then
// answers 400, and beats silently dropping the operator's hook.
func TestServe_RefusesInternalHookURLFromEnv(t *testing.T) {
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", "http://169.254.169.254/latest/meta-data/")
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")
	t.Cleanup(paths.Reset)

	var buf bytes.Buffer
	err := runServe(&buf, serveParams{
		host:         "127.0.0.1",
		port:         0,
		searchRoots:  []string{t.TempDir()},
		appVersion:   "test",
		skipFrontend: true,
	})

	if err == nil {
		t.Fatalf("runServe: got nil error, want a refusal to start")
	}
	if !strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("error should name the offending URL: %v", err)
	}
}

// TestServe_AcceptsPublicHookURLFromEnv is the positive control: a
// public hook URL must not block startup. The server is not actually
// run — validation happens before the listener opens, so reaching the
// listen step is proof enough.
func TestServe_AcceptsPublicHookURLFromEnv(t *testing.T) {
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", "https://example.com/hook")
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")

	if err := validateEnvHooks(); err != nil {
		t.Errorf("validateEnvHooks: %v", err)
	}
}

// TestTrace_RefusesInternalHookURLFlag covers the CLI flags: a hook
// URL that the HTTP layer would reject with 400 must not be accepted
// from the command line either.
func TestTrace_RefusesInternalHookURLFlag(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")
	dir := t.TempDir()
	file := filepath.Join(dir, "a.log")
	if err := os.WriteFile(file, []byte("error here\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cases := []struct {
		name string
		p    traceParams
	}{
		{"on_complete", traceParams{hookOnComplete: "http://127.0.0.1/x"}},
		{"on_file", traceParams{hookOnFile: "http://169.254.169.254/"}},
		{"on_match", traceParams{hookOnMatch: "http://10.0.0.1/x", maxResults: 1}},
		{"bad_scheme", traceParams{hookOnComplete: "ftp://example.com/x"}},
		{"credentials", traceParams{hookOnComplete: "http://u:p@example.com/x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			p.regexps = []string{"error"}
			p.args = []string{file}

			var buf bytes.Buffer
			err := runTrace(&buf, p)
			if err == nil {
				t.Fatalf("runTrace: got nil error, want a rejection")
			}
		})
	}
}

// TestTrace_AcceptsPublicHookURLFlag is the positive control for the
// flag validation: a public URL is not rejected up front.
func TestTrace_AcceptsPublicHookURLFlag(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")
	dir := t.TempDir()
	file := filepath.Join(dir, "a.log")
	if err := os.WriteFile(file, []byte("error here\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var buf bytes.Buffer
	err := runTrace(&buf, traceParams{
		regexps:        []string{"error"},
		args:           []string{file},
		hookOnComplete: "https://example.com/hook",
	})
	if err != nil {
		t.Errorf("runTrace: %v (%s)", err, buf.String())
	}
}
