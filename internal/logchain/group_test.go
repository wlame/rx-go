package logchain

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// One generation in several encodings is one part. Alone it is no
// chain; with a second generation the plain file is the part and the
// other encodings are its duplicates.
func TestGroup_OneGenerationInSeveralEncodingsIsOnePart(t *testing.T) {
	dir := t.TempDir()
	text := textLines("x.log-2025121008")
	writeFiles(t, dir, map[string][]byte{
		"x.log-2025121008":     text,
		"x.log-2025121008.gz":  compressedcopy.Encode(t, compressedcopy.Gzip, text),
		"x.log-2025121008.zst": compressedcopy.Encode(t, compressedcopy.Zstd, text),
	})
	if got := Group(dir, listedEntries(t, dir), ClassifyPinned); len(got) != 0 {
		t.Fatalf("one generation formed chains %v", chainsByName(got))
	}

	writeFiles(t, dir, map[string][]byte{"x.log-2025121009": textLines("x.log-2025121009")})
	got := Group(dir, listedEntries(t, dir), ClassifyPinned)
	if len(got) != 1 || !slices.Equal(partNames(got[0]), []string{"x.log-2025121008", "x.log-2025121009"}) {
		t.Fatalf("chains %v", chainsByName(got))
	}
	first := got[0].Parts[0]
	if !slices.Equal(first.Duplicates, []string{"x.log-2025121008.zst", "x.log-2025121008.gz"}) {
		t.Fatalf("duplicates %v, want the zstd copy then the gzip copy", first.Duplicates)
	}
	if first.Format != compression.FormatNone || got[0].Parts[1].Duplicates == nil || len(got[0].Parts[1].Duplicates) != 0 {
		t.Fatalf("parts %+v", got[0].Parts)
	}
}

// The encoding read is chosen by what the bytes are, not by the name:
// plain, seekable zstd, zstd, gzip, bzip2, xz.
func TestGroup_DuplicatePreferenceFollowsTheDetectedKind(t *testing.T) {
	text := textLines("y.log.1")
	cases := []struct {
		label string
		files map[string][]byte
		chose string
		dups  []string
	}{
		{"a gzip file without the suffix loses to a plain file with it", map[string][]byte{
			"y.log.1":    compressedcopy.Encode(t, compressedcopy.Gzip, text),
			"y.log.1.gz": text,
		}, "y.log.1.gz", []string{"y.log.1"}},
		{"seekable zstd before zstd before gzip", map[string][]byte{
			"y.log.1.gz":  compressedcopy.Encode(t, compressedcopy.Gzip, text),
			"y.log.1.zst": compressedcopy.Encode(t, compressedcopy.SeekableZstd, text),
			"y.log.1.xz":  compressedcopy.Encode(t, compressedcopy.Xz, text),
		}, "y.log.1.zst", []string{"y.log.1.gz", "y.log.1.xz"}},
		{"gzip before xz", map[string][]byte{
			"y.log.1.xz": compressedcopy.Encode(t, compressedcopy.Xz, text),
			"y.log.1.gz": compressedcopy.Encode(t, compressedcopy.Gzip, text),
		}, "y.log.1.gz", []string{"y.log.1.xz"}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, tc.files)
			writeFiles(t, dir, map[string][]byte{"y.log": textLines("y.log")})
			got := Group(dir, listedEntries(t, dir), ClassifyPinned)
			if len(got) != 1 || len(got[0].Parts) != 2 {
				t.Fatalf("chains %v", chainsByName(got))
			}
			part := got[0].Parts[0]
			if part.Name != tc.chose || !slices.Equal(part.Duplicates, tc.dups) {
				t.Fatalf("chose %s with duplicates %v, want %s %v", part.Name, part.Duplicates, tc.chose, tc.dups)
			}
		})
	}
}

// Missing numbers: from 0 when a .0 part exists, else from 1, up to the
// highest number present, named only when no more of them are missing
// than parts with a number are present. Dated parts, years among them,
// are never missing.
func TestGroup_MissingNumbers(t *testing.T) {
	cases := []struct {
		label   string
		files   []string
		missing []string
	}{
		{"a hole", []string{"syslog.1", "syslog.2", "syslog.4"}, []string{"syslog.3"}},
		{"from 0", []string{"dmesg.0", "dmesg.2.gz"}, []string{"dmesg.1"}},
		{"the first one", []string{"x.2", "x.3"}, []string{"x.1"}},
		{"none", []string{"syslog", "syslog.1", "syslog.2.gz"}, []string{}},
		{"dated parts", []string{"app.log-20261001", "app.log-20261005", "app.log"}, []string{}},
		{"before the extension", []string{"app.log", "app.1.log", "app.3.log.gz"}, []string{"app.2.log"}},
		{"named without a compression suffix", []string{"dpkg.log.1", "dpkg.log.4.gz"}, []string{"dpkg.log.2", "dpkg.log.3"}},
		{"in numeric order", []string{"z.1", "z.2", "z.3", "z.4", "z.5", "z.6", "z.12"},
			[]string{"z.7", "z.8", "z.9", "z.10", "z.11"}},
		{"as many missing as present", []string{"x.1", "x.4"}, []string{"x.2", "x.3"}},
		{"more missing than present", []string{"x.1", "x.5"}, []string{}},
		{"far more missing than present", []string{"z.1", "z.12"}, []string{}},
		{"years", []string{"report.2023", "report.2024", "report.2026"}, []string{}},
		{"years beside numbers", []string{"report.1", "report.3", "report.2024"}, []string{"report.2"}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := Group(testDir, fakeEntries(tc.files...), allText)
			if len(got) != 1 {
				t.Fatalf("chains %v", chainsByName(got))
			}
			if got[0].Missing == nil || !slices.Equal(got[0].Missing, tc.missing) {
				t.Fatalf("missing %#v, want %v", got[0].Missing, tc.missing)
			}
			if got[0].MissingCount != len(tc.missing) {
				t.Fatalf("missing count %d, want %d", got[0].MissingCount, len(tc.missing))
			}
		})
	}
}

// A chain names at most MaxMissingNames missing parts, the lowest
// numbers first; MissingCount says how many are missing in all.
func TestGroup_MissingNamesStopAtTheirLimit(t *testing.T) {
	// 300 parts present (1–150 and 301–450) around a hole of 150.
	var names []string
	for n := 1; n <= 450; n++ {
		if n <= 150 || n > 300 {
			names = append(names, fmt.Sprintf("x.log.%d.gz", n))
		}
	}
	got := Group(testDir, fakeEntries(names...), allText)
	if len(got) != 1 || len(got[0].Parts) != 300 {
		t.Fatalf("chains %v", chainsByName(got))
	}
	want := make([]string, 0, MaxMissingNames)
	for n := 151; n < 151+MaxMissingNames; n++ {
		want = append(want, fmt.Sprintf("x.log.%d", n))
	}
	if !slices.Equal(got[0].Missing, want) || got[0].MissingCount != 150 {
		t.Fatalf("missing %v (%d names), count %d; want %d names from x.log.151, count 150",
			got[0].Missing, len(got[0].Missing), got[0].MissingCount, MaxMissingNames)
	}
}

// A file whose text check fails is no part; a chain of such files is
// no chain. An empty file is text and is never opened for the check.
func TestGroup_BinaryFilesAreExcludedAndEmptyFilesAreText(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"wtmp":              binaryBytes,
		"wtmp.1":            binaryBytes,
		"kern.log":          textLines("kern.log"),
		"kern.log.1":        binaryBytes,
		"kern.log.2.gz":     compressedcopy.Encode(t, compressedcopy.Gzip, textLines("kern.log.2")),
		"kern.log.3.gz":     compressedcopy.Encode(t, compressedcopy.Gzip, binaryBytes),
		"edge-agent.log": nil, "edge-agent.log.1": nil, "edge-agent.log.2": nil,
	})
	var classified []string
	classify := func(e Entry) (filekind.Kind, error) {
		classified = append(classified, e.Name)
		return ClassifyPinned(e)
	}
	got := chainsByName(Group(dir, listedEntries(t, dir), classify))
	want := map[string][]string{
		"kern.log":          {"kern.log.2.gz", "kern.log"},
		"edge-agent.log": {"edge-agent.log.2", "edge-agent.log.1", "edge-agent.log"},
	}
	if len(got) != len(want) || !slices.Equal(got["kern.log"], want["kern.log"]) ||
		!slices.Equal(got["edge-agent.log"], want["edge-agent.log"]) {
		t.Fatalf("chains %v, want %v", got, want)
	}
	for _, name := range classified {
		if strings.HasPrefix(name, "edge-agent") {
			t.Fatalf("an empty file was opened for the text check: %v", classified)
		}
	}
}

// The text check runs only for names that matched a template and the
// active files of their groups, never for an unrelated file, and a name
// that matched alone is not opened either.
func TestGroup_ClassifiesOnlyNamesThatCanFormAChain(t *testing.T) {
	var classified []string
	classify := func(e Entry) (filekind.Kind, error) {
		classified = append(classified, e.Name)
		return filekind.Kind{}, nil
	}
	Group(testDir, fakeEntries("lastlog", "README", "notes.txt", "report.2024", "syslog", "syslog.1", "node-1.log"), classify)
	slices.Sort(classified)
	if !slices.Equal(classified, []string{"syslog", "syslog.1"}) {
		t.Fatalf("classified %v", classified)
	}
}

// A file the classifier cannot read is left out, like a binary file.
func TestGroup_UnreadableFilesAreLeftOut(t *testing.T) {
	classify := func(e Entry) (filekind.Kind, error) {
		if e.Name == "syslog.2" {
			return filekind.Kind{}, os.ErrPermission
		}
		return filekind.Kind{}, nil
	}
	got := chainsByName(Group(testDir, fakeEntries("syslog", "syslog.1", "syslog.2"), classify))
	if !slices.Equal(got["syslog"], []string{"syslog.1", "syslog"}) {
		t.Fatalf("chains %v", got)
	}
}

// A numbered-ext part needs a text active file: a binary one does not
// count, and the numbered-ext parts are no chain then.
func TestGroup_NumbersBeforeTheExtensionNeedATextActiveFile(t *testing.T) {
	classify := func(e Entry) (filekind.Kind, error) {
		if e.Name == "node.log" {
			return filekind.Kind{NotText: filekind.NotTextPrefix + ": test"}, nil
		}
		return filekind.Kind{}, nil
	}
	if got := Group(testDir, fakeEntries("node.log", "node-1.log", "node-2.log"), classify); len(got) != 0 {
		t.Fatalf("chains %v", chainsByName(got))
	}
}

// The names and kinds of a real /var/log (the playground's log-fb):
// exactly the rotated logs are chains, and every other file is in none.
func TestGroup_RealLogDirectoryLayout(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{}
	text := func(name string) { files[name] = textLines(name) }
	gz := func(name string) { files[name] = compressedcopy.Encode(t, compressedcopy.Gzip, textLines(name)) }
	empty := func(name string) { files[name] = nil }
	binary := func(name string) { files[name] = binaryBytes }

	empty("alternatives.log")
	text("alternatives.log.1")
	for n := 2; n <= 6; n++ {
		gz(fmt.Sprintf("alternatives.log.%d.gz", n))
	}
	text("auth.log")
	for _, d := range []string{"20260930-1790726400", "20261001-1790812801", "20261002-1790899201",
		"20261003-1790985601", "20261004-1791072001", "20261005-1791158401", "20261006-1791244800"} {
		gz("auth.log-" + d + ".gz")
		gz("syslog-" + d + ".gz")
	}
	text("syslog")
	binary("btmp")
	binary("btmp.1")
	text("dmesg")
	text("dmesg.0")
	gz("dmesg.1.gz")
	gz("dmesg.2.gz")
	text("dnslogs")
	empty("dpkg.log")
	text("dpkg.log.1")
	for n := 2; n <= 11; n++ {
		gz(fmt.Sprintf("dpkg.log.%d.gz", n))
	}
	text("edge-bpf.log")
	empty("edge-agent.log")
	for n := 1; n <= 7; n++ {
		empty(fmt.Sprintf("edge-agent.log.%d", n))
	}
	text("edgectl.log")
	empty("edged.log")
	text("fontconfig.log")
	empty("kern.log")
	gz("kern.log-20260929-1790640000.gz")
	gz("kern.log-20261003-1790985601.gz")
	gz("kern.log-20261004-1791072001.gz")
	binary("lastlog")
	binary("wtmp")
	binary("wtmp.1")
	writeFiles(t, dir, files)
	for _, sub := range []string{"apt", "atop", "probe-agent", "ntpstats", "openvswitch", "private", "prometheus", "redis", "subd"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	got := Group(dir, listedEntries(t, dir), ClassifyPinned)
	wantCounts := map[string]int{
		"alternatives.log": 7, "auth.log": 8, "dmesg": 4, "dpkg.log": 12,
		"edge-agent.log": 8, "kern.log": 4, "syslog": 8,
	}
	inChain := map[string]bool{}
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
		if len(c.Parts) != wantCounts[c.Name] {
			t.Fatalf("chain %s has %d parts %v, want %d", c.Name, len(c.Parts), partNames(c), wantCounts[c.Name])
		}
		if !c.HasActive() || len(c.Missing) != 0 || c.TooManyParts {
			t.Fatalf("chain %s: active %v missing %v too many %v", c.Name, c.HasActive(), c.Missing, c.TooManyParts)
		}
		for _, p := range c.Parts {
			inChain[p.Name] = true
		}
	}
	if !slices.Equal(names, []string{"alternatives.log", "auth.log", "dmesg", "dpkg.log", "edge-agent.log", "kern.log", "syslog"}) {
		t.Fatalf("chains %v", names)
	}
	parts := 0
	for name := range files {
		inWant := false
		for chain := range wantCounts {
			if name == chain || strings.HasPrefix(name, chain+".") || strings.HasPrefix(name, chain+"-") {
				inWant = true
			}
		}
		if inChain[name] != inWant {
			t.Fatalf("%s in a chain: %v, want %v", name, inChain[name], inWant)
		}
		if inWant {
			parts++
		}
	}
	if parts != 51 {
		t.Fatalf("%d files in chains, want 51", parts)
	}
}

// A chain of more than MaxParts parts is still returned, marked, and
// its files are classified only until that is known: MaxParts+1 parts
// are found, the files after them are neither classified nor kept, and
// no missing part is named. NamedParts counts the parts the names give.
// One of two parts whose numbers span far more is not marked: it names
// no missing part, since more are missing than present.
func TestGroup_TooManyPartsIsMarked(t *testing.T) {
	for _, active := range []bool{true, false} {
		var names []string
		if active {
			names = append(names, "big.log")
		}
		for n := 1; n <= 20000; n++ {
			names = append(names, fmt.Sprintf("big.log.%d", n))
		}
		calls := 0
		classify := func(Entry) (filekind.Kind, error) {
			calls++
			return filekind.Kind{}, nil
		}
		got := Group(testDir, fakeEntries(names...), classify)
		if len(got) != 1 || !got[0].TooManyParts || got[0].HasActive() != active {
			t.Fatalf("active %v: %d chains, marked %v", active, len(got), len(got) == 1 && got[0].TooManyParts)
		}
		c := got[0]
		if calls != MaxParts+1 || len(c.Parts) != MaxParts+1 || c.NamedParts != len(names) {
			t.Fatalf("active %v: classified %d entries, kept %d parts, named %d; want %d, %d and %d",
				active, calls, len(c.Parts), c.NamedParts, MaxParts+1, MaxParts+1, len(names))
		}
		if c.Missing == nil || len(c.Missing) != 0 || c.MissingCount != 0 {
			t.Fatalf("active %v: missing %d names, count %d", active, len(c.Missing), c.MissingCount)
		}
	}

	exactly := Group(testDir, fakeEntries(fakeNames("big.log", MaxParts-1)...), allText)
	if len(exactly) != 1 || exactly[0].TooManyParts || len(exactly[0].Parts) != MaxParts {
		t.Fatalf("a chain of exactly MaxParts parts is marked")
	}

	sparse := Group(testDir, fakeEntries("x.1", "x.99999"), allText)
	if len(sparse) != 1 || sparse[0].TooManyParts || len(sparse[0].Missing) != 0 || sparse[0].MissingCount != 0 {
		t.Fatalf("sparse chain: %+v", sparse)
	}
	if sparse[0].Missing == nil {
		t.Fatalf("missing must be an empty list, not nil")
	}
}

// A chain is known to be too large once MaxParts+1 of its files are
// parts; files that are not text do not count, so a chain whose names
// pass MaxParts while its text parts do not is an ordinary chain.
func TestGroup_FilesThatAreNotTextDoNotCountTowardTooManyParts(t *testing.T) {
	names := fakeNames("big.log", MaxParts+5)
	classify := func(e Entry) (filekind.Kind, error) {
		// Every number that ends in 7 or 8: about 2,000 of them.
		if last := e.Name[len(e.Name)-1]; last == '7' || last == '8' {
			return filekind.Kind{NotText: filekind.NotTextPrefix + ": test"}, nil
		}
		return filekind.Kind{}, nil
	}
	got := Group(testDir, fakeEntries(names...), classify)
	if len(got) != 1 || got[0].TooManyParts || len(got[0].Parts) > MaxParts {
		t.Fatalf("marked %v with %d parts", len(got) == 1 && got[0].TooManyParts, len(got[0].Parts))
	}
}

// fakeNames is the active file name and its parts name.1 … name.n.
func fakeNames(name string, n int) []string {
	names := []string{name}
	for i := 1; i <= n; i++ {
		names = append(names, fmt.Sprintf("%s.%d", name, i))
	}
	return names
}

// BenchmarkGroup_20000Parts measures grouping a directory listing of
// 20,000 rotated names (no file is read).
func BenchmarkGroup_20000Parts(b *testing.B) {
	names := []string{"big.log"}
	for n := 1; n <= 20000; n++ {
		names = append(names, fmt.Sprintf("big.log.%d.gz", n))
	}
	entries := fakeEntries(names...)
	b.ReportAllocs()
	for b.Loop() {
		Group(testDir, entries, allText)
	}
}
