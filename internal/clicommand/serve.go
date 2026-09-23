package clicommand

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/frontend"
	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/webapi"
)

// NewServeCommand builds the `rx serve` cobra command.
//
// Starts the HTTP server. Wires up search-root sandbox, makes sure the
// rx-viewer frontend is on disk (downloaded when the cache is empty or
// outside the supported range, refreshed when a newer release inside the
// range is out and the last check is a day old, or now with
// --update-viewer), then blocks until SIGINT/SIGTERM.
//
// Parity with rx-python/src/rx/cli/serve.py — flag names and defaults
// match exactly so users can switch between binaries.
func NewServeCommand(out io.Writer, appVersion string) *cobra.Command {
	var (
		host         string
		port         int
		searchRoots  []string
		skipFrontend bool
		updateViewer bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the rx-tool HTTP API server",
		Long: "Launches the HTTP API server on --host:--port with the " +
			"rx-viewer SPA and Prometheus metrics. SIGINT or SIGTERM triggers a " +
			"graceful shutdown with a 10s drain.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(out, serveParams{
				host:         host,
				port:         port,
				searchRoots:  searchRoots,
				appVersion:   appVersion,
				skipFrontend: skipFrontend,
				updateViewer: updateViewer,
			})
		},
	}
	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Host to bind to")
	cmd.Flags().IntVar(&port, "port", 7777, "Port to bind to")
	cmd.Flags().StringArrayVar(&searchRoots, "search-root", nil,
		"Restrict file access to this directory (repeatable; default: current dir)")
	cmd.Flags().BoolVar(&skipFrontend, "skip-frontend", false,
		"Don't try to download the rx-viewer SPA — /docs still works")
	cmd.Flags().BoolVar(&updateViewer, "update-viewer", false,
		"Check GitHub for a newer rx-viewer release inside the supported range now, "+
			"instead of once a day")
	return cmd
}

// validateEnvHooks runs the hook-URL guard over RX_HOOK_ON_*_URL.
// Returns the first failure, or nil when no env hook is configured.
func validateEnvHooks() error {
	env := hooks.HookEnvFromEnv()
	return hooks.ValidateConfig(hooks.HookConfig{
		OnFileURL:     env.OnFileURL,
		OnMatchURL:    env.OnMatchURL,
		OnCompleteURL: env.OnCompleteURL,
	})
}

type serveParams struct {
	host         string
	port         int
	searchRoots  []string
	appVersion   string
	skipFrontend bool
	updateViewer bool
}

// viewerMode is how `rx serve` manages the viewer at start-up.
type viewerMode int

const (
	// viewerCheckDaily: the default — install when missing, check for a
	// newer release when the last check is a day old.
	viewerCheckDaily viewerMode = iota
	// viewerCheckNow: --update-viewer — check now.
	viewerCheckNow
	// viewerUnmanaged: --skip-frontend — serve what is on disk, ask nothing.
	viewerUnmanaged
)

// viewerModeFor maps the two flags to a mode. Together they contradict
// each other, which is a usage error.
func viewerModeFor(skipFrontend, updateViewer bool) (viewerMode, error) {
	switch {
	case skipFrontend && updateViewer:
		return 0, fmt.Errorf("--update-viewer and --skip-frontend cannot be used together")
	case skipFrontend:
		return viewerUnmanaged, nil
	case updateViewer:
		return viewerCheckNow, nil
	default:
		return viewerCheckDaily, nil
	}
}

// viewerStartTimeout bounds the whole viewer step of the start-up: a
// first install or an update download. The release listing of a daily
// check has its own, shorter bound (frontend.ReleaseCheckTimeout).
const viewerStartTimeout = 60 * time.Second

// prepareViewer runs the viewer step of `rx serve` and returns what will
// be served. It never fails the start: a failed download or check is
// written to stderr as one warning line that also says what is served
// instead (the cached viewer, or none), and the server starts either way.
func prepareViewer(fm *frontend.Manager, mode viewerMode, stderr io.Writer) frontend.Served {
	if mode == viewerUnmanaged {
		return fm.Cached()
	}
	ensure := fm.Ensure
	if mode == viewerCheckNow {
		ensure = fm.Update
	}
	ctx, cancel := context.WithTimeout(context.Background(), viewerStartTimeout)
	defer cancel()
	served, err := ensure(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: %v. Serving %s.\n", err, served.Describe())
	}
	return served
}

// runServe wires everything up and blocks.
func runServe(out io.Writer, p serveParams) error {
	// Read the level from RX_LOG_LEVEL and publish it to webapi, so
	// /health reports the configured level. slog.Default() stays the
	// handler, so callers that set their own handler are not disturbed;
	// only the level-reporting side channel changes.
	configureLogLevelFromEnv()

	mode, err := viewerModeFor(p.skipFrontend, p.updateViewer)
	if err != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	}

	// SECURITY: refuse to start when an env-configured webhook points
	// somewhere the SSRF guard would block. Checked before anything
	// else is set up, so a misconfigured server never reaches the
	// listener. Starting anyway would report the same problem once per
	// trace request instead of once at startup;
	// RX_ALLOW_INTERNAL_HOOKS is the documented opt-in for a genuinely
	// internal collector.
	if err := validateEnvHooks(); err != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	}

	// Resolve + apply search roots. The flag wins, then RX_SEARCH_ROOTS
	// — which is how a parent rx passes its own sandbox down — and the
	// current directory is the last resort.
	rootsToSet := p.searchRoots
	if len(rootsToSet) == 0 {
		rootsToSet = config.GetPathSepEnv("RX_SEARCH_ROOTS")
	}
	if len(rootsToSet) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve cwd: %w", err)
		}
		rootsToSet = []string{cwd}
	}
	// Expand to absolute. SetSearchRoots rejects non-directories.
	resolvedRoots := make([]string, 0, len(rootsToSet))
	for _, r := range rootsToSet {
		abs, err := filepath.Abs(r)
		if err != nil {
			return fmt.Errorf("abs %s: %w", r, err)
		}
		resolvedRoots = append(resolvedRoots, abs)
	}
	if err := paths.SetSearchRoots(resolvedRoots); err != nil {
		return exitWithError(os.Stderr, ExitUsageError, "search roots: %s", err.Error())
	}
	// Publish to env so any subprocess we spawn inherits the sandbox.
	_ = os.Setenv("RX_SEARCH_ROOTS", strings.Join(resolvedRoots, string(os.PathListSeparator)))

	// A bind other machines can reach, or a root as wide as $HOME or /,
	// is allowed but said out loud before the server comes up.
	apiToken := os.Getenv("RX_API_TOKEN")
	home, _ := os.UserHomeDir()
	for _, warning := range serveWarnings(p.host, resolvedRoots, home, apiToken != "") {
		_, _ = fmt.Fprintln(os.Stderr, warning)
	}

	// Lookup ripgrep once (affects health + 503 responses).
	rgPath, _ := exec.LookPath("rg")

	// Prepare the frontend cache. Best-effort: a download failure on a
	// corporate network shouldn't stop the server — rx-viewer degrades
	// gracefully (SPA fallback → /docs). It runs before the listener
	// binds, so the bundle never changes under a request.
	fm := frontend.NewManager(frontend.Config{})
	served := prepareViewer(fm, mode, os.Stderr)

	// One hook dispatcher (queue, HTTP client, workers) for the whole
	// server. It holds no URLs: each /v1/trace request resolves its own
	// from RX_HOOK_ON_*_URL and its hook_on_* parameters and passes them
	// in through Dispatcher.ForRequest.
	hookDisp := hooks.NewDispatcher(hooks.DispatcherConfig{})

	// Build server.
	srv := webapi.NewServer(webapi.Config{
		Host:        p.host,
		Port:        p.port,
		AppVersion:  p.appVersion,
		RipgrepPath: rgPath,
		Frontend:    fm,
		Hooks:       hookDisp,
		Logger:      slog.Default(),
		APIToken:    apiToken,
	})

	// Nice startup banner — mirrors Python output.
	_, _ = fmt.Fprintf(out, "Starting RX API server on http://%s:%d\n", p.host, p.port)
	if len(resolvedRoots) == 1 {
		_, _ = fmt.Fprintf(out, "Search root: %s\n", resolvedRoots[0])
	} else {
		_, _ = fmt.Fprintf(out, "Search roots (%d):\n", len(resolvedRoots))
		for _, r := range resolvedRoots {
			_, _ = fmt.Fprintf(out, "  - %s\n", r)
		}
	}
	_, _ = fmt.Fprintf(out, "Viewer: %s\n", served.Describe())
	if apiToken != "" {
		_, _ = fmt.Fprintln(out, "API token: required on /v1 (RX_API_TOKEN is set)")
	}
	_, _ = fmt.Fprintf(out, "API docs available at http://%s:%d/docs\n", p.host, p.port)
	_, _ = fmt.Fprintf(out, "Metrics available at http://%s:%d/metrics\n", p.host, p.port)
	_, _ = fmt.Fprintln(out, "")

	// Run in a goroutine; main thread handles signal.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	// Wait for either startup failure or shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("server failed: %w", err)
		}
		return nil
	case sig := <-sigCh:
		_, _ = fmt.Fprintf(out, "\nReceived %s, shutting down...\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Shutdown error: %v\n", err)
		}
		return nil
	}
}

// configureLogLevelFromEnv reads RX_LOG_LEVEL and publishes the resolved
// slog.Level to webapi.SetRequestedLogLevel so /health reports the
// actual configured level instead of a stale env echo. Called once at
// server startup; silent no-op if RX_LOG_LEVEL is empty.
//
// The recognized values match Python's logging levels: DEBUG, INFO,
// WARNING (alias for WARN), ERROR. Unknown values leave the reported
// level unset (webapi falls back to the env-string behavior).
func configureLogLevelFromEnv() {
	raw := strings.ToUpper(os.Getenv("RX_LOG_LEVEL"))
	if raw == "" {
		// Unset → default INFO. Publish explicitly so /health doesn't
		// need to branch on nil.
		lvl := slog.LevelInfo
		webapi.SetRequestedLogLevel(&lvl)
		return
	}
	var lvl slog.Level
	switch raw {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "INFO":
		lvl = slog.LevelInfo
	case "WARN", "WARNING":
		lvl = slog.LevelWarn
	case "ERROR":
		lvl = slog.LevelError
	default:
		// Unknown level string — leave unset, webapi will fall back to
		// echoing the env var verbatim (preserves legacy surface).
		return
	}
	webapi.SetRequestedLogLevel(&lvl)
}
