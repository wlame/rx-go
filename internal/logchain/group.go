package logchain

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
)

// MaxParts is the most parts one chain may have. A group with more is
// still returned by Group, with Candidate.TooManyParts set, so a
// listing shows it and describing it says why it cannot be read as one
// text. It also bounds the missing numbers a chain can name: a numbered
// chain whose numbers span more than MaxParts is marked the same way.
const MaxParts = 10000

// Entry is one entry of a directory listing, as Group takes it. The
// caller fills it from a paths.ListedEntry (EntriesOf) or from a
// paths.WalkEntry, leaving out the entries the listing refused.
type Entry struct {
	// Name is the entry's name in the directory.
	Name string
	// Path is the directory's path joined with Name.
	Path string
	// Info is the entry's stat from the listing; for a symbolic link,
	// the stat of the file it leads to.
	Info os.FileInfo
	// File is the entry pinned by the listing. Every read of the
	// entry's bytes goes through it.
	File paths.Pinned
}

// Part is one file of a chain.
type Part struct {
	// Name is the file's name in the directory.
	Name string
	// Path is the file's path: the chain's directory joined with Name.
	Path string
	// Key is the number or date the name carries; the zero Key (KeyNone)
	// for the active part.
	Key Key
	// IsActive says that this is the active part, the file named like
	// the chain.
	IsActive bool
	// Duplicates are the names of the other encodings of the same
	// generation (`x.log.1.gz` beside `x.log.1`), in the order of
	// preference; never nil.
	Duplicates []string
	// Info is the file's stat from the listing.
	Info os.FileInfo
	// Template is the ID of the name template the name matched; "" for
	// the active part.
	Template string
	// Format is the file's compression format, detected from its bytes
	// when it was listed (compression.FormatNone for a plain file).
	Format compression.Format
	// File is the file pinned by the listing; read the part through it.
	File paths.Pinned

	// newestLow is the template's direction, kept for ordering.
	newestLow bool
}

// Candidate is a chain found by name: two parts or more of one chain
// name in one directory. Its parts are in the provisional order.
type Candidate struct {
	// Dir is the chain's directory, Name its chain name.
	Dir, Name string
	// Parts are the chain's parts in the provisional order, oldest
	// first; the active part, when it exists, is last.
	Parts []Part
	// Missing are the names absent numbered parts would have; never
	// nil.
	Missing []string
	// TooManyParts says that the chain has more than MaxParts parts, or
	// that its numbers span more than MaxParts (Missing is then empty).
	TooManyParts bool
}

// Handle is the chain's handle: its directory joined with its name.
func (c Candidate) Handle() string { return filepath.Join(c.Dir, c.Name) }

// HasActive reports whether the chain's active file exists.
func (c Candidate) HasActive() bool {
	return len(c.Parts) > 0 && c.Parts[len(c.Parts)-1].IsActive
}

// Classify decides what an entry holds: its compression format and
// whether its text is text, by the rule every command applies
// (filekind). An error means the entry cannot be read.
//
// Group calls it only for the entries that can form a chain, once per
// entry, never for an empty file (which is plain text).
type Classify func(Entry) (filekind.Kind, error)

// ClassifyPinned is the Classify every caller uses: filekind.OfPinned
// on the entry's pin, the check /v1/tree makes for is_text (at most the
// file's signature, its seek table and 8 KiB of its text).
func ClassifyPinned(e Entry) (filekind.Kind, error) {
	return filekind.OfPinned(e.File)
}

// Group finds the chains among the entries of one directory, dir.
//
// A file belongs to a chain when its name matches a row of Templates
// (the chain name comes from the row), or when it is named exactly like
// a chain name some other file gives: that file is the active part.
// Left out are directories, hidden entries (unless hidden entries are
// on), names ending in `.tmp`, files whose text check fails or that
// cannot be read (classify), and the parts of a template that needs the
// active file when that file is absent or not text. One generation in
// several encodings is one part (see choosePart). A group needs two
// parts to be a chain.
//
// classify is called only for entries that can form a chain: the names
// of a group that holds two names or more, so the files of a large
// directory that no template matches are never opened. The candidates
// come back sorted by name, case-insensitive, as /v1/tree sorts files.
//
// The work is bounded by the number of entries: one regexp match per
// name (four at most), one classify per entry at most, and a sort per
// chain.
func Group(dir string, entries []Entry, classify Classify) []Candidate {
	return groupNamed(dir, entries, classify, "")
}

// member is an entry whose name matched a template.
type member struct {
	entry Entry
	match nameMatch
}

// groupNamed is Group, restricted to the chain named only when only is
// not empty: the other groups are neither classified nor returned.
func groupNamed(dir string, entries []Entry, classify Classify, only string) []Candidate {
	byChain := map[string][]member{}
	bare := map[string]Entry{}
	for _, e := range entries {
		if !isCandidateFile(e) {
			continue
		}
		m, ok := matchName(e.Name)
		if !ok {
			bare[e.Name] = e
			continue
		}
		if only != "" && m.chain != only {
			continue
		}
		byChain[m.chain] = append(byChain[m.chain], member{entry: e, match: m})
	}

	out := make([]Candidate, 0, len(byChain))
	for chain, members := range byChain {
		active, hasActive := bare[chain]
		if c, ok := buildCandidate(dir, chain, members, active, hasActive, classify); ok {
			out = append(out, c)
		}
	}
	// Go note: map iteration order is random, so the candidates are
	// sorted before anyone sees them.
	slices.SortFunc(out, func(a, b Candidate) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// isCandidateFile reports whether an entry may be a part at all: a file
// (not a directory), not hidden unless hidden entries are on, and not a
// `.tmp` file (a rotation or a compression in progress).
func isCandidateFile(e Entry) bool {
	return e.Info != nil && !e.Info.IsDir() && !paths.SkipEntry(e.Name) && !strings.HasSuffix(e.Name, ".tmp")
}

// buildCandidate makes the chain of one chain name from the entries
// whose names matched a template (members) and the active file, when
// one exists. It reports false when they do not form a chain.
func buildCandidate(dir, chain string, members []member, active Entry, hasActive bool, classify Classify) (Candidate, bool) {
	if !hasActive {
		members = withoutActiveOnlyParts(members)
	}
	// Two generations at least, counting the active file: otherwise no
	// text check could make a chain, and none is made.
	if countGenerations(members)+boolCount(hasActive) < 2 {
		return Candidate{}, false
	}

	var activePart Part
	if hasActive {
		format, ok := textFormat(active, classify)
		if ok {
			activePart = Part{
				Name: active.Name, Path: active.Path, IsActive: true, Duplicates: []string{},
				Info: active.Info, Format: format, File: active.File,
			}
		} else {
			hasActive = false
			members = withoutActiveOnlyParts(members)
		}
	}

	parts := chooseParts(members, classify)
	if hasActive {
		parts = append(parts, activePart)
	}
	if len(parts) < 2 {
		return Candidate{}, false
	}

	c := Candidate{Dir: dir, Name: chain, Parts: parts}
	c.Missing, c.TooManyParts = missingNumbers(parts)
	if len(parts) > MaxParts {
		c.TooManyParts = true
	}
	sortProvisional(c.Parts)
	return c, true
}

// withoutActiveOnlyParts drops the members of templates that need the
// active file.
func withoutActiveOnlyParts(members []member) []member {
	return slices.DeleteFunc(members, func(m member) bool { return m.match.template.NeedsActive })
}

// countGenerations counts the distinct generations among members.
func countGenerations(members []member) int {
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		seen[m.match.generation] = struct{}{}
	}
	return len(seen)
}

// boolCount is 1 for true and 0 for false.
func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

// textFormat classifies an entry and returns its compression format, or
// false when the entry is not text or cannot be read. An empty file is
// plain text and is not opened.
func textFormat(e Entry, classify Classify) (compression.Format, bool) {
	if e.Info.Size() == 0 {
		return compression.FormatNone, true
	}
	kind, err := classify(e)
	if err != nil || !kind.IsText() {
		return compression.FormatNone, false
	}
	return kind.Format, true
}

// formatPreference ranks the encodings of one generation: the first
// that exists is the one rx reads (plain, seekable zstd, zstd, gzip,
// bzip2, xz). The format is the detected one, never the extension's.
var formatPreference = map[compression.Format]int{
	compression.FormatNone:         0,
	compression.FormatSeekableZstd: 1,
	compression.FormatZstd:         2,
	compression.FormatGzip:         3,
	compression.FormatBz2:          4,
	compression.FormatXz:           5,
}

// formatRank is a format's place in formatPreference; a format not in
// the table comes after every one that is.
func formatRank(f compression.Format) int {
	if rank, ok := formatPreference[f]; ok {
		return rank
	}
	return len(formatPreference)
}

// encoding is one text member of a generation, with its format.
type encoding struct {
	member
	format compression.Format
}

// chooseParts classifies the members, drops those that are not text,
// and makes one part per generation: the encoding formatPreference
// ranks first (then the shorter name, then by name), with the others as
// its duplicates in the same order.
func chooseParts(members []member, classify Classify) []Part {
	byGeneration := map[string][]encoding{}
	var order []string
	for _, m := range members {
		format, ok := textFormat(m.entry, classify)
		if !ok {
			continue
		}
		g := m.match.generation
		if _, seen := byGeneration[g]; !seen {
			order = append(order, g)
		}
		byGeneration[g] = append(byGeneration[g], encoding{member: m, format: format})
	}

	parts := make([]Part, 0, len(order))
	for _, g := range order {
		encodings := byGeneration[g]
		slices.SortFunc(encodings, func(a, b encoding) int {
			return cmp.Or(
				cmp.Compare(formatRank(a.format), formatRank(b.format)),
				cmp.Compare(len(a.entry.Name), len(b.entry.Name)),
				cmp.Compare(a.entry.Name, b.entry.Name))
		})
		chosen := encodings[0]
		duplicates := make([]string, 0, len(encodings)-1)
		for _, other := range encodings[1:] {
			duplicates = append(duplicates, other.entry.Name)
		}
		parts = append(parts, Part{
			Name: chosen.entry.Name, Path: chosen.entry.Path, Key: chosen.match.key,
			Duplicates: duplicates, Info: chosen.entry.Info, Template: chosen.match.template.ID,
			Format: chosen.format, File: chosen.entry.File,
			newestLow: chosen.match.template.NewestLow,
		})
	}
	return parts
}

// missingNumbers names the numbers missing from a chain's numbered
// parts: every number from the lowest expected one (0 when a part has
// number 0, else 1) up to the highest present one that no part has. A
// missing part is named like the highest-numbered part, without a
// compression suffix (`dpkg.log.4`, `app.4.log`): its encoding cannot be
// known. Dated parts have no missing numbers; rotation skips quiet days.
//
// The list is never longer than MaxParts: when the numbers span more,
// it is empty and tooMany is true.
func missingNumbers(parts []Part) (missing []string, tooMany bool) {
	present := map[int64]bool{}
	var highest *Part
	for i := range parts {
		p := &parts[i]
		if p.Key.Kind != KeyNumber {
			continue
		}
		present[p.Key.Number] = true
		if highest == nil || p.Key.Number > highest.Key.Number {
			highest = p
		}
	}
	missing = []string{}
	if highest == nil {
		return missing, false
	}
	lowest := int64(1)
	if present[0] {
		lowest = 0
	}
	if highest.Key.Number-lowest+1 > MaxParts {
		return missing, true
	}
	// The name of the highest-numbered part around its number, without
	// a compression suffix.
	m, _ := matchName(highest.Name)
	for n := lowest; n < highest.Key.Number; n++ {
		if !present[n] {
			missing = append(missing, m.beforeNumber+strconv.FormatInt(n, 10)+m.afterNumber)
		}
	}
	return missing, false
}

// sortProvisional puts a chain's parts in the provisional order, oldest
// first.
//
// The order is: by key in the template's direction (a higher number is
// older, a lower date is older), then by modification time, then by
// name; the active part last. When one chain mixes numbered and dated
// parts, the two kinds cannot be compared by key: the kind whose newest
// part is older (by modification time) comes first, as a whole. The
// order is provisional: the parts' timestamps set the real one.
func sortProvisional(parts []Part) {
	rank := keyKindRanks(parts)
	slices.SortStableFunc(parts, func(a, b Part) int {
		if a.IsActive != b.IsActive {
			if a.IsActive {
				return 1
			}
			return -1
		}
		if c := cmp.Compare(rank[a.Key.Kind], rank[b.Key.Kind]); c != 0 {
			return c
		}
		c := cmp.Or(cmp.Compare(a.Key.DateMs, b.Key.DateMs), cmp.Compare(a.Key.Number, b.Key.Number))
		if a.newestLow {
			c = -c
		}
		return cmp.Or(c, a.Info.ModTime().Compare(b.Info.ModTime()), cmp.Compare(a.Name, b.Name))
	})
}

// keyKindRanks orders the key kinds of a chain's frozen parts: the kind
// whose newest part has the older modification time first, ties by
// kind.
func keyKindRanks(parts []Part) map[KeyKind]int {
	newest := map[KeyKind]time.Time{}
	for _, p := range parts {
		if p.IsActive {
			continue
		}
		if t, ok := newest[p.Key.Kind]; !ok || p.Info.ModTime().After(t) {
			newest[p.Key.Kind] = p.Info.ModTime()
		}
	}
	kinds := make([]KeyKind, 0, len(newest))
	for k := range newest {
		kinds = append(kinds, k)
	}
	slices.SortFunc(kinds, func(a, b KeyKind) int {
		return cmp.Or(newest[a].Compare(newest[b]), cmp.Compare(a, b))
	})
	rank := make(map[KeyKind]int, len(kinds))
	for i, k := range kinds {
		rank[k] = i
	}
	return rank
}
