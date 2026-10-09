package logchain

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Each name template finds its chain, names it, and puts its parts in
// the provisional order, oldest first: by key in the template's
// direction, the active part last.
func TestGroup_EachTemplateFindsItsChainInProvisionalOrder(t *testing.T) {
	cases := []struct {
		label    string
		files    []string
		chain    string
		parts    []string
		template string // the template of the oldest part
		key      string // the key of the oldest part as its name writes it
	}{
		{
			label: "numbered", files: []string{"syslog", "syslog.1"},
			chain: "syslog", parts: []string{"syslog.1", "syslog"}, template: "numbered", key: "1",
		},
		{
			label: "numbered keys are numbers, not text",
			files: []string{
				"dpkg.log", "dpkg.log.1", "dpkg.log.2.gz", "dpkg.log.3.gz", "dpkg.log.4.gz", "dpkg.log.5.gz",
				"dpkg.log.6.gz", "dpkg.log.7.gz", "dpkg.log.8.gz", "dpkg.log.9.gz", "dpkg.log.10.gz", "dpkg.log.11.gz",
			},
			chain: "dpkg.log",
			parts: []string{
				"dpkg.log.11.gz", "dpkg.log.10.gz", "dpkg.log.9.gz", "dpkg.log.8.gz", "dpkg.log.7.gz", "dpkg.log.6.gz",
				"dpkg.log.5.gz", "dpkg.log.4.gz", "dpkg.log.3.gz", "dpkg.log.2.gz", "dpkg.log.1", "dpkg.log",
			},
			template: "numbered", key: "11",
		},
		{
			label: "numbered from 0", files: []string{"dmesg", "dmesg.0", "dmesg.1.gz"},
			chain: "dmesg", parts: []string{"dmesg.1.gz", "dmesg.0", "dmesg"}, template: "numbered", key: "1",
		},
		{
			label: "dated with an epoch suffix",
			files: []string{"syslog-20260401-1775001601.gz", "syslog-20260331-1774915200.gz", "syslog"},
			chain: "syslog", parts: []string{"syslog-20260331-1774915200.gz", "syslog-20260401-1775001601.gz", "syslog"},
			template: "dated", key: "20260331-1774915200",
		},
		{
			label: "dated by the hour", files: []string{"app.log.2026-10-01_12", "app.log.2026-10-01_11"},
			chain: "app.log", parts: []string{"app.log.2026-10-01_11", "app.log.2026-10-01_12"},
			template: "dated", key: "2026-10-01_11",
		},
		{
			label: "dated with a time of day", files: []string{"0.log.20261001-130000", "0.log.20261001-120000", "0.log"},
			chain: "0.log", parts: []string{"0.log.20261001-120000", "0.log.20261001-130000", "0.log"},
			template: "dated", key: "20261001-120000",
		},
		{
			label: "dated by epoch", files: []string{"access_log.1775001600", "access_log.1774915200"},
			chain: "access_log", parts: []string{"access_log.1774915200", "access_log.1775001600"},
			template: "dated", key: "1774915200",
		},
		{
			label: "dated by yyyymmddhh", files: []string{"x.log.2026100112", "x.log.2026100111.gz", "x.log"},
			chain: "x.log", parts: []string{"x.log.2026100111.gz", "x.log.2026100112", "x.log"},
			template: "dated", key: "2026100111",
		},
		{
			label: "numbered before the extension", files: []string{"app.1.log.gz", "app.2.log", "app.log"},
			chain: "app.log", parts: []string{"app.2.log", "app.1.log.gz", "app.log"}, template: "numbered-ext", key: "2",
		},
		{
			label: "dated before the extension, then a number",
			files: []string{"app-2026-10-01.3.log.gz", "app-2026-10-01.2.log.gz", "app-2026-09-30.7.log.gz"},
			chain: "app.log", parts: []string{"app-2026-09-30.7.log.gz", "app-2026-10-01.2.log.gz", "app-2026-10-01.3.log.gz"},
			template: "dated-ext", key: "2026-09-30.7",
		},
		{
			label:    "dated before the extension, to the millisecond",
			files:    []string{"app-2026-10-01T13-00-00.000.log.gz", "app-2026-10-01T12-00-00.000.log.gz", "app.log"},
			chain:    "app.log",
			parts:    []string{"app-2026-10-01T12-00-00.000.log.gz", "app-2026-10-01T13-00-00.000.log.gz", "app.log"},
			template: "dated-ext", key: "2026-10-01T12-00-00.000",
		},
		{
			label: "dated before the extension, no active file",
			files: []string{"postgresql-2026-10-02_000000.log", "postgresql-2026-10-01_000000.log"},
			chain: "postgresql.log", parts: []string{"postgresql-2026-10-01_000000.log", "postgresql-2026-10-02_000000.log"},
			template: "dated-ext", key: "2026-10-01_000000",
		},
		{
			label: "logback: a higher index the same day is newer",
			files: []string{"app-2025-02-15.1.log.gz", "app-2025-02-15.0.log.gz", "app.log"},
			chain: "app.log", parts: []string{"app-2025-02-15.0.log.gz", "app-2025-02-15.1.log.gz", "app.log"},
			template: "dated-ext", key: "2025-02-15.0",
		},
		{
			// A four-digit number from 1970 to 2100 where a rotation
			// number goes is a year, and a later year is newer.
			label: "yearly parts", files: []string{"report.2024", "report.2023.gz", "report.2025", "report"},
			chain: "report", parts: []string{"report.2023.gz", "report.2024", "report.2025", "report"},
			template: "numbered", key: "2023",
		},
		{
			label: "yearly parts before the extension", files: []string{"app.2024.log", "app.2023.log.gz", "app.log"},
			chain: "app.log", parts: []string{"app.2023.log.gz", "app.2024.log", "app.log"},
			template: "numbered-ext", key: "2023",
		},
		{
			label: "numbers outside the years are rotation numbers", files: []string{"x.1969", "x.2101", "x"},
			chain: "x", parts: []string{"x.2101", "x.1969", "x"}, template: "numbered", key: "2101",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := Group(testDir, fakeEntries(tc.files...), allText)
			if len(got) != 1 {
				t.Fatalf("chains %v, want one named %q", chainsByName(got), tc.chain)
			}
			c := got[0]
			if c.Name != tc.chain || c.Dir != testDir || c.Handle() != filepath.Join(testDir, tc.chain) {
				t.Fatalf("chain %q in %q (handle %q), want %q", c.Name, c.Dir, c.Handle(), tc.chain)
			}
			if names := partNames(c); !slices.Equal(names, tc.parts) {
				t.Fatalf("parts %v\nwant  %v", names, tc.parts)
			}
			oldest := c.Parts[0]
			if oldest.Template != tc.template || oldest.Key.Text != tc.key {
				t.Fatalf("oldest part template %q key %q, want %q %q", oldest.Template, oldest.Key.Text, tc.template, tc.key)
			}
			wantActive := slices.Contains(tc.files, tc.chain)
			if c.HasActive() != wantActive {
				t.Fatalf("has active %v, want %v", c.HasActive(), wantActive)
			}
			for i, p := range c.Parts {
				isLast := i == len(c.Parts)-1
				if p.IsActive != (wantActive && isLast) {
					t.Fatalf("part %s active %v", p.Name, p.IsActive)
				}
				if p.IsActive && (p.Key.Kind != KeyNone || p.Template != "") {
					t.Fatalf("active part has key %+v template %q", p.Key, p.Template)
				}
				if p.Path != filepath.Join(testDir, p.Name) {
					t.Fatalf("part path %q", p.Path)
				}
			}
		})
	}
}

// Names that look rotated but are not a chain stay out of every chain.
func TestGroup_LookAlikesFormNoChain(t *testing.T) {
	cases := []struct {
		label string
		files []string
		want  map[string][]string
	}{
		{"numbers before the extension without the active file", []string{"node-1.log", "node-2.log"}, map[string][]string{}},
		{"numbers before the extension with the active file", []string{"node-1.log", "node-2.log", "node.log"},
			map[string][]string{"node.log": {"node-2.log", "node-1.log", "node.log"}}},
		{"one file with a year-like number", []string{"report.2024"}, map[string][]string{}},
		{"one file", []string{"syslog.1"}, map[string][]string{}},
		{"a .tmp file is not a part", []string{"app.log", "app.log.1.tmp"}, map[string][]string{}},
		{"a .tmp file beside a chain", []string{"app.log", "app.log.1", "app.log.2.tmp"},
			map[string][]string{"app.log": {"app.log.1", "app.log"}}},
		{"hidden names", []string{".syslog", ".syslog.1", ".syslog.2"}, map[string][]string{}},
		{"unrelated names", []string{"README", "notes.txt", "lastlog", "fontconfig.log"}, map[string][]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := chainsByName(Group(testDir, fakeEntries(tc.files...), allText))
			if len(got) != len(tc.want) {
				t.Fatalf("chains %v, want %v", got, tc.want)
			}
			for name, parts := range tc.want {
				if !slices.Equal(got[name], parts) {
					t.Fatalf("chain %s: %v, want %v", name, got[name], parts)
				}
			}
		})
	}
}

// A directory is never a part and never the active part.
func TestGroup_DirectoriesAreNotParts(t *testing.T) {
	entries := fakeEntries("syslog", "syslog.2")
	entries = append(entries, Entry{Name: "syslog.1", Path: filepath.Join(testDir, "syslog.1"),
		Info: fakeInfo{name: "syslog.1", dir: true, mtime: baseTime}})
	got := chainsByName(Group(testDir, entries, allText))
	if !slices.Equal(got["syslog"], []string{"syslog.2", "syslog"}) || len(got) != 1 {
		t.Fatalf("chains %v", got)
	}

	entries = fakeEntries("app.log.1", "app.log.2")
	entries = append(entries, Entry{Name: "app.log", Path: filepath.Join(testDir, "app.log"),
		Info: fakeInfo{name: "app.log", dir: true, mtime: baseTime}})
	got = chainsByName(Group(testDir, entries, allText))
	if !slices.Equal(got["app.log"], []string{"app.log.2", "app.log.1"}) {
		t.Fatalf("a directory named like the chain became its active part: %v", got)
	}
}

// Numbered and dated parts of one name are one chain. The provisional
// order puts the kind of key whose newest part is older first, and each
// kind in its own key order.
func TestGroup_NumberedAndDatedPartsOfOneNameAreOneChain(t *testing.T) {
	mtimes := map[string]time.Time{
		"syslog-20260930.gz": time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC),
		"syslog-20261001.gz": time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC),
		"syslog.2.gz":        time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC),
		"syslog.1":           time.Date(2026, 10, 3, 2, 0, 0, 0, time.UTC),
		"syslog":             time.Date(2026, 10, 4, 2, 0, 0, 0, time.UTC),
	}
	var entries []Entry
	for _, name := range []string{"syslog.1", "syslog-20261001.gz", "syslog", "syslog.2.gz", "syslog-20260930.gz"} {
		entries = append(entries, Entry{Name: name, Path: filepath.Join(testDir, name),
			Info: fakeInfo{name: name, size: 10, mtime: mtimes[name]}})
	}
	got := Group(testDir, entries, allText)
	want := []string{"syslog-20260930.gz", "syslog-20261001.gz", "syslog.2.gz", "syslog.1", "syslog"}
	if len(got) != 1 || !slices.Equal(partNames(got[0]), want) {
		t.Fatalf("chains %v, want syslog %v", chainsByName(got), want)
	}

	// The same names when the dated parts are the newer ones.
	for i := range entries {
		info := entries[i].Info.(fakeInfo)
		switch entries[i].Name {
		case "syslog.2.gz":
			info.mtime = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		case "syslog.1":
			info.mtime = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
		}
		entries[i].Info = info
	}
	got = Group(testDir, entries, allText)
	want = []string{"syslog.2.gz", "syslog.1", "syslog-20260930.gz", "syslog-20261001.gz", "syslog"}
	if len(got) != 1 || !slices.Equal(partNames(got[0]), want) {
		t.Fatalf("chains %v, want syslog %v", chainsByName(got), want)
	}
}

// Parts with equal keys fall back to their modification time, then to
// their name, so the order never depends on the listing's order.
func TestGroup_EqualKeysOrderByModificationTime(t *testing.T) {
	older := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	entries := []Entry{
		{Name: "app.log.1", Path: "/logs/app.log.1", Info: fakeInfo{name: "app.log.1", size: 5, mtime: baseTime}},
		{Name: "app.1.log", Path: "/logs/app.1.log", Info: fakeInfo{name: "app.1.log", size: 5, mtime: older}},
		{Name: "app.log", Path: "/logs/app.log", Info: fakeInfo{name: "app.log", size: 5, mtime: baseTime}},
	}
	got := Group(testDir, entries, allText)
	want := []string{"app.1.log", "app.log.1", "app.log"}
	if len(got) != 1 || !slices.Equal(partNames(got[0]), want) {
		t.Fatalf("chains %v, want %v", chainsByName(got), want)
	}
}

// Chains come back sorted by name, case-insensitive, as /v1/tree sorts
// files.
func TestGroup_ChainsAreSortedByNameIgnoringCase(t *testing.T) {
	got := Group(testDir, fakeEntries("b.log", "b.log.1", "A.log.1", "A.log.2", "c.log.1", "c.log.2"), allText)
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"A.log", "b.log", "c.log"}) {
		t.Fatalf("order %v", names)
	}
}

// A name is matched literally: spaces, brackets, regexp metacharacters
// and non-ASCII letters in a stem are only text.
func TestGroup_NamesAreMatchedLiterally(t *testing.T) {
	cases := []struct {
		files []string
		chain string
		parts []string
	}{
		{[]string{"my app (1).log", "my app (1).log.1", "my app (1).log.2.gz"}, "my app (1).log",
			[]string{"my app (1).log.2.gz", "my app (1).log.1", "my app (1).log"}},
		{[]string{"журнал.log", "журнал.log.1"}, "журнал.log", []string{"журнал.log.1", "журнал.log"}},
		{[]string{"a+b*[c].log-20261001", "a+b*[c].log-20261002"}, "a+b*[c].log",
			[]string{"a+b*[c].log-20261001", "a+b*[c].log-20261002"}},
		{[]string{"x^$|.log", "x^$|.1.log"}, "x^$|.log", []string{"x^$|.1.log", "x^$|.log"}},
	}
	for _, tc := range cases {
		got := Group(testDir, fakeEntries(tc.files...), allText)
		if len(got) != 1 || got[0].Name != tc.chain || !slices.Equal(partNames(got[0]), tc.parts) {
			t.Fatalf("%v: chains %v, want %q %v", tc.files, chainsByName(got), tc.chain, tc.parts)
		}
	}
}

// The keys a name gives, as the provisional order compares them.
func TestMatchName_Keys(t *testing.T) {
	ms := func(y int, mo time.Month, d, h, mi, s, milli int) int64 {
		return time.Date(y, mo, d, h, mi, s, milli*int(time.Millisecond), time.UTC).UnixMilli()
	}
	cases := []struct {
		name     string
		chain    string
		kind     KeyKind
		number   int64
		dateMs   int64
		template string
	}{
		{"syslog.1", "syslog", KeyNumber, 1, 0, "numbered"},
		{"dpkg.log.11.gz", "dpkg.log", KeyNumber, 11, 0, "numbered"},
		{"dmesg.0", "dmesg", KeyNumber, 0, 0, "numbered"},
		{"syslog-20260401-1775001601.gz", "syslog", KeyDate, 1775001601, ms(2026, 4, 1, 0, 0, 0, 0), "dated"},
		{"app.log.2026-10-01_12", "app.log", KeyDate, 0, ms(2026, 10, 1, 12, 0, 0, 0), "dated"},
		{"app.log.2026-10-01_12-30", "app.log", KeyDate, 0, ms(2026, 10, 1, 12, 30, 0, 0), "dated"},
		{"app.log.2026-10-01_1230", "app.log", KeyDate, 0, ms(2026, 10, 1, 12, 30, 0, 0), "dated"},
		{"app.log.2026-10-01_12-30-15", "app.log", KeyDate, 0, ms(2026, 10, 1, 12, 30, 15, 0), "dated"},
		{"app.log.2026-10-01T123015", "app.log", KeyDate, 0, ms(2026, 10, 1, 12, 30, 15, 0), "dated"},
		{"0.log.20261001-120000", "0.log", KeyDate, 0, ms(2026, 10, 1, 12, 0, 0, 0), "dated"},
		{"access_log.1774915200", "access_log", KeyDate, 0, 1774915200 * 1000, "dated"},
		{"x.log.2026100112", "x.log", KeyDate, 0, ms(2026, 10, 1, 12, 0, 0, 0), "dated"},
		{"x.log.20261001.bz2", "x.log", KeyDate, 0, ms(2026, 10, 1, 0, 0, 0, 0), "dated"},
		{"app.1.log.gz", "app.log", KeyNumber, 1, 0, "numbered-ext"},
		{"app_7.txt.xz", "app.txt", KeyNumber, 7, 0, "numbered-ext"},
		{"app-2026-10-01.3.log.gz", "app.log", KeyDate, 3, ms(2026, 10, 1, 0, 0, 0, 0), "dated-ext"},
		{"app-2026-10-01T12-00-00.000.log.zst", "app.log", KeyDate, 0, ms(2026, 10, 1, 12, 0, 0, 0), "dated-ext"},
		{"app-2026-10-01T12-00-00.250.log", "app.log", KeyDate, 0, ms(2026, 10, 1, 12, 0, 0, 250), "dated-ext"},
		{"postgresql-2026-10-01_000000.log", "postgresql.log", KeyDate, 0, ms(2026, 10, 1, 0, 0, 0, 0), "dated-ext"},
		// A four-digit number from 1970 to 2100 in a rotation number's
		// place is a year: the key is the start of that year.
		{"report.2023", "report", KeyDate, 0, ms(2023, 1, 1, 0, 0, 0, 0), "numbered"},
		{"report.1970.gz", "report", KeyDate, 0, 0, "numbered"},
		{"report.2100", "report", KeyDate, 0, ms(2100, 1, 1, 0, 0, 0, 0), "numbered"},
		{"app.2024.log.gz", "app.log", KeyDate, 0, ms(2024, 1, 1, 0, 0, 0, 0), "numbered-ext"},
		{"report.1969", "report", KeyNumber, 1969, 0, "numbered"},
		{"report.2101", "report", KeyNumber, 2101, 0, "numbered"},
		{"report.02024", "report", KeyNumber, 2024, 0, "numbered"},
		{"report.999", "report", KeyNumber, 999, 0, "numbered"},
	}
	for _, tc := range cases {
		m, ok := matchName(tc.name)
		if !ok {
			t.Fatalf("%s: no template matched", tc.name)
		}
		if m.chain != tc.chain || m.template.ID != tc.template || m.key.Kind != tc.kind ||
			m.key.Number != tc.number || m.key.DateMs != tc.dateMs {
			t.Fatalf("%s: chain %q template %q key %+v; want %q %q kind %d number %d date %d",
				tc.name, m.chain, m.template.ID, m.key, tc.chain, tc.template, tc.kind, tc.number, tc.dateMs)
		}
	}
	for _, name := range []string{"syslog", "app.log", "README", "x.123456", "x.log.gz", "-20261001", "app.log.1.tmp"} {
		if m, ok := matchName(name); ok && !strings.HasSuffix(name, ".tmp") {
			t.Fatalf("%s matched %q as chain %q", name, m.template.ID, m.chain)
		}
	}
}

// Every template is compiled once, from the table, and names a chain
// that differs from the part's own name.
func TestTemplates_TableIsWellFormed(t *testing.T) {
	ids := map[string]bool{}
	// The provisional order compares the keys of two parts of one kind
	// in one direction, so every row of a kind must share it.
	direction := map[KeyKind]bool{}
	for _, tpl := range Templates {
		if tpl.ID == "" || tpl.Pattern == nil || tpl.Name == nil || tpl.KeyKind == KeyNone || ids[tpl.ID] {
			t.Fatalf("bad template row %+v", tpl)
		}
		ids[tpl.ID] = true
		if newestLow, seen := direction[tpl.KeyKind]; seen && newestLow != tpl.NewestLow {
			t.Fatalf("template %s orders its key kind in the other direction", tpl.ID)
		}
		direction[tpl.KeyKind] = tpl.NewestLow
	}
	for _, want := range []string{"numbered", "dated", "numbered-ext", "dated-ext"} {
		if !ids[want] {
			t.Fatalf("template %q missing", want)
		}
	}
}

// Matching a name never panics, and a match always names a chain that
// is a non-empty name different from the part's, without a separator.
func FuzzMatchName(f *testing.F) {
	for _, seed := range []string{
		"syslog.1", "dpkg.log.11.gz", "syslog-20260401-1775001601.gz", "app.log.2026-10-01_12",
		"0.log.20261001-120000", "access_log.1774915200", "app.1.log.gz", "app-2026-10-01.3.log.gz",
		"app-2026-10-01T12-00-00.000.log.gz", "postgresql-2026-10-01_000000.log", "my app (1).log.2.gz",
		"журнал.log.1", "x.99999", "x.2026-13-45_99", "x.9999999999", ".1", "-20261001",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		m, ok := matchName(name)
		if !ok {
			return
		}
		if m.chain == "" || m.chain == name || strings.ContainsRune(m.chain, '/') && !strings.ContainsRune(name, '/') {
			t.Fatalf("%q: chain %q", name, m.chain)
		}
		if !strings.HasPrefix(name, m.generation) || m.key.Text == "" {
			t.Fatalf("%q: generation %q key %+v", name, m.generation, m.key)
		}
		if utf8.ValidString(name) && !utf8.ValidString(m.chain) {
			t.Fatalf("%q: chain %q is not valid UTF-8", name, m.chain)
		}
	})
}
