package findings

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// captureWarnings sends the default logger to a buffer for the test and
// lets the one-per-process warning of an unsafe cache entry fire again.
func captureWarnings(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	unsafeWarned.Store(false)
	t.Cleanup(func() {
		slog.SetDefault(prev)
		unsafeWarned.Store(false)
	})
	return buf
}

// lockedBuffer is a bytes.Buffer safe for the logger's writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// snapshot maps every file of dir (not recursive) to its content, a
// symbolic link to "-> target".
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			out[e.Name()] = "-> " + target
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

// outsideDir makes a directory outside the cache, holding files a
// scan must neither write nor delete.
func outsideDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"package.json":     `{"name": "victim"}`,
		"credentials.json": `{"token": "not-a-real-token"}`,
		"notes.txt":        "keep me\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A per-file cache directory that is a symbolic link, to a directory
// outside the cache or to another directory inside it, is not followed:
// the scan answers without the cache, writes and deletes nothing in the
// link's target, logs one warning, and Read and Summary see no entry.
func TestScan_PerFileDirectoryLinkIsNotFollowed(t *testing.T) {
	for _, where := range []string{"outside the cache", "inside the cache"} {
		t.Run(where, func(t *testing.T) {
			opts := testOptions(t)
			target := outsideDir(t)
			if where == "inside the cache" {
				inside := filepath.Join(opts.Dir, "other_0123456789abcdef")
				if err := os.Rename(target, inside); err != nil {
					t.Fatal(err)
				}
				target = inside
			}
			requireLinkedDirectoryUntouched(t, opts, target)
		})
	}
}

// requireLinkedDirectoryUntouched makes the per-file directory of a new
// log a link to target and checks that scans, reads and summaries leave
// target as it was.
func requireLinkedDirectoryUntouched(t *testing.T, opts ScanOptions, target string) {
	t.Helper()
	logs := captureWarnings(t)
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()
	file := classify(t, writeLog(t, t.TempDir(), "app.log", text))
	s, rec := newRecordedScheduler(1)
	before := snapshot(t, target)
	if err := os.Symlink(target, cacheDir(opts.Dir, file.Path())); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		got := mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)
		requireSameFindings(t, got, coldAnswer(t, text, cat, "err-line"))
	}
	if scans := rec.scanned(); len(scans) != 2 {
		t.Errorf("scans %v: the second call must scan again (nothing cached)", scans)
	}
	if after := snapshot(t, target); !reflect.DeepEqual(after, before) {
		t.Errorf("the link's target changed:\nbefore %v\nafter  %v", before, after)
	}
	read, err := s.read(file, cat, pick(t, cat, "err-line"), opts)
	if err != nil || len(read.Checked) != 0 {
		t.Errorf("read through the link: %+v (%v)", read, err)
	}
	if sum, err := s.summary(file.Pinned(), cat, opts); err != nil || sum != nil {
		t.Errorf("summary through the link: %+v (%v)", sum, err)
	}
	if n := strings.Count(logs.String(), "findings_cache_unsafe_entry"); n != 1 {
		t.Errorf("%d warnings, want one:\n%s", n, logs.String())
	}
}

// An entry file, meta.json or the lock file that is a symbolic link,
// to a file outside the cache or to another file of the directory, is
// never read, written or deleted through: the entry counts as missing
// and the link's target stays as it was.
func TestScan_FileLinksInTheCacheDirectoryAreNotFollowed(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()
	ids := []string{"err-line", "warn-line"}

	// link replaces name in the cache directory of a file scanned once
	// with a symbolic link and returns the files the link must leave
	// alone: a copy of the replaced file, outside or inside the
	// directory.
	cases := []struct {
		name string
		link func(t *testing.T, dir, name string) (watch string)
	}{
		{"to a file outside", func(t *testing.T, dir, name string) string {
			outside := filepath.Join(t.TempDir(), "copy.json")
			moveAndLink(t, filepath.Join(dir, name), outside)
			return outside
		}},
		{"to a file of the directory", func(t *testing.T, dir, name string) string {
			// Not "*.json": the clean-up may remove an unnamed entry
			// file of the directory, which is not a link's target.
			inside := filepath.Join(dir, "copy.bak")
			moveAndLink(t, filepath.Join(dir, name), inside)
			return inside
		}},
	}
	for _, tc := range cases {
		for _, name := range []string{"err-line.json", metaName} {
			t.Run(name+" "+tc.name, func(t *testing.T) {
				captureWarnings(t)
				file := classify(t, writeLog(t, t.TempDir(), "app.log", text))
				s, rec := newRecordedScheduler(1)
				opts := testOptions(t)
				mustScan(t, s, file, cat, pick(t, cat, ids...), opts)
				rec.scanned()
				dir := cacheDir(opts.Dir, file.Path())
				watch := tc.link(t, dir, name)
				watched, _ := os.ReadFile(watch)

				read, err := s.read(file, cat, pick(t, cat, ids...), opts)
				if err != nil || slices.Contains(read.Checked, "err-line") {
					t.Fatalf("read followed the link: checked %v (%v)", read.Checked, err)
				}
				got := mustScan(t, s, file, cat, pick(t, cat, ids...), opts)
				if scans := rec.scanned(); len(scans) != 1 || !slices.Contains(scans[0], "err-line") {
					t.Errorf("scans %v: err-line must be scanned again", scans)
				}
				requireSameFindings(t, got, coldAnswer(t, text, cat, ids...))
				if now, err := os.ReadFile(watch); err != nil || !bytes.Equal(now, watched) {
					t.Errorf("the link's target changed (%v)", err)
				}
			})
		}
	}

	t.Run("lock file", func(t *testing.T) {
		logs := captureWarnings(t)
		file := classify(t, writeLog(t, t.TempDir(), "app.log", text))
		s := newScheduler(1)
		opts := testOptions(t)
		dir := cacheDir(opts.Dir, file.Path())
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		missing := filepath.Join(t.TempDir(), "made-by-the-lock")
		if err := os.Symlink(missing, filepath.Join(dir, lockName)); err != nil {
			t.Fatal(err)
		}
		mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)
		if _, err := os.Lstat(missing); err == nil {
			t.Error("the lock link's target was created")
		}
		if names := entryFiles(t, dir); !slices.Equal(names, []string{lockName}) {
			t.Errorf("the directory holds %v, want only the lock link (nothing written)", names)
		}
		if !strings.Contains(logs.String(), "findings_cache_unsafe_entry") {
			t.Errorf("no warning:\n%s", logs.String())
		}
	})
}

// moveAndLink moves the file at path to dest and puts a symbolic link
// to dest in its place.
func moveAndLink(t *testing.T, path, dest string) {
	t.Helper()
	if err := os.Rename(path, dest); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dest, path); err != nil {
		t.Fatal(err)
	}
}

// entryName takes only a signature id the loader accepts (kebab-case,
// at most 64 bytes), whatever reaches it.
func TestEntryName_TakesOnlyKebabCaseIDs(t *testing.T) {
	for _, tc := range []struct {
		id, want string
		ok       bool
	}{
		{"err-line", "err-line.json", true},
		{"a1", "a1.json", true},
		{"meta", metaEntryName, true},
		{strings.Repeat("a", 64), strings.Repeat("a", 64) + ".json", true},
		{strings.Repeat("a", 65), "", false},
		{"", "", false},
		{".", "", false},
		{"..", "", false},
		{"a/b", "", false},
		{"../x", "", false},
		{"a\x00b", "", false},
		{"Upper", "", false},
		{"under_score", "", false},
		{".tmp-x", "", false},
	} {
		got, ok := entryName(tc.id)
		if got != tc.want || ok != tc.ok {
			t.Errorf("entryName(%q) = %q, %v; want %q, %v", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

// A signature whose id no loader gives (one that would name a path) is
// scanned and answered but never cached; the others of the scan are.
func TestScan_IDNoLoaderGivesIsAnsweredNotCached(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	s := newScheduler(1)
	opts := testOptions(t)
	bad := *pick(t, cat, "warn-line")[0]
	bad.ID = "../escape"
	got := mustScan(t, s, file, cat, append(pick(t, cat, "err-line"), &bad), opts)
	if !slices.Equal(got.Checked, []string{"err-line", "../escape"}) || got.BySignature["../escape"].Count == 0 {
		t.Fatalf("answer: checked %v", got.Checked)
	}
	dir := cacheDir(opts.Dir, file.Path())
	if names := entryFiles(t, dir); !slices.Equal(names, []string{lockName, "err-line.json", metaName}) {
		t.Errorf("files %v", names)
	}
	if names := entryFiles(t, opts.Dir); len(names) != 1 {
		t.Errorf("the cache dir holds %v", names)
	}
}

// Read opens meta.json and the entries of the selected signatures it
// holds, nothing else of the cache, and reads of the scanned file only
// the two 64 KiB windows of the identity's fingerprint; it never scans.
func TestRead_ReadsOnlyTheCacheFilesItNeeds(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	mustScan(t, s, file, cat, pick(t, cat, "err-line", "warn-line", "py-trace"), opts)
	rec.scanned()
	rec.sourceBytes.Store(0)

	var opened []string
	var cacheBytes int64
	s.openCache = func(root *os.Root, name string) (io.ReadCloser, error) {
		opened = append(opened, name)
		rc, err := openCacheFile(root, name)
		if err != nil {
			return nil, err
		}
		return countingReadCloser{ReadCloser: rc, n: &cacheBytes}, nil
	}
	read, err := s.read(file, cat, pick(t, cat, "py-trace", "blank-lines", "err-line"), opts)
	if err != nil || !slices.Equal(read.Checked, []string{"py-trace", "err-line"}) {
		t.Fatalf("read: %+v (%v)", read, err)
	}
	if want := []string{metaName, "py-trace.json", "err-line.json"}; !slices.Equal(opened, want) {
		t.Errorf("opened %v, want %v", opened, want)
	}
	dir := cacheDir(opts.Dir, file.Path())
	var want int64
	for _, name := range []string{metaName, "py-trace.json", "err-line.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		want += info.Size()
	}
	if cacheBytes != want {
		t.Errorf("read %d bytes of the cache, want %d (the three files once)", cacheBytes, want)
	}
	const fingerprintBytes = 2 * 64 << 10
	if got := rec.sourceBytes.Load(); got != fingerprintBytes {
		t.Errorf("read %d bytes of the file, want the %d of the fingerprint", got, fingerprintBytes)
	}
	if scans := rec.scanned(); len(scans) != 0 {
		t.Errorf("read scanned %v", scans)
	}
}

// countingReadCloser counts the bytes read through it.
type countingReadCloser struct {
	io.ReadCloser
	n *int64
}

func (c countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	*c.n += int64(n)
	return n, err
}
