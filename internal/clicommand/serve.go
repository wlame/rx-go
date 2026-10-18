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
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/frontend"
	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
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
			treeRefresh, err := treeRefreshSeconds(cmd)
			if err != nil {
				return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
			}
			return runServe(out, serveParams{
				host:               host,
				port:               port,
				searchRoots:        searchRoots,
				appVersion:         appVersion,
				skipFrontend:       skipFrontend,
				updateViewer:       updateViewer,
				maskSecrets:        maskSecretsMode(cmd),
				treeRefreshSeconds: treeRefresh,
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
	bindMaskSecretsFlag(cmd)
	cmd.Flags().Int(treeRefreshFlag, config.DefaultTreeRefreshSeconds,
		"How often, in seconds, the viewer reads the open folders of its files list again; 0 turns it off. "+
			"Without the flag: RX_TREE_REFRESH_SECONDS, else the default")
	return cmd
}

// treeRefreshFlag is the flag of `rx serve` that sets how often the
// viewer reads the loaded folders of its files list again.
const treeRefreshFlag = "tree-refresh"

// treeRefreshSeconds is the refresh interval of a run of cmd: the value
// of --tree-refresh when the command line gives it (0 included, which
// turns the refresh off), otherwise RX_TREE_REFRESH_SECONDS
// (config.TreeRefreshSeconds). A flag value outside the setting's range
// is an error that names the range. The variable follows the rule of
// every integer setting instead: a warning, then the default or the
// maximum.
func treeRefreshSeconds(cmd *cobra.Command) (int, error) {
	// Go note: Changed tells a flag the user wrote from one left at its
	// default; only then does the flag win over the variable.
	if !cmd.Flags().Changed(treeRefreshFlag) {
		return config.TreeRefreshSeconds(), nil
	}
	// GetInt fails only for a flag that is not a registered int;
	// NewServeCommand registered it, so the error is nil here.
	seconds, _ := cmd.Flags().GetInt(treeRefreshFlag)
	setting := config.TreeRefreshSecondsSetting
	if seconds < setting.Min || seconds > setting.Max {
		return 0, fmt.Errorf("--%s must be from %d to %d seconds, got %d", treeRefreshFlag, setting.Min, setting.Max, seconds)
	}
	return seconds, nil
}

// validateEnvHooks reads the hook environment once (the URLs and the
// address policy) and runs the hook-URL guard over RX_HOOK_ON_*_URL
// under that policy. Returns the snapshot and the first failure, or
// nil when every env hook passes. The server is built from the same
// snapshot, so what it validates, dials and reports is one reading.
func validateEnvHooks() (hooks.HookEnv, error) {
	env := hooks.HookEnvFromEnv()
	return env, hooks.ValidateConfig(env.EnvConfig(), env.Policy)
}

type serveParams struct {
	host         string
	port         int
	searchRoots  []string
	appVersion   string
	skipFrontend bool
	updateViewer bool
	// maskSecrets is the server's masking mode, for every findings
	// answer it gives: --mask-secrets, else RX_MASK_SECRETS
	// (maskSecretsMode).
	maskSecrets bool
	// treeRefreshSeconds is how often the viewer reads the loaded
	// folders of its files list again, 0 for never: --tree-refresh, else
	// RX_TREE_REFRESH_SECONDS (treeRefreshSeconds). /health gives it to
	// the viewer.
	treeRefreshSeconds int
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
// each other, which is a usage error. So is --update-viewer when
// servesLocalPath says RX_FRONTEND_PATH names the viewer: rx serves that
// directory as it is, so there is nothing to update.
func viewerModeFor(skipFrontend, updateViewer, servesLocalPath bool) (viewerMode, error) {
	switch {
	case skipFrontend && updateViewer:
		return 0, fmt.Errorf("--update-viewer and --skip-frontend cannot be used together")
	case updateViewer && servesLocalPath:
		return 0, fmt.Errorf("--update-viewer has nothing to update: RX_FRONTEND_PATH names a viewer build rx serves as it is")
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
// written to stderr as one warning line that ends with the viewer served
// instead (the cached one, or none), and the server starts either way.
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
		_, _ = fmt.Fprintf(stderr, "Warning: %v. Viewer: %s.\n", err, served.Describe())
	}
	return served
}

// runServe wires everything up and blocks.
func runServe(out io.Writer, p serveParams) error {
	// The stop signals are taken first, before the startup work (the
	// viewer download, the signature catalogue), so a SIGINT or SIGTERM
	// that arrives during the startup is kept and stops the server as
	// soon as it is up, instead of going unheard.
	//
	// Go note: signal.Notify sends each signal to every channel
	// registered for it; the buffer of one keeps a signal that arrives
	// before the select below reads it. signal.Stop unregisters the
	// channel when runServe returns, so a later runServe in the same
	// process gets its own signals and no stale channel takes them.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// The manager only reads the environment here; it touches the disk
	// and the network later, in prepareViewer.
	fm := frontend.NewManager(frontend.Config{})
	mode, err := viewerModeFor(p.skipFrontend, p.updateViewer, fm.ServesLocalPath())
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
	hookEnv, err := validateEnvHooks()
	if err != nil {
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
	served := prepareViewer(fm, mode, os.Stderr)

	// The signature catalogue, loaded once: the findings routes serve
	// it until the server stops, so a change to a signature file takes
	// a restart. Load errors do not stop the start; each is logged, and
	// GET /v1/signatures lists them.
	catalogue := loadServeSignatures(slog.Default())
	// Every setting of the scans and of the findings cache is read here,
	// once; internal/findings and internal/sigscan read no environment.
	findingsOptions := serveFindingsOptions(p)
	findings.SetMaxScans(config.MaxSignatureScans())
	// GET /v1/findings reads one file's findings on every page, filter
	// and key press; the decoded answers of the last few files read are
	// kept (findings.SetReadCache: never stale, bounded).
	findings.SetReadCache(serveFindingsReadCacheFiles, serveFindingsReadCacheBytes)

	// One hook dispatcher (queue, HTTP client, workers) for the whole
	// server. It holds no URLs: each /v1/trace request resolves its own
	// from RX_HOOK_ON_*_URL and its hook_on_* parameters and passes them
	// in through Dispatcher.ForRequest.
	hookDisp := hooks.NewDispatcher(hooks.DispatcherConfig{Policy: &hookEnv.Policy})

	// Build server.
	srv := webapi.NewServer(webapi.Config{
		Host:        p.host,
		Port:        p.port,
		AppVersion:  p.appVersion,
		RipgrepPath: rgPath,
		Frontend:    fm,
		Hooks:       hookDisp,
		HookEnv:     &hookEnv,
		Logger:      slog.Default(),
		APIToken:    apiToken,
		Signatures:  catalogue,
		Findings:    &findingsOptions,
		// Go note: &p.treeRefreshSeconds points into this call's own
		// copy of p, which lives as long as the server needs it.
		TreeRefreshSeconds: &p.treeRefreshSeconds,
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
	// The banner reads the mode from the options the server is given,
	// so it names the mode the answers have.
	_, _ = fmt.Fprintln(out, secretsBannerLine(findingsOptions.Scan.MaskSecrets))
	_, _ = fmt.Fprintln(out, treeRefreshBannerLine(p.treeRefreshSeconds))
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

// serveFindingsOptions are the settings of every findings answer of a
// server started with p: the environment's (webapi.FindingsOptionsFromEnv,
// RX_MASK_SECRETS included) with the masking mode of the command line,
// which wins over the variable (p.maskSecrets, from maskSecretsMode).
func serveFindingsOptions(p serveParams) findings.TraceOptions {
	opts := webapi.FindingsOptionsFromEnv()
	opts.Scan.MaskSecrets = p.maskSecrets
	return opts
}

// secretsBannerLine is the line of the start banner that says whether
// the server masks secrets in the text of findings, and how to turn
// masking on when it does not.
func secretsBannerLine(maskSecrets bool) string {
	if maskSecrets {
		return "Secrets: masked"
	}
	return "Secrets: shown; start with --mask-secrets or " + config.MaskingEnv + "=1 to mask them"
}

// treeRefreshBannerLine is the line of the start banner that says how
// often the viewer reads the loaded folders of its files list again.
func treeRefreshBannerLine(seconds int) string {
	if seconds == 0 {
		return "Tree refresh: off"
	}
	return fmt.Sprintf("Tree refresh: every %d s", seconds)
}

// The bounds of the findings read cache of `rx serve`: the decoded
// findings of at most 8 files and 64 MiB, the least recently read file
// out first.
const (
	serveFindingsReadCacheFiles = 8
	serveFindingsReadCacheBytes = 64 << 20
)

// loadServeSignatures loads the signature catalogue `rx serve` serves:
// the built-ins and the user's files on the search path, every example
// checked by the scan engine, as `rx signatures` loads them. Each load
// error is logged as a warning that names its file, line and id.
func loadServeSignatures(logger *slog.Logger) *signatures.Catalogue {
	cat := signatures.Load(signatures.LoadOptions{CheckExamples: sigscan.CheckExamples})
	for _, e := range cat.Errors() {
		logger.Warn("signature_load_error", "file", e.File, "line", e.Line, "id", e.ID, "message", e.Message)
	}
	return cat
}
