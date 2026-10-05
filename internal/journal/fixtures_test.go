package journal

// The journal fixtures in testdata/ are invented journals with the text
// `journalctl --file=F --no-pager --utc -q -o short-iso-precise` prints
// for them (systemd 257, TZ=UTC, LC_ALL=C). The helpers that read them
// live in internal/testutil/journalfixture, which other packages use too;
// this file names them for this package's tests and checks that the
// fixtures are what the helpers say. It also holds render_cases.json.zst:
// hand-written entries with journalctl's output for each (renderCase),
// read through renderCases.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// fixtureRoot is the directory of the journal fixtures, relative to the
// package directory (go test runs each package's tests in its own
// directory).
const fixtureRoot = "testdata"

// fixtureSets are the sets every compression and mode is written for,
// and fixtureVariants the names of those variants.
var (
	fixtureSets     = journalfixture.Sets
	fixtureVariants = journalfixture.Variants
	fixtureChains   = journalfixture.Chains
)

// defaultFixtureSizeLimit bounds the stored (zstd-compressed) size of a
// journal fixture, and fixtureSizeLimits holds the sets that need more.
// A journal costs about 17 bytes per entry after `zstd -19`, whatever
// its values: the sequence number, the two times, the entry's place in
// the entry list and its hash differ from entry to entry. The many set's
// 2,000 entries take 35 to 40 KB.
const defaultFixtureSizeLimit = 16 << 10

var fixtureSizeLimits = map[string]int64{"many": 48 << 10}

// fixtureInfo is one object of testdata/fixtures.json: a journal fixture
// and the counts journalctl gives for it.
type fixtureInfo = journalfixture.Info

// renderCase is one object of testdata/render_cases.json: one entry, as
// its fields are stored in a journal, and what journalctl prints for it.
type renderCase struct {
	// Name is the hand-written case's id, with ".N" added for the N-th
	// entry of a case of several entries ("A01.2").
	Name string `json:"name"`
	// Realtime is the ENTRY object's realtime, in microseconds since the
	// epoch.
	Realtime uint64 `json:"realtime"`
	// Fields are the entry's items in stored order (sorted by the offset
	// of their DATA objects), without _BOOT_ID, which `journalctl -o
	// export` prints apart from the other fields.
	Fields []renderCaseField `json:"fields"`
	// Expected is journalctl's output for the entry, every line with its
	// "\n"; empty for an entry journalctl does not print.
	Expected string `json:"expected"`
}

// renderCaseField is one FIELD=value item of a renderCase. The value is
// base64 because it may hold NUL bytes and invalid UTF-8.
type renderCaseField struct {
	Name     string `json:"name"`
	ValueB64 string `json:"value_b64"`
}

// entryFirstLine matches the first line of an entry in journalctl's
// text: a line starting with the 32-byte UTC time. A continuation line
// of a multi-line message starts with spaces instead.
var entryFirstLine = regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}\+00:00`)

// fixtureJournal decompresses testdata/NAME.journal.zst into a new
// temporary directory and returns the path of NAME.journal there
// (journalfixture.Journal).
func fixtureJournal(tb testing.TB, name string) string {
	tb.Helper()
	return journalfixture.Journal(tb, name)
}

// fixtureDir decompresses the parts of the chain in testdata/DIR into a
// new temporary directory and returns it (journalfixture.ChainDir).
func fixtureDir(tb testing.TB, dir string) string {
	tb.Helper()
	return journalfixture.ChainDir(tb, dir)
}

// readFixtureManifest returns the objects of testdata/fixtures.json.
func readFixtureManifest(tb testing.TB) []fixtureInfo {
	tb.Helper()
	return journalfixture.Manifest(tb)
}

// renderCases returns the objects of testdata/render_cases.json.zst, in
// the file's order.
func renderCases(tb testing.TB) []renderCase {
	tb.Helper()
	data := journalfixture.Decompressed(tb, filepath.Join(fixtureRoot, "render_cases.json.zst"))
	var cases []renderCase
	if err := json.Unmarshal(data, &cases); err != nil {
		tb.Fatalf("decode the render cases: %v", err)
	}
	return cases
}

// storedJournals lists the journal fixtures on disk as manifest names.
func storedJournals(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, pattern := range []string{"*.journal.zst", "*/*.journal.zst"} {
		stored, err := filepath.Glob(filepath.Join(fixtureRoot, pattern))
		if err != nil {
			t.Fatalf("list the journal fixtures: %v", err)
		}
		for _, file := range stored {
			rel, err := filepath.Rel(fixtureRoot, file)
			if err != nil {
				t.Fatalf("name of %s: %v", file, err)
			}
			names = append(names, strings.TrimSuffix(filepath.ToSlash(rel), ".journal.zst"))
		}
	}
	slices.Sort(names)
	return names
}

func TestFixtures_ManifestListsEveryStoredJournal(t *testing.T) {
	fixtures := readFixtureManifest(t)
	var listed []string
	for _, f := range fixtures {
		listed = append(listed, f.Name)
	}
	slices.Sort(listed)
	if stored := storedJournals(t); !slices.Equal(listed, stored) {
		t.Fatalf("fixtures.json lists %q, testdata holds %q", listed, stored)
	}
	for _, set := range fixtureSets {
		for _, variant := range fixtureVariants {
			if !slices.Contains(listed, set+"-"+variant) {
				t.Errorf("no fixture %s-%s", set, variant)
			}
		}
	}
	for _, chain := range fixtureChains {
		if !slices.ContainsFunc(listed, func(name string) bool { return strings.HasPrefix(name, chain+"/") }) {
			t.Errorf("no part of the chain %s", chain)
		}
	}
}

func TestFixtures_EveryJournalIsSmallStartsWithTheSignatureAndHasItsText(t *testing.T) {
	signature := []byte("LPKSHHRH")
	for _, f := range readFixtureManifest(t) {
		t.Run(f.Name, func(t *testing.T) {
			info, err := os.Stat(f.CompressedPath())
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			limit, ok := fixtureSizeLimits[f.Set]
			if !ok {
				limit = defaultFixtureSizeLimit
			}
			if info.Size() >= limit {
				t.Errorf("stored size %d bytes, want under %d", info.Size(), limit)
			}
			journal, err := os.ReadFile(fixtureJournal(t, f.Name))
			if err != nil {
				t.Fatalf("read the decompressed journal: %v", err)
			}
			if !bytes.HasPrefix(journal, signature) {
				t.Errorf("the journal starts with %q, want %q", journal[:min(len(journal), 8)], signature)
			}
			if _, err := os.Stat(f.TextPath()); err != nil {
				t.Errorf("no text for the journal: %v", err)
			}
		})
	}
}

func TestFixtures_ChainDirectoryHoldsEveryPartUnderItsJournalName(t *testing.T) {
	fixtures := readFixtureManifest(t)
	for _, chain := range fixtureChains {
		var want []string
		for _, f := range fixtures {
			if f.Set == chain {
				want = append(want, path.Base(f.Name)+".journal")
			}
		}
		dir := fixtureDir(t, chain)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("list %s: %v", dir, err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			if !bytes.HasPrefix(data, []byte("LPKSHHRH")) {
				t.Errorf("%s/%s does not start with the journal signature", chain, e.Name())
			}
		}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s decompresses to %q, want %q", chain, got, want)
		}
	}
}

func TestFixtures_CountsAgreeWithTheText(t *testing.T) {
	// Every variant of a set shows the entries of the set's one text; the
	// parts of a chain together show the entries of its all.txt.
	shownInChain := map[string]int64{}
	for _, f := range readFixtureManifest(t) {
		if f.Entries != f.Shown+f.WithoutMessage {
			t.Errorf("%s: %d entries, %d shown and %d without a message", f.Name, f.Entries, f.Shown, f.WithoutMessage)
		}
		if f.IsChainPart() {
			shownInChain[f.TextPath()] += f.Shown
			continue
		}
		if got := shownEntries(t, f.TextPath()); got != f.Shown {
			t.Errorf("%s shows %d entries, fixtures.json says %d for %s", f.TextPath(), got, f.Shown, f.Name)
		}
	}
	for text, shown := range shownInChain {
		if got := shownEntries(t, text); got != shown {
			t.Errorf("%s shows %d entries, its parts in fixtures.json %d", text, got, shown)
		}
	}
}

// shownEntries counts the entries a text of journalctl shows.
func shownEntries(t *testing.T, text string) int64 {
	t.Helper()
	data, err := os.ReadFile(text)
	if err != nil {
		t.Fatalf("read %s: %v", text, err)
	}
	return int64(len(entryFirstLine.FindAllIndex(data, -1)))
}

func TestFixtures_EachSetHasOneText(t *testing.T) {
	for _, set := range fixtureSets {
		text := filepath.Join(fixtureRoot, set+".txt")
		data, err := os.ReadFile(text)
		if err != nil {
			t.Fatalf("read %s: %v", text, err)
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", text)
		}
		perVariant, err := filepath.Glob(filepath.Join(fixtureRoot, set+"-*.txt"))
		if err != nil {
			t.Fatalf("list the texts of %s: %v", set, err)
		}
		if len(perVariant) > 0 {
			t.Errorf("texts of single variants beside %s: %q", text, perVariant)
		}
	}
}

func TestFixtures_RenderCasesDecode(t *testing.T) {
	const handWrittenCases = 166
	cases := renderCases(t)
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, c := range cases {
		if names[c.Name] {
			t.Errorf("render case %s appears twice", c.Name)
		}
		names[c.Name] = true
		id, _, _ := strings.Cut(c.Name, ".")
		ids[id] = true
		if c.Realtime == 0 {
			t.Errorf("render case %s has no realtime", c.Name)
		}
		if len(c.Fields) == 0 {
			t.Errorf("render case %s has no field", c.Name)
		}
		for _, field := range c.Fields {
			if _, err := base64.StdEncoding.DecodeString(field.ValueB64); err != nil {
				t.Errorf("render case %s, field %s: %v", c.Name, field.Name, err)
			}
		}
	}
	if len(ids) != handWrittenCases {
		t.Errorf("render_cases.json holds %d cases, want %d", len(ids), handWrittenCases)
	}
}
