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

	"github.com/wlame/rx-go/internal/frontend"
	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/webapi"
)

// NewServeCommand builds the `rx serve` cobra command.
//
// Starts the HTTP server. Wires up search-root sandbox, ensures the
// rx-viewer frontend is downloaded (lazy: only when cache is empty),
// then blocks until SIGINT/SIGTERM.
//
// Parity with rx-python/src/rx/cli/serve.py — flag names and defaults
// match exactly so users can switch between binaries.
func NewServeCommand(out io.Writer, appVersion string) *cobra.Command {
	var (
		host         string
		port         int
		searchRoots  []string
		skipFrontend bool
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
			})
		},
	}
	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Host to bind to")
	cmd.Flags().IntVar(&port, "port", 7777, "Port to bind to")
	cmd.Flags().StringArrayVar(&searchRoots, "search-root", nil,
		"Restrict file access to this directory (repeatable; default: current dir)")
	cmd.Flags().BoolVar(&skipFrontend, "skip-frontend", false,
		"Don't try to download the rx-viewer SPA — /docs still works")
	return cmd
}

type serveParams struct {
	host         string
	port         int
	searchRoots  []string
	appVersion   string
	skipFrontend bool
}

// runServe wires everything up and blocks.
func runServe(out io.Writer, p serveParams) error {
	// Configure slog level from RX_LOG_LEVEL and publish the value to
	// webapi so /health can report accurately (see Stage 8 Reviewer 3
	// High #13). Keep slog.Default() as the handler so we don't
	// disrupt callers that set their own handler — we only adjust the
	// level-reporting side channel.
	configureLogLevelFromEnv()

	// Resolve + apply search roots. Default: CWD.
	rootsToSet := p.searchRoots
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
		_ = exitWithError(os.Stderr, ExitUsageError, "search roots: %s", err.Error())
		return err
	}
	// Publish to env so any subprocess we spawn inherits the sandbox.
	_ = os.Setenv("RX_SEARCH_ROOTS", strings.Join(resolvedRoots, string(os.PathListSeparator)))

	// Lookup ripgrep once (affects health + 503 responses).
	rgPath, _ := exec.LookPath("rg")

	// Prepare the frontend cache. Best-effort: a download failure on a
	// corporate network shouldn't stop the server — rx-viewer degrades
	// gracefully (SPA fallback → /docs).
	fm := frontend.NewManager(frontend.Config{})
	if !p.skipFrontend {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := fm.Ensure(ctx); err != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"Warning: frontend fetch failed (%v). Continuing without SPA.\n", err)
		}
		cancel()
	}

	// Hook dispatcher wired to env-configured URLs.
	hookDisp := hooks.NewDispatcher(hooks.DispatcherConfig{
		Env: hooks.HookEnvFromEnv(),
	})

	// Build server.
	srv := webapi.NewServer(webapi.Config{
		Host:        p.host,
		Port:        p.port,
		AppVersion:  p.appVersion,
		RipgrepPath: rgPath,
		Frontend:    fm,
		Hooks:       hookDisp,
		Logger:      slog.Default(),
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
