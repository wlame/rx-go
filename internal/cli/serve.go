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

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
)

// newServeCommand creates the "serve" subcommand that starts a web API server.
//
// For Phase 5 this is a minimal server with only a /health endpoint. Full API
// endpoint wiring happens in Phase 6.
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

	// Build the chi router with a basic /health endpoint.
	// Full API wiring is deferred to Phase 6.
	r := chi.NewRouter()
	r.Get("/health", healthHandler)

	addr := fmt.Sprintf("%s:%d", flags.host, flags.port)

	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
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
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

		if err := srv.Shutdown(shutdownCtx); err != nil {
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

// healthHandler responds with a simple JSON health check.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, version)
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

// StartTestServer starts the serve command's HTTP handler on a random port and
// returns the listener address and a shutdown function. Used by tests to verify
// the server starts and responds correctly without binding to a fixed port.
func StartTestServer() (addr string, shutdown func(), err error) {
	r := chi.NewRouter()
	r.Get("/health", healthHandler)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}

	srv := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		_ = srv.Serve(listener)
	}()

	shutdownFn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}

	return listener.Addr().String(), shutdownFn, nil
}
