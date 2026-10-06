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
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
)

// MaxParts is the most parts one chain may have. A group with more is
// still returned by Group, with Candidate.TooManyParts set, so a
// listing shows it and describing it says why it cannot be read as one
// text.
const MaxParts = 10000

// MaxMissingNames is the most missing parts a chain names
// (Candidate.Missing); Candidate.MissingCount says how many are
// missing in all.
//
// SECURITY: the names are made up from the numbers between the parts,
// not read from the directory, so without a limit a few files could
// make an answer far larger than the directory's listing. With it the
// names of one chain take at most MaxMissingNames times the length of
// one file name.
const MaxMissingNames = 100

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
	// ReadError is the error the text check met when it could not open
	// or state the file (its permissions, an I/O error, or its name led
	// to another file or to none by then); nil for a part that was
	// classified. Such a part stays in the chain, so a description can
	// say why the chain cannot be read; Format is then FormatNone.
	ReadError error
	// NotText is the reason the text check gave (filekind.Kind.NotText)
	// for a part that is not text, which only a journal chain keeps (see
	// Group): a binary file named system.journal, or a journal whose
	// header rx cannot read. Describing the chain says that such a part
	// cannot be read as a journal. Empty for every other part.
	NotText string

	// newestLow is the direction of the part's key (nameMatch), kept
	// for ordering.
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
	// Missing are the names absent numbered parts would have, the
	// lowest numbers first, at most MaxMissingNames of them; never nil.
	Missing []string
	// MissingCount is how many numbered parts are missing, whether
	// Missing names them all or stops at MaxMissingNames.
	MissingCount int
	// TooManyParts says that the chain has more than MaxParts parts.
	// Such a chain is not read as one text, and its files are classified
	// only until that is known: Parts then holds the first MaxParts+1
	// parts found, in the order of the directory listing (by name), the
	// files after them are left out, and Missing is empty.
	TooManyParts bool
	// DirInfo is the stat of the chain's directory as Resolve pinned it,
	// with every symbolic link on the way resolved: its device and inode
	// name the directory whatever path spelled it, another case on a
	// case-insensitive disk or a link to it. Nil for a candidate Group
	// found from a listing alone.
	DirInfo os.FileInfo
	// NamedParts is how many parts the names of the chain's files give:
	// one per generation, the active file included, counted before any
	// file is read. In a chain of text logs, files that turn out not to
	// be text make it larger than len(Parts); a journal chain keeps them
	// as parts (Part.NotText). For a chain of more than MaxParts parts it
	// is the count its description gives.
	NamedParts int
}

// Handle is the chain's handle: its directory joined with its name.
func (c Candidate) Handle() string { return filepath.Join(c.Dir, c.Name) }

// DirectoryIdentity returns the device and inode a directory's stat
// records (Candidate.DirInfo): the pair that names one directory on one
// machine, whatever path spelled it (another case on a case-insensitive
// disk, a symbolic link to it). ok is false when there is no stat, when
// the platform's stat gives no inode, and when the inode is 0: a
// filesystem that numbers no file (some FUSE and network filesystems
// report 0 for every one) would give every directory one pair, and so
// name them all as one. The caller then keys the directory by its path,
// as the chain search and the index tasks of chains do.
func DirectoryIdentity(info os.FileInfo) (device, inode uint64, ok bool) {
	if info == nil {
		return 0, 0, false
	}
	inode, device, ok = index.InodeAndDevice(info)
	if !ok || inode == 0 {
		return 0, 0, false
	}
	return device, inode, true
}

// HasActive reports whether the chain's active file exists.
func (c Candidate) HasActive() bool {
	return len(c.Parts) > 0 && c.Parts[len(c.Parts)-1].IsActive
}

// IsJournal reports whether c is a journal chain: rotated systemd
// journals, a frozen part of which has a name of the journal row
// (`system@….journal`). Such a chain is ordered by its parts' headers,
// not by the timestamps of their text, and each of its parts must be a
// journal (see Describe).
func (c Candidate) IsJournal() bool {
	for _, p := range c.Parts {
		if p.Template == journalTemplateID {
			return true
		}
	}
	return false
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
// a chain name some other file gives: that file is the active part. When
// archives of a journal chain are in the listing (`user-1000@….journal`),
// the file named like that chain is its active part even when its name
// also matches another row (`user-1000.journal`, see groupNamed).
// Left out are directories, hidden entries (unless hidden entries are
// on), names ending in `.tmp`, files whose text check says they are not
// text (classify; a journal chain keeps them as parts, with the reason
// in Part.NotText, see journalChainKeeps), and the parts of a template
// that needs the active file when that file is absent or, in a chain of
// text logs, not text (a journal chain keeps an active file that is not
// text, and those parts with it). A
// file the text check cannot read stays a part, with the error
// (Part.ReadError): the chain it belongs to cannot be read as one text,
// and its description says why. One generation in several encodings is
// one part (see choosePart). A group needs two parts to be a chain.
//
// classify is called only for entries that can form a chain: the names
// of a group that holds two names or more, so the files of a large
// directory that no template matches are never opened. In a chain of
// more than MaxParts parts it is called only until MaxParts+1 parts are
// found (see Candidate.TooManyParts). The candidates come back sorted
// by name, case-insensitive, as /v1/tree sorts files.
//
// The work is bounded by the number of entries: at most one regexp
// match per row for each name, one classify per entry at most, and a
// sort per chain.
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
//
// When archives of a journal chain are in the listing (names of the
// journal row, `user-1000@….journal`), the file named like that chain is
// its active part, whatever other row its name also matches: journald's
// user journal `user-1000.journal` has the shape of a numbered part
// before an extension, but beside its archives it is their active file,
// not the 1000th rotation of `user.journal`. Without such archives the
// name is matched as any other: `user-1000.journal` (or `app-1.journal`)
// stays a numbered part of `user.journal` (`app.journal`).
func groupNamed(dir string, entries []Entry, classify Classify, only string) []Candidate {
	matched := matchEntries(entries)
	journalChains := journalChainNames(matched)
	byChain := map[string][]member{}
	bare := map[string]Entry{}
	for _, m := range matched {
		_, namesJournalChain := journalChains[m.entry.Name]
		if !m.ok || namesJournalChain {
			bare[m.entry.Name] = m.entry
			continue
		}
		if only != "" && m.match.chain != only {
			continue
		}
		byChain[m.match.chain] = append(byChain[m.match.chain], m.member)
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

// matchedEntry is an entry that may be a part, with what the template
// table says about its name; ok is false when no row matches it.
type matchedEntry struct {
	member
	ok bool
}

// matchEntries matches the name of every entry that may be a part
// (isCandidateFile) against the template table, once each, in the
// listing's order.
func matchEntries(entries []Entry) []matchedEntry {
	matched := make([]matchedEntry, 0, len(entries))
	for _, e := range entries {
		if !isCandidateFile(e) {
			continue
		}
		m, ok := matchName(e.Name)
		matched = append(matched, matchedEntry{member: member{entry: e, match: m}, ok: ok})
	}
	return matched
}

// journalChainNames are the chain names the names of the journal row
// give, as a set.
func journalChainNames(matched []matchedEntry) map[string]struct{} {
	names := map[string]struct{}{}
	for _, m := range matched {
		if m.ok && m.match.template.ID == journalTemplateID {
			names[m.match.chain] = struct{}{}
		}
	}
	return names
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
	// The rule comes from the members' names, before any file is read:
	// a journal chain keeps every member, text or not.
	keeps := keepRuleOf(members)
	if !hasActive {
		members = withoutActiveOnlyParts(members)
	}
	// Two generations at least, counting the active file: otherwise no
	// text check could make a chain, and none is made.
	named := countGenerations(members) + boolCount(hasActive)
	if named < 2 {
		return Candidate{}, false
	}

	var activePart Part
	if hasActive {
		if checked := textCheck(active, classify); keeps(checked) {
			activePart = Part{
				Name: active.Name, Path: active.Path, IsActive: true, Duplicates: []string{},
				Info: active.Info, Format: checked.format, File: active.File, ReadError: checked.readErr,
				NotText: checked.notText,
			}
		} else {
			hasActive = false
			members = withoutActiveOnlyParts(members)
		}
	}

	// SECURITY: the frozen parts are classified only until the chain is
	// known to have more than MaxParts parts, the active file counted.
	// Such a chain is not read as one text, so classifying the rest of a
	// directory of rotated names would be work for nothing: a listing
	// costs at most MaxParts+1 classifications per chain, however many
	// files share its name.
	parts := chooseParts(members, classify, keeps, MaxParts+1-boolCount(hasActive))
	if hasActive {
		parts = append(parts, activePart)
	}
	if len(parts) < 2 {
		return Candidate{}, false
	}

	c := Candidate{Dir: dir, Name: chain, Parts: parts, Missing: []string{}, NamedParts: named}
	c.TooManyParts = len(parts) > MaxParts
	if !c.TooManyParts {
		// The parts of a chain that is too large stop where it became
		// known, so their numbers would name holes that are not there.
		c.Missing, c.MissingCount = missingNumbers(parts)
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

// textResult is what the text check says about an entry.
type textResult struct {
	// format is the entry's compression format, detected from its
	// bytes; FormatNone for an empty file and for one the check could
	// not read.
	format compression.Format
	// notText is why the entry is not text (filekind.Kind.NotText); ""
	// for a text file.
	notText string
	// readErr is the error the check met when it could not open or
	// state the entry; nil when it classified it.
	readErr error
}

// textCheck classifies an entry for membership: its compression format
// and whether it is text, or the error of a check that could not open
// or state it. An empty file is plain text and is not opened.
func textCheck(e Entry, classify Classify) textResult {
	if e.Info.Size() == 0 {
		return textResult{format: compression.FormatNone}
	}
	kind, err := classify(e)
	if err != nil {
		return textResult{format: compression.FormatNone, readErr: err}
	}
	return textResult{format: kind.Format, notText: kind.NotText}
}

// keepsMember says whether a chain keeps an entry as a part, given what
// the text check found about it (checked).
type keepsMember func(checked textResult) bool

// textChainKeeps is the rule of a chain of text logs: it keeps its text
// files, and the files it cannot read so that its description says
// why. A file that is not text (`wtmp.1`, a NUL byte in a log) is no
// part.
func textChainKeeps(checked textResult) bool { return checked.notText == "" }

// journalChainKeeps is the rule of a journal chain: it keeps every
// member. A member that is not text is a journal whose header rx cannot
// read, or a file that is no journal (a binary file named
// system.journal). Leaving it out would leave a chain that looks whole
// while entries are lost, so it stays a part (Part.NotText), and the
// chain's description says that it cannot be read as a journal.
func journalChainKeeps(textResult) bool { return true }

// keepRuleOf is the rule of the chain whose rotated files are members:
// a journal chain's when one of their names is of the journal row, a
// chain of text logs' otherwise.
func keepRuleOf(members []member) keepsMember {
	for _, m := range members {
		if m.match.template.ID == journalTemplateID {
			return journalChainKeeps
		}
	}
	return textChainKeeps
}

// formatPreference ranks the encodings of one generation: the first
// that exists is the one rx reads (plain, seekable zstd, zstd, gzip,
// bzip2, xz, a systemd journal). The format is the detected one, never
// the extension's. The journal row's names have no compression suffix,
// so one of its generations has one encoding; a journal ranks last for
// a generation of another row (`app.log.1` a journal beside
// `app.log.1.gz`).
var formatPreference = map[compression.Format]int{
	compression.FormatNone:         0,
	compression.FormatSeekableZstd: 1,
	compression.FormatZstd:         2,
	compression.FormatGzip:         3,
	compression.FormatBz2:          4,
	compression.FormatXz:           5,
	compression.FormatJournal:      6,
}

// formatRank is a format's place in formatPreference; a format not in
// the table comes after every one that is.
func formatRank(f compression.Format) int {
	if rank, ok := formatPreference[f]; ok {
		return rank
	}
	return len(formatPreference)
}

// encoding is one member of a generation that its chain keeps (its
// keepsMember rule), with what the text check said about it.
//
// Go note: embedding textResult makes its fields (format, notText,
// readErr) fields of encoding too: e.format reads e.textResult.format.
type encoding struct {
	member
	textResult
}

// cannotBeRead is 1 for an encoding that cannot be read as text (the
// text check could not read it, or it is not text) and 0 for a text
// encoding, so a sort puts every text encoding first.
func (e encoding) cannotBeRead() int { return boolCount(e.readErr != nil || e.notText != "") }

// chooseParts makes one part per generation of members, classifying
// them a generation at a time in the order the generations first appear
// (the directory listing's, by name), and stops once it holds limit
// parts: the generations after that are neither classified nor kept. A
// generation makes no part when keeps declines every one of its members
// (in a chain of text logs: when none of them is text).
func chooseParts(members []member, classify Classify, keeps keepsMember, limit int) []Part {
	byGeneration := map[string][]member{}
	var order []string
	for _, m := range members {
		g := m.match.generation
		if _, seen := byGeneration[g]; !seen {
			order = append(order, g)
		}
		byGeneration[g] = append(byGeneration[g], m)
	}

	parts := make([]Part, 0, min(len(order), limit))
	for _, g := range order {
		if len(parts) == limit {
			break
		}
		if part, ok := choosePart(byGeneration[g], classify, keeps); ok {
			parts = append(parts, part)
		}
	}
	return parts
}

// choosePart classifies the members of one generation and makes its
// part: the readable text encoding formatPreference ranks first (then
// the shorter name, then by name), with the other encodings as its
// duplicates in the same order. An encoding that cannot be read as
// text (the text check could not read it, or a journal chain keeps it
// though it is not text) comes after every text one, so it is the part
// only when no encoding is text. It reports false when keeps declines
// every member.
func choosePart(members []member, classify Classify, keeps keepsMember) (Part, bool) {
	encodings := make([]encoding, 0, len(members))
	for _, m := range members {
		if checked := textCheck(m.entry, classify); keeps(checked) {
			encodings = append(encodings, encoding{member: m, textResult: checked})
		}
	}
	if len(encodings) == 0 {
		return Part{}, false
	}
	slices.SortFunc(encodings, func(a, b encoding) int {
		return cmp.Or(
			cmp.Compare(a.cannotBeRead(), b.cannotBeRead()),
			cmp.Compare(formatRank(a.format), formatRank(b.format)),
			cmp.Compare(len(a.entry.Name), len(b.entry.Name)),
			cmp.Compare(a.entry.Name, b.entry.Name))
	})
	chosen := encodings[0]
	duplicates := make([]string, 0, len(encodings)-1)
	for _, other := range encodings[1:] {
		duplicates = append(duplicates, other.entry.Name)
	}
	return Part{
		Name: chosen.entry.Name, Path: chosen.entry.Path, Key: chosen.match.key,
		Duplicates: duplicates, Info: chosen.entry.Info, Template: chosen.match.template.ID,
		Format: chosen.format, File: chosen.entry.File, ReadError: chosen.readErr, NotText: chosen.notText,
		newestLow: chosen.match.newestLow,
	}, true
}

// missingNumbers names the numbers missing from a chain's numbered
// parts: every number from the lowest expected one (0 when a part has
// number 0, else 1) up to the highest present one that no part has. A
// missing part is named like the highest-numbered part, without a
// compression suffix (`dpkg.log.4`, `app.4.log`): its encoding cannot be
// known. Dated parts have no missing numbers, years included; rotation
// skips quiet days.
//
// The numbers are named only when no more of them are missing than
// parts with a number are present. A rotation keeps its parts in an
// unbroken run, so a few holes are parts lost; a span mostly of holes
// (`x.1` beside `x.500`) says the numbers are not a rotation's, and
// naming hundreds of parts that never existed would only hide the
// chain. Then both results are empty.
//
// It returns the names of the lowest MaxMissingNames missing numbers and
// how many are missing in all. The loop below stops at whichever comes
// first: the limit, or the highest present number, which the count rule
// keeps within twice the number of present numbers.
func missingNumbers(parts []Part) (names []string, count int) {
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
	names = []string{}
	if highest == nil {
		return names, 0
	}
	lowest := int64(1)
	if present[0] {
		lowest = 0
	}
	// Every present number lies in [lowest, highest], so the span less
	// the present numbers is how many are missing.
	absent := highest.Key.Number - lowest + 1 - int64(len(present))
	if absent > int64(len(present)) {
		return names, 0
	}
	// The name of the highest-numbered part around its number, without
	// a compression suffix.
	m, _ := matchName(highest.Name)
	for n := lowest; n < highest.Key.Number && len(names) < MaxMissingNames; n++ {
		if !present[n] {
			names = append(names, m.beforeNumber+strconv.FormatInt(n, 10)+m.afterNumber)
		}
	}
	// Go note: the conversion is safe: absent is at most the number of
	// present parts, which is a slice length, an int.
	return names, int(absent)
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
