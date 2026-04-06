package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wlame/rx/internal/api"
	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/frontend"
)

// newServeCommand creates the "serve" subcommand that starts a web API server.
func newServeCommand() *cobra.Command {
	var (
		host        string
		port        int
		searchRoots []string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the RX web API server",
		Long: `Start the RX web API server with REST endpoints for search, samples,
index management, and more.

Examples:
  rx serve
  rx serve --port=8080
  rx serve --host=0.0.0.0 --port=4799
  rx serve --search-root=/var/log --search-root=/home/data`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd, serveFlags{
				host:        host,
				port:        port,
				searchRoots: searchRoots,
			})
		},
	}

	cmd.Flags().StringVar(&host, "host", "0.0.0.0", "Host to bind to")
	cmd.Flags().IntVar(&port, "port", 4799, "Port to bind to")
	cmd.Flags().StringSliceVar(&searchRoots, "search-root", nil, "Root directory for searches (repeatable)")

	return cmd
}

// serveFlags groups serve command flags.
type serveFlags struct {
	host        string
	port        int
	searchRoots []string
}

// runServe starts the HTTP server with graceful shutdown on SIGINT/SIGTERM.
func runServe(cmd *cobra.Command, flags serveFlags) error {
	// Set search roots in the environment so config.Load() picks them up.
	if len(flags.searchRoots) > 0 {
		os.Setenv("RX_SEARCH_ROOTS", joinSearchRoots(flags.searchRoots))
	}

	// Load configuration from environment.
	cfg := config.Load()

	// Default to current working directory if no search roots configured
	// (matches Python's behavior: set_search_root(None) → cwd).
	if len(cfg.SearchRoots) == 0 {
		cwd, err := os.Getwd()
		if err == nil {
			cfg.SearchRoots = []string{cwd}
			slog.Info("no search root configured, defaulting to current directory", "root", cwd)
		}
	}

	// Propagate version to the API package so /health reports it.
	api.SetVersion(version)

	// Download/update frontend SPA if needed (non-blocking on failure).
	fm := frontend.NewManager(&cfg)
	frontendDir, err := fm.EnsureFrontend()
	if err != nil {
		slog.Warn("frontend: setup failed, SPA will not be available", "error", err)
	} else if frontendDir != "" {
		slog.Info("frontend: ready", "dir", frontendDir)
	}

	// Create the fully-wired API server with all endpoint handlers.
	srv := api.NewServer(&cfg)
	srv.FrontendDir = frontendDir

	addr := fmt.Sprintf("%s:%d", flags.host, flags.port)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown: listen for SIGINT/SIGTERM, then shut down the server
	// with a timeout to let in-flight requests complete.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("RX server listening", "addr", addr)
		fmt.Fprintf(cmd.ErrOrStderr(), "RX server listening on %s\n", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Block until shutdown signal or server error.
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
		fmt.Fprintln(cmd.ErrOrStderr(), "Shutting down server...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server shutdown: %w", err)
		}
		return nil

	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("server: %w", err)
		}
		return nil
	}
}

// joinSearchRoots joins search root paths with os.PathListSeparator.
func joinSearchRoots(roots []string) string {
	sep := string(os.PathListSeparator)
	result := ""
	for i, r := range roots {
		if i > 0 {
			result += sep
		}
		result += r
	}
	return result
}

// StartTestServer starts the API server on a random port and returns the
// listener address and a shutdown function. Used by tests to verify the
// server starts and responds correctly without binding to a fixed port.
func StartTestServer() (addr string, shutdown func(), err error) {
	cfg := config.Load()
	api.SetVersion(version)
	srv := api.NewServer(&cfg)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}

	httpSrv := &http.Server{
		Handler:           srv.Router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		_ = httpSrv.Serve(listener)
	}()

	shutdownFn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}

	return listener.Addr().String(), shutdownFn, nil
}
