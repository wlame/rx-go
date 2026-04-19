// Package frontend manages the rx-viewer SPA bundle — downloading,
// caching, and validating static-file paths at request time.
//
// The Python implementation at rx-python/src/rx/frontend_manager.py
// fetches dist.tar.gz from GitHub releases. rx-go keeps the same
// behavior, the same env vars, the same on-disk layout, and the
// same .metadata.json schema so a cache written by one toolchain
// loads in the other.
//
// Behavior (matches Python exactly):
//
//   - RX_FRONTEND_URL set → force download from that URL (every startup).
//   - RX_FRONTEND_VERSION set → pin to that version; use cached if
//     present, else download.
//   - Neither set + valid cache present → use cache, no GitHub hit.
//   - Neither set + no cache → download latest from GitHub.
//
// Directory resolution: RX_FRONTEND_PATH > config.GetFrontendCacheDir().
//
// Security: tarball entries are path-sanitized before extraction.
// Any entry whose resolved path would escape the destination
// directory (`..`, absolute paths, symlink traversal) is rejected,
// the tarball is abandoned, and an error is returned.
package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wlame/rx-go/internal/config"
)

// ============================================================================
// Configuration
// ============================================================================

// GitHubRepo is the rx-viewer source repository. Constant because
// rx-go shouldn't be reassembled against a different viewer — the
// API contract is pinned.
const GitHubRepo = "wlame/rx-viewer"

// githubAPIBase is the REST API root. Overridden in tests via Manager.APIBase.
const githubAPIBase = "https://api.github.com"

// RequestTimeout applies to GitHub API calls and tarball downloads.
const RequestTimeout = 30 * time.Second

// ============================================================================
// Metadata
// ============================================================================

// Version is the SPA build identifier embedded in dist/version.json.
// Serialization keys match the Python schema for cross-toolchain
// compat — CamelCase in Go, preserving Python's mixedCase JSON.
type Version struct {
	Version   string `json:"version"`
	BuildDate string `json:"buildDate"`
	Commit    string `json:"commit"`
}

// CacheMetadata is the sidecar .metadata.json we write next to the
// extracted SPA. Lets rx-viewer introspect "what version is cached"
// without parsing the dist files.
type CacheMetadata struct {
	Version      Version `json:"version"`
	DownloadedAt string  `json:"downloaded_at"`
	LastCheck    string  `json:"last_check"`
	ReleaseURL   string  `json:"release_url"`
}

// ============================================================================
// Manager
// ============================================================================

// Manager encapsulates the download/cache/serve behavior of the
// frontend bundle. One Manager per process is the norm (the Python
// version uses a module-level singleton).
type Manager struct {
	CacheDir string
	Repo     string
	APIBase  string

	// Env var snapshots.
	envURL     string
	envVersion string

	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Config collects the knobs that can be overridden in tests. Nil
// fields take env/default values via NewManager.
type Config struct {
	CacheDir   string
	Repo       string
	APIBase    string
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// NewManager builds a Manager with env-var and default fallbacks.
// Safe to call multiple times; each Manager has its own state.
func NewManager(cfg Config) *Manager {
	m := &Manager{
		CacheDir:   cfg.CacheDir,
		Repo:       cfg.Repo,
		APIBase:    cfg.APIBase,
		HTTPClient: cfg.HTTPClient,
		Logger:     cfg.Logger,
	}
	if m.CacheDir == "" {
		if v := os.Getenv("RX_FRONTEND_PATH"); v != "" {
			m.CacheDir = expandHome(v)
		} else {
			m.CacheDir = config.GetFrontendCacheDir()
		}
	}
	if m.Repo == "" {
		m.Repo = GitHubRepo
	}
	if m.APIBase == "" {
		m.APIBase = githubAPIBase
	}
	if m.HTTPClient == nil {
		m.HTTPClient = &http.Client{Timeout: RequestTimeout}
	}
	if m.Logger == nil {
		m.Logger = slog.Default()
	}
	m.envURL = os.Getenv("RX_FRONTEND_URL")
	m.envVersion = os.Getenv("RX_FRONTEND_VERSION")
	return m
}

// expandHome replaces a leading "~" with the user's home dir. Matches
// Python's Path.expanduser().
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// ============================================================================
// Cache probes
// ============================================================================

// MetadataPath returns the path to .metadata.json inside the cache.
func (m *Manager) MetadataPath() string {
	return filepath.Join(m.CacheDir, ".metadata.json")
}

// IndexHTMLPath returns the path to the SPA's entry HTML file.
func (m *Manager) IndexHTMLPath() string {
	return filepath.Join(m.CacheDir, "index.html")
}

// IsAvailable reports whether the cache contains a usable SPA —
// index.html and an assets/ directory must both be present.
func (m *Manager) IsAvailable() bool {
	if fi, err := os.Stat(m.IndexHTMLPath()); err != nil || fi.IsDir() {
		return false
	}
	if fi, err := os.Stat(filepath.Join(m.CacheDir, "assets")); err != nil || !fi.IsDir() {
		return false
	}
	return true
}

// ReadMetadata parses the cache sidecar. Returns (nil, nil) if the
// file doesn't exist. A parse failure is a real error so operators
// can debug a corrupt cache.
func (m *Manager) ReadMetadata() (*CacheMetadata, error) {
	data, err := os.ReadFile(m.MetadataPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	var md CacheMetadata
	if err := json.Unmarshal(data, &md); err != nil {
		return nil, fmt.Errorf("parse metadata: %w", err)
	}
	return &md, nil
}

// WriteMetadata writes .metadata.json to the cache directory, creating
// the dir if needed.
func (m *Manager) WriteMetadata(md *CacheMetadata) error {
	if err := os.MkdirAll(m.CacheDir, 0o700); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	b, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	return os.WriteFile(m.MetadataPath(), b, 0o600)
}

// ============================================================================
// GitHub API
// ============================================================================

// releaseInfo captures the subset of GitHub's Release JSON we need.
type releaseInfo struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// fetchLatestRelease queries /repos/{owner}/{repo}/releases/latest.
// Returns (nil, nil) on 404 (repo has no releases), an error on
// other failures.
func (m *Manager) fetchLatestRelease(ctx context.Context) (*releaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", m.APIBase, m.Repo)
	return m.fetchRelease(ctx, url)
}

// fetchRelease is the shared HTTP handler for both tag/latest lookups.
func (m *Manager) fetchRelease(ctx context.Context, url string) (*releaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no such release
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github returned %d", resp.StatusCode)
	}

	var rel releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

// DistURL picks the dist.tar.gz asset from a Release. Returns "" if
// the release doesn't have one (shouldn't happen with rx-viewer
// releases but defensive).
func (r *releaseInfo) DistURL() string {
	for _, a := range r.Assets {
		if a.Name == "dist.tar.gz" {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// directDownloadURL constructs the conventional release-asset URL,
// bypassing the API. Used when we know the version but don't want to
// pay a second API call to resolve the asset URL.
func (m *Manager) directDownloadURL(version string) string {
	if version == "latest" {
		return fmt.Sprintf("https://github.com/%s/releases/latest/download/dist.tar.gz", m.Repo)
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/dist.tar.gz", m.Repo, version)
}

// ============================================================================
// Download + extract
// ============================================================================

// Download fetches the given URL and extracts the tar.gz contents
// into the cache directory. On success, writes .metadata.json with
// the supplied releaseTag and returns nil.
//
// Download is idempotent — re-running overwrites existing content.
// Any existing files NOT in the new tarball are left in place; the
// Python version (conservatively) clears everything first. rx-go
// follows the same policy to avoid stale assets.
func (m *Manager) Download(ctx context.Context, downloadURL, releaseTag string) error {
	if downloadURL == "" {
		return errors.New("frontend download: empty URL")
	}
	m.Logger.Info("frontend_downloading", "url", downloadURL)
	if err := os.MkdirAll(m.CacheDir, 0o700); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	// Follow redirects — GitHub asset URLs redirect to CDN.
	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	// Stream the tarball to a temp file first so a partial download
	// never overwrites a working cache. Clean up on any failure path.
	tmpPath := filepath.Join(m.CacheDir, "dist.tar.gz.tmp")
	if err := streamToFile(resp.Body, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	// Clear existing content BEFORE extracting so removed files don't
	// linger (same as Python).
	if err := m.clearExistingContent(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	// Extract with traversal protection.
	if err := extractTarGz(tmpPath, m.CacheDir); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	_ = os.Remove(tmpPath)

	// Prefer the version inside dist/version.json when present;
	// otherwise fall back to the release tag.
	version := Version{Version: strings.TrimPrefix(releaseTag, "v"), BuildDate: time.Now().UTC().Format(time.RFC3339)}
	if v, err := readDistVersion(m.CacheDir); err == nil && v != nil {
		version = *v
	}

	now := time.Now().UTC().Format(time.RFC3339)
	md := &CacheMetadata{
		Version:      version,
		DownloadedAt: now,
		LastCheck:    now,
		ReleaseURL:   downloadURL,
	}
	if err := m.WriteMetadata(md); err != nil {
		return err
	}
	m.Logger.Info("frontend_installed", "version", version.Version)
	return nil
}

// streamToFile copies r to path. Used for the temp tarball download.
func streamToFile(r io.Reader, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open temp: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// clearExistingContent removes every file in CacheDir EXCEPT the
// metadata file and the in-flight tarball. Preserves the metadata so
// we can recover if extraction fails mid-flight (the next startup
// sees an "old" cache + stale metadata, fine).
func (m *Manager) clearExistingContent() error {
	entries, err := os.ReadDir(m.CacheDir)
	if err != nil {
		return fmt.Errorf("read cache dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == ".metadata.json" || name == "dist.tar.gz.tmp" {
			continue
		}
		path := filepath.Join(m.CacheDir, name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// readDistVersion parses dist/version.json if present.
func readDistVersion(cacheDir string) (*Version, error) {
	data, err := os.ReadFile(filepath.Join(cacheDir, "version.json"))
	if err != nil {
		return nil, err
	}
	var v Version
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ============================================================================
// High-level Ensure
// ============================================================================

// Ensure runs the "make sure a usable frontend is on disk" algorithm
// specified in the file doc comment. Returns nil if the cache is in
// a usable state at return time, or an error explaining why not.
func (m *Manager) Ensure(ctx context.Context) error {
	// Case 1: RX_FRONTEND_URL set — force download.
	if m.envURL != "" {
		return m.Download(ctx, m.envURL, "custom")
	}

	// Case 2: RX_FRONTEND_VERSION set.
	if m.envVersion != "" {
		cached, _ := m.ReadMetadata()
		requested := strings.TrimPrefix(m.envVersion, "v")
		if cached != nil && strings.TrimPrefix(cached.Version.Version, "v") == requested && m.IsAvailable() {
			return nil
		}
		// Download the specific version.
		url := m.directDownloadURL(m.envVersion)
		return m.Download(ctx, url, m.envVersion)
	}

	// Case 3: No overrides + valid cache → use it, no network hit.
	if m.IsAvailable() {
		return nil
	}

	// Case 4: No overrides, no cache → fetch latest release.
	rel, err := m.fetchLatestRelease(ctx)
	if err != nil {
		return fmt.Errorf("fetch latest release: %w", err)
	}
	if rel == nil {
		return errors.New("frontend: no latest release found on github")
	}
	durl := rel.DistURL()
	if durl == "" {
		return errors.New("frontend: latest release has no dist.tar.gz asset")
	}
	return m.Download(ctx, durl, rel.TagName)
}

// ============================================================================
// Static-file path validation
// ============================================================================

// ValidateStaticPath takes a request path relative to the cache dir
// (e.g. "assets/app.js") and returns the absolute path on disk, or
// nil if the resolved path would escape the cache directory, doesn't
// exist, or is a directory.
//
// Defense-in-depth: uses filepath.Clean to normalise ".." segments,
// then checks that the result starts with the cache root prefix.
func (m *Manager) ValidateStaticPath(requested string) string {
	if requested == "" {
		return ""
	}
	// Eval the absolute cache root so symlinks don't defeat prefix
	// check. If it can't be resolved we bail out — safer to 404.
	rootAbs, err := filepath.Abs(m.CacheDir)
	if err != nil {
		return ""
	}
	clean := filepath.Clean(requested)
	if strings.HasPrefix(clean, string(filepath.Separator)) {
		return "" // absolute paths not allowed in a request
	}
	full := filepath.Join(rootAbs, clean)
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return ""
	}
	if !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) && fullAbs != rootAbs {
		return ""
	}
	fi, err := os.Stat(fullAbs)
	if err != nil || fi.IsDir() {
		return ""
	}
	return fullAbs
}
