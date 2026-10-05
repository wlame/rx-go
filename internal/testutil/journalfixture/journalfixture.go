// Package journalfixture gives tests the invented systemd journal
// fixtures of internal/journal/testdata, from any package: a journal by
// name, the parts of a rotated chain, a set's text and the manifest.
//
// The fixtures are invented journals with the text
// `journalctl --file=F --no-pager --utc -q -o short-iso-precise` prints
// for them (systemd 257, TZ=UTC, LC_ALL=C). `just journal-fixtures`
// writes them with Docker; tests only read them, so they need no Docker
// and no journalctl.
//
//   - NAME.journal.zst: one journal of a set (basic, many, stamps) in one
//     variant, NAME = SET-COMPRESSION-MODE, for example
//     basic-zstd-compact. The journals are stored zstd-compressed;
//     Journal decompresses one into a test's temporary directory.
//   - SET.txt: the text of every variant of the set. Compression and
//     compact mode do not change the text.
//   - chain-one-counter/ and chain-two-counters/: the parts of a rotated
//     journal written by systemd-journald (system.journal and its
//     system@….journal archives), with all.txt, the text journalctl
//     prints over every part.
//   - fixtures.json: one Info per journal, with its entry counts.
//
// This is test-only infrastructure. It lives under internal/testutil so
// production binaries never depend on it, and it must not be imported
// from non-test code. It does not import internal/journal, whose own
// tests use it.
package journalfixture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Sets are the sets every compression and mode is written for, Variants
// the names of those variants, and Chains the directories that hold the
// parts of a rotated journal.
var (
	Sets     = []string{"basic", "many", "stamps"}
	Variants = []string{
		"none-regular", "none-compact", "xz-regular", "xz-compact",
		"lz4-regular", "lz4-compact", "zstd-regular", "zstd-compact",
	}
	Chains = []string{"chain-one-counter", "chain-two-counters"}
)

// Info is one object of fixtures.json: a journal fixture and the counts
// journalctl gives for it.
type Info struct {
	// Name is the fixture's path under the testdata directory without
	// ".journal.zst": "basic-zstd-compact", or "chain-one-counter/system"
	// for a part of a chain.
	Name string `json:"name"`
	// Set is the set a fixture belongs to: basic, many, stamps, or the
	// chain directory's name.
	Set string `json:"set"`
	// Compression is the compression the journal's header names: none,
	// xz, lz4 or zstd.
	Compression string `json:"compression"`
	// Compact is true for a journal in compact mode.
	Compact bool `json:"compact"`
	// Entries counts the entries `journalctl -o export` gives.
	Entries int64 `json:"entries"`
	// Shown counts the entries journalctl prints.
	Shown int64 `json:"shown"`
	// WithoutMessage counts the entries that have no MESSAGE field.
	WithoutMessage int64 `json:"without_message"`
}

// IsChainPart reports whether the fixture is a part of a chain, which
// lives in a directory of its own and has no text file of its own.
func (f Info) IsChainPart() bool { return strings.Contains(f.Name, "/") }

// TextPath is the absolute path of the file holding journalctl's text
// for the fixture: its set's SET.txt, or all.txt of its chain, which
// holds the text of every part of the chain.
func (f Info) TextPath() string {
	if f.IsChainPart() {
		return filepath.Join(mustRoot(), f.Set, "all.txt")
	}
	return filepath.Join(mustRoot(), f.Set+".txt")
}

// CompressedPath is the absolute path where the fixture is stored.
func (f Info) CompressedPath() string {
	return filepath.Join(mustRoot(), filepath.FromSlash(f.Name)+".journal.zst")
}

// fixtureDirFromModule is where the fixtures live, relative to the
// module's root directory.
const fixtureDirFromModule = "internal/journal/testdata"

// findRoot returns the absolute path of the fixture directory, looked
// up once (sync.OnceValues runs the function on the first call and
// hands every later call the same two results, from any goroutine).
var findRoot = sync.OnceValues(locateRoot)

// locateRoot finds the fixture directory. runtime.Caller(0) gives the
// path of this source file as the compiler recorded it, three levels
// below the module root. A build with -trimpath records a path relative
// to the module instead; the fixture directory is then found by walking
// up from the working directory (go test runs a package's tests in its
// own directory) to the directory that holds go.mod.
func locateRoot() (string, error) {
	if _, file, _, ok := runtime.Caller(0); ok && filepath.IsAbs(file) {
		dir := filepath.Join(filepath.Dir(file), "..", "..", "..", filepath.FromSlash(fixtureDirFromModule))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find the journal fixtures: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, filepath.FromSlash(fixtureDirFromModule)), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("find the journal fixtures: no go.mod above the working directory")
		}
		dir = parent
	}
}

// Root returns the absolute path of the fixture directory,
// internal/journal/testdata. It fails tb when the directory cannot be
// found.
func Root(tb testing.TB) string {
	tb.Helper()
	dir, err := findRoot()
	if err != nil {
		tb.Fatal(err)
	}
	return dir
}

// mustRoot is Root for the methods of Info, which take no testing.TB.
// The directory is found from this file's own path, so a failure means
// the source tree is not where the test binary was built from.
func mustRoot() string {
	dir, err := findRoot()
	if err != nil {
		panic(err)
	}
	return dir
}

// Journal decompresses NAME.journal.zst of the fixture directory into a
// new temporary directory and returns the path of NAME.journal there
// (the last element of NAME only, for a chain part). The file is removed
// when the test ends.
func Journal(tb testing.TB, name string) string {
	tb.Helper()
	compressed := filepath.Join(Root(tb), filepath.FromSlash(name)+".journal.zst")
	journal := filepath.Join(tb.TempDir(), filepath.Base(filepath.FromSlash(name))+".journal")
	writeDecompressed(tb, compressed, journal)
	return journal
}

// ChainDir decompresses every *.journal.zst of the fixture directory's
// subdirectory dir into a new temporary directory, keeping their names
// without ".zst", and returns the directory. Only the journals are
// copied: the directory holds the parts of a chain and nothing else
// (all.txt is read with Info.TextPath).
func ChainDir(tb testing.TB, dir string) string {
	tb.Helper()
	compressed, err := filepath.Glob(filepath.Join(Root(tb), dir, "*.journal.zst"))
	if err != nil {
		tb.Fatalf("list the journals of %s: %v", dir, err)
	}
	if len(compressed) == 0 {
		tb.Fatalf("no journal in %s", filepath.Join(Root(tb), dir))
	}
	out := tb.TempDir()
	for _, part := range compressed {
		writeDecompressed(tb, part, filepath.Join(out, strings.TrimSuffix(filepath.Base(part), ".zst")))
	}
	return out
}

// SetText returns the bytes of SET.txt: the text journalctl prints for
// every variant of the set.
func SetText(tb testing.TB, set string) []byte {
	tb.Helper()
	text, err := os.ReadFile(Info{Set: set}.TextPath())
	if err != nil {
		tb.Fatalf("read the text of %s: %v", set, err)
	}
	return text
}

// Manifest returns the objects of fixtures.json, in the file's order.
func Manifest(tb testing.TB) []Info {
	tb.Helper()
	data, err := os.ReadFile(filepath.Join(Root(tb), "fixtures.json"))
	if err != nil {
		tb.Fatalf("read the fixture manifest: %v", err)
	}
	var fixtures []Info
	if err := json.Unmarshal(data, &fixtures); err != nil {
		tb.Fatalf("decode the fixture manifest: %v", err)
	}
	return fixtures
}

// Decompressed returns the zstd-decompressed bytes of the file at src,
// a path such as Root(tb)/render_cases.json.zst.
func Decompressed(tb testing.TB, src string) []byte {
	tb.Helper()
	compressed, err := os.ReadFile(src) //nolint:gosec // a fixture path the test names
	if err != nil {
		tb.Fatalf("read fixture: %v", err)
	}
	// zstd.NewReader(nil) makes a decoder for whole buffers only
	// (DecodeAll); Close releases its goroutines, and defer runs it when
	// this function returns, whichever way it returns.
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		tb.Fatalf("make a zstd decoder: %v", err)
	}
	defer decoder.Close()
	plain, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		tb.Fatalf("decompress %s: %v", src, err)
	}
	return plain
}

// writeDecompressed writes the zstd-decompressed bytes of src to dst.
func writeDecompressed(tb testing.TB, src, dst string) {
	tb.Helper()
	if err := os.WriteFile(dst, Decompressed(tb, src), 0o600); err != nil {
		tb.Fatalf("write %s: %v", dst, err)
	}
}
