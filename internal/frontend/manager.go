// Package frontend manages downloading and serving the rx-viewer SPA from GitHub releases.
//
// The manager downloads dist.tar.gz from wlame/rx-viewer releases, caches it locally,
// and provides path validation for secure static file serving.
package frontend

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	json "github.com/goccy/go-json"

	"github.com/wlame/rx/internal/config"
)

const (
	githubRepo       = "wlame/rx-viewer"
	githubAPIBase    = "https://api.github.com"
	requestTimeout   = 30 * time.Second
	maxDownloadBytes = 100 * 1024 * 1024 // 100 MB safety limit
)

// Version holds parsed version information from the frontend's version.json file.
type Version struct {
	Version   string `json:"version"`
	BuildDate string `json:"buildDate"`
	Commit    string `json:"commit"`
}

// CacheMetadata tracks when and where the cached frontend was downloaded from.
type CacheMetadata struct {
	Version      Version `json:"version"`
	DownloadedAt string  `json:"downloaded_at"`
	LastCheck    string  `json:"last_check"`
	ReleaseURL   string  `json:"release_url"`
}

// Manager handles downloading, caching, and validating the frontend SPA.
type Manager struct {
	cacheDir   string // directory where the frontend is cached
	envVersion string // RX_FRONTEND_VERSION override
	envURL     string // RX_FRONTEND_URL override
}

// NewManager creates a frontend manager from the given config.
// The cache directory is resolved from: cfg.FrontendPath > cfg.CacheDir/frontend > ~/.cache/rx/frontend.
func NewManager(cfg *config.Config) *Manager {
	cacheDir := filepath.Join(cfg.CacheDir, "frontend")
	if cfg.FrontendPath != "" {
		cacheDir = cfg.FrontendPath
	}

	return &Manager{
		cacheDir:   cacheDir,
		envVersion: cfg.FrontendVersion,
		envURL:     cfg.FrontendURL,
	}
}

// EnsureFrontend checks if the frontend is available and downloads it if needed.
// Returns the path to the frontend dist directory, or an empty string if the frontend
// could not be obtained (download failure should not block server startup).
func (m *Manager) EnsureFrontend() (string, error) {
	// Case 1: RX_FRONTEND_URL is set -- force download from that URL.
	if m.envURL != "" {
		slog.Info("frontend: URL override", "url", m.envURL)
		if err := m.download(m.envURL, "custom"); err != nil {
			slog.Warn("frontend: download from URL override failed", "error", err)
			// Fallback to cached if available.
			if m.IsAvailable() {
				return m.cacheDir, nil
			}
			return "", nil
		}
		return m.cacheDir, nil
	}

	// Case 2: RX_FRONTEND_VERSION is set.
	if m.envVersion != "" {
		// Check if the requested version is already cached.
		cached := m.readCacheMetadata()
		if cached != nil {
			reqVer := strings.TrimPrefix(m.envVersion, "v")
			cachedVer := strings.TrimPrefix(cached.Version.Version, "v")
			if reqVer == cachedVer && m.IsAvailable() {
				slog.Info("frontend: requested version already cached", "version", reqVer)
				return m.cacheDir, nil
			}
		}

		url := m.directDownloadURL(m.envVersion)
		if err := m.download(url, m.envVersion); err != nil {
			slog.Warn("frontend: download of requested version failed", "version", m.envVersion, "error", err)
			if m.IsAvailable() {
				slog.Warn("frontend: falling back to cached version")
				return m.cacheDir, nil
			}
			return "", nil
		}
		return m.cacheDir, nil
	}

	// Case 3: No env vars -- use cached if available (no GitHub requests).
	if m.IsAvailable() {
		slog.Debug("frontend: using cached version")
		return m.cacheDir, nil
	}

	// Case 4: No cache -- download latest.
	slog.Info("frontend: no cached version found, downloading latest")
	url := m.directDownloadURL("latest")
	if err := m.download(url, "latest"); err != nil {
		slog.Warn("frontend: download of latest failed", "error", err)
		return "", nil
	}
	return m.cacheDir, nil
}

// IsAvailable returns true if the frontend is cached and has the essential files
// (index.html and assets/ directory).
func (m *Manager) IsAvailable() bool {
	indexPath := filepath.Join(m.cacheDir, "index.html")
	assetsPath := filepath.Join(m.cacheDir, "assets")

	indexInfo, err := os.Stat(indexPath)
	if err != nil || indexInfo.IsDir() {
		return false
	}

	assetsInfo, err := os.Stat(assetsPath)
	if err != nil || !assetsInfo.IsDir() {
		return false
	}

	return true
}

// Dir returns the path to the frontend cache directory.
func (m *Manager) Dir() string {
	return m.cacheDir
}

// ValidateStaticPath validates that a requested file path is safe (no directory traversal)
// and exists within the cache directory. Returns the resolved absolute path, or an error.
func (m *Manager) ValidateStaticPath(requestedPath string) (string, error) {
	// Clean the path to remove any ../ sequences before joining.
	cleaned := filepath.Clean("/" + requestedPath)
	// filepath.Clean("/" + path) normalizes to an absolute-style path; strip leading "/".
	cleaned = strings.TrimPrefix(cleaned, "/")

	fullPath := filepath.Join(m.cacheDir, cleaned)

	// Resolve and verify containment.
	resolvedBase, err := filepath.Abs(m.cacheDir)
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}

	resolvedFull, err := filepath.Abs(fullPath)
	if err != nil {
		return "", fmt.Errorf("resolve requested path: %w", err)
	}

	// Ensure the resolved path is within the cache directory.
	if !isWithin(resolvedFull, resolvedBase) {
		return "", fmt.Errorf("path traversal attempt blocked: %s", requestedPath)
	}

	// Check that the file exists.
	info, err := os.Stat(resolvedFull)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", requestedPath)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory: %s", requestedPath)
	}

	return resolvedFull, nil
}

// download fetches dist.tar.gz from the given URL and extracts it into the cache directory.
func (m *Manager) download(downloadURL, releaseTag string) error {
	slog.Info("frontend: downloading", "url", downloadURL)

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}

	// Create the cache directory.
	if err := os.MkdirAll(m.cacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	// Write to a temp file first, then extract.
	tmpFile := filepath.Join(m.cacheDir, "dist.tar.gz.tmp")
	defer os.Remove(tmpFile) // Clean up on any exit path.

	out, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	// Limit download size for safety.
	written, err := io.Copy(out, io.LimitReader(resp.Body, maxDownloadBytes))
	out.Close()
	if err != nil {
		return fmt.Errorf("write download: %w", err)
	}
	slog.Info("frontend: download complete", "bytes", written)

	// Clear existing frontend files (keep metadata and temp file).
	if err := m.clearExisting(tmpFile); err != nil {
		return fmt.Errorf("clear existing: %w", err)
	}

	// Extract tar.gz.
	if err := m.extractTarGz(tmpFile); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Save metadata.
	m.saveMetadata(releaseTag, downloadURL)

	slog.Info("frontend: installed successfully")
	return nil
}

// clearExisting removes existing frontend files from the cache directory,
// preserving the metadata file and the temp file.
func (m *Manager) clearExisting(tmpFile string) error {
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		return err
	}

	metadataName := ".metadata.json"
	tmpName := filepath.Base(tmpFile)

	for _, entry := range entries {
		name := entry.Name()
		if name == metadataName || name == tmpName {
			continue
		}
		path := filepath.Join(m.cacheDir, name)
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("frontend: failed to remove", "path", path, "error", err)
		}
	}
	return nil
}

// extractTarGz extracts a .tar.gz file into the cache directory.
// It validates that all extracted paths stay within the cache directory (security).
func (m *Manager) extractTarGz(tarGzPath string) error {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	resolvedBase, err := filepath.Abs(m.cacheDir)
	if err != nil {
		return fmt.Errorf("resolve cache dir: %w", err)
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		// Security: validate the extracted path stays within cache dir.
		target := filepath.Join(m.cacheDir, filepath.Clean(header.Name))
		resolvedTarget, err := filepath.Abs(target)
		if err != nil {
			return fmt.Errorf("resolve target: %w", err)
		}
		if !isWithin(resolvedTarget, resolvedBase) {
			return fmt.Errorf("unsafe archive path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(resolvedTarget, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", header.Name, err)
			}

		case tar.TypeReg:
			// Ensure parent directory exists.
			if err := os.MkdirAll(filepath.Dir(resolvedTarget), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", header.Name, err)
			}

			outFile, err := os.Create(resolvedTarget)
			if err != nil {
				return fmt.Errorf("create %s: %w", header.Name, err)
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return fmt.Errorf("write %s: %w", header.Name, err)
			}
			outFile.Close()
		}
	}

	return nil
}

// saveMetadata writes a .metadata.json file with download details.
func (m *Manager) saveMetadata(releaseTag, downloadURL string) {
	// Try to read version.json from the extracted frontend.
	var ver Version
	versionPath := filepath.Join(m.cacheDir, "version.json")
	if data, err := os.ReadFile(versionPath); err == nil {
		_ = json.Unmarshal(data, &ver)
	}
	if ver.Version == "" {
		ver.Version = strings.TrimPrefix(releaseTag, "v")
	}

	meta := CacheMetadata{
		Version:      ver,
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		LastCheck:    time.Now().UTC().Format(time.RFC3339),
		ReleaseURL:   downloadURL,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		slog.Warn("frontend: failed to marshal metadata", "error", err)
		return
	}

	metaPath := filepath.Join(m.cacheDir, ".metadata.json")
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		slog.Warn("frontend: failed to write metadata", "error", err)
	}
}

// readCacheMetadata reads the .metadata.json file if it exists.
func (m *Manager) readCacheMetadata() *CacheMetadata {
	metaPath := filepath.Join(m.cacheDir, ".metadata.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil
	}

	var meta CacheMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		slog.Warn("frontend: failed to parse metadata", "error", err)
		return nil
	}
	return &meta
}

// directDownloadURL constructs a GitHub release download URL for the given version.
func (m *Manager) directDownloadURL(version string) string {
	if version == "latest" {
		return fmt.Sprintf("https://github.com/%s/releases/latest/download/dist.tar.gz", githubRepo)
	}
	// Ensure version has "v" prefix.
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/dist.tar.gz", githubRepo, version)
}

// isWithin returns true if child is equal to or a subdirectory/file under parent.
func isWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(child, prefix)
}
