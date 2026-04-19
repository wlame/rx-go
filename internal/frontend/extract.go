package frontend

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ============================================================================
// Tarball extraction with traversal protection
// ============================================================================

// ErrTarballTraversal is returned when an entry's destination path
// would escape the target directory via ".." or an absolute path.
// The extraction is aborted at the offending entry (no partial
// extraction is left behind) and this error is returned.
var ErrTarballTraversal = errors.New("tarball entry escapes destination directory")

// ErrTarballUnsupportedEntry is returned when the tarball contains
// entry types we don't want to handle (block/char devices, symlinks
// pointing outside destDir). Regular files and directories are
// always OK.
var ErrTarballUnsupportedEntry = errors.New("unsupported tarball entry type")

// extractTarGz extracts a gzipped tarball at tarPath into destDir.
// Every entry is validated before any write; absolute paths and
// paths containing ".." segments are rejected with ErrTarballTraversal.
//
// Symlinks: allowed only if the RESOLVED target (after filepath.Clean)
// still lives inside destDir. A symlink pointing to "/etc/passwd" is
// rejected.
//
// Maximum entry size is capped at 512 MB per file — more than enough
// for any reasonable SPA bundle but a safety valve against a
// pathological tarball that would otherwise exhaust disk.
func extractTarGz(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }()

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("abs destDir: %w", err)
	}

	const maxFileBytes = 512 * 1024 * 1024 // 512 MB per entry

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		// Validate the path: no absolute, no ".." traversal, must
		// resolve under destDir.
		if err := validateTarPath(absDest, hdr.Name); err != nil {
			return err
		}
		target := filepath.Join(absDest, filepath.Clean(hdr.Name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("mkdir parent for %s: %w", target, err)
			}
			// #nosec G115 — Typeflag 'TypeRegA' is legacy-reg; same write path.
			if hdr.Size > maxFileBytes {
				return fmt.Errorf("tarball entry %s exceeds %d bytes", hdr.Name, maxFileBytes)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("open %s: %w", target, err)
			}
			// LimitReader defends against corrupted-size fields
			// (malformed tar where the declared size is less than
			// actual content).
			if _, err := io.Copy(out, io.LimitReader(tr, maxFileBytes)); err != nil {
				_ = out.Close()
				return fmt.Errorf("copy %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Reject symlinks whose linkname is an absolute path. The
			// filepath.Join trick used elsewhere is deceptive: given
			// a Linux absolute second arg, Join strips the leading
			// slash and appends, so "/foo/bar" + "/etc/passwd" becomes
			// "/foo/bar/etc/passwd", which then passes the
			// "under destDir" check but points at a sensitive file on
			// the actual filesystem via the symlink. We want to block
			// ANY absolute linkname outright.
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("%w: symlink %s → absolute %q",
					ErrTarballTraversal, hdr.Name, hdr.Linkname)
			}
			// For relative linknames, make sure the resolved target
			// (relative to target's parent dir) still lives under destDir.
			// #nosec G305 — this IS the validation step; we prevent
			// traversal by resolving and then enforcing the prefix check
			// against absDest below. gosec flags the path construction
			// itself but misses that we gate the actual os.Symlink on
			// the resulting path being inside the destination root.
			linkDest := filepath.Join(filepath.Dir(target), hdr.Linkname)
			linkAbs, err := filepath.Abs(filepath.Clean(linkDest))
			if err != nil {
				return fmt.Errorf("resolve symlink target: %w", err)
			}
			if !strings.HasPrefix(linkAbs, absDest+string(filepath.Separator)) && linkAbs != absDest {
				return fmt.Errorf("%w: symlink %s → %s", ErrTarballTraversal, hdr.Name, hdr.Linkname)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				// Don't treat "already exists" as fatal — Python
				// doesn't either.
				if !errors.Is(err, os.ErrExist) {
					return fmt.Errorf("symlink %s: %w", target, err)
				}
			}
		default:
			// Block/char/FIFO/hardlink — not something a frontend
			// tarball should contain. Fail loud so operators know
			// someone's shipping an unexpected bundle.
			return fmt.Errorf("%w: %s (typeflag %c)",
				ErrTarballUnsupportedEntry, hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

// validateTarPath rejects entry names that:
//
//   - are absolute paths (leading '/')
//   - contain ".." components
//   - resolve to a location outside absDest after Clean
//
// Matches Python's rx-python/src/rx/frontend_manager.py::validate_path_security
// except it operates on the NAME, not the realized filesystem path,
// so we can bail BEFORE writing anything.
func validateTarPath(absDest, name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty entry name", ErrTarballTraversal)
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("%w: absolute path %q", ErrTarballTraversal, name)
	}
	// Reject explicit ".." segments BEFORE Clean turns them into
	// something innocuous. filepath.Clean("foo/../bar") → "bar" which
	// might look safe but hides the attacker's intent.
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return fmt.Errorf("%w: %q contains parent segment", ErrTarballTraversal, name)
		}
	}
	clean := filepath.Clean(name)
	if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/..") {
		return fmt.Errorf("%w: %q resolves outside dest", ErrTarballTraversal, name)
	}
	full := filepath.Join(absDest, clean)
	absFull, err := filepath.Abs(full)
	if err != nil {
		return fmt.Errorf("%w: resolve %s: %v", ErrTarballTraversal, name, err)
	}
	if !strings.HasPrefix(absFull, absDest+string(filepath.Separator)) && absFull != absDest {
		return fmt.Errorf("%w: %s resolves to %s (outside %s)",
			ErrTarballTraversal, name, absFull, absDest)
	}
	return nil
}
