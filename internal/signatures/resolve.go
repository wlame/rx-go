package signatures

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// SelectionError is a selection that cannot be resolved: an unknown
// word, a word that names a refused or disabled signature, or a
// selection that names nothing. The CLI exits 2 on it, the HTTP API
// answers 400.
type SelectionError struct {
	// Word is the word at fault (or the whole selection when it selects
	// nothing).
	Word string
	// Reason says what is wrong with it.
	Reason string
	// Suggestions are up to five close names, for an unknown word.
	Suggestions []string
	// LoadErrors are the errors that refused the signature the word
	// names.
	LoadErrors []LoadError
}

// Error is one message with the reason, the close names and the load
// errors.
func (e *SelectionError) Error() string {
	var b strings.Builder
	b.WriteString(e.Reason)
	if len(e.Suggestions) > 0 {
		b.WriteString("; close names: ")
		b.WriteString(strings.Join(e.Suggestions, ", "))
	}
	for _, le := range e.LoadErrors {
		b.WriteString("; ")
		b.WriteString(le.Error())
	}
	return b.String()
}

// maxSelectionWords is the most words a selection may have. Each word
// costs a walk of the catalogue, so the bound keeps a selection that
// comes from a request cheap; a real one names a few groups or ids.
const maxSelectionWords = 1000

// The selection words with a meaning of their own, and the prefixes
// that force a namespace.
const (
	wordAll     = "all"
	wordDefault = "default"
	groupPrefix = "group:"
	tagPrefix   = "tag:"
)

// Resolve turns a selection into signatures, in catalogue order.
//
// The selection is comma-separated words, compared without case, applied
// left to right to a set that starts empty:
//
//   - a word adds what it names: an id first, else a group, else a tag;
//     group:x and tag:x name only a group or a tag;
//   - all adds every active signature, default the default set;
//   - -word removes what word names;
//   - an empty selection (no words at all) is the default set.
//
// It returns a *SelectionError for an unknown word (with close names),
// a word naming a refused signature (with its load errors) or a
// disabled one, a selection of more than maxSelectionWords words, and a
// selection that ends up naming nothing.
func (c *Catalogue) Resolve(expr string) ([]*Signature, error) {
	words := selectionWords(expr)
	if len(words) == 0 {
		return c.DefaultSet(), nil
	}
	if len(words) > maxSelectionWords {
		return nil, &SelectionError{Word: expr, Reason: fmt.Sprintf(
			"the selection has %d words; at most %d are taken", len(words), maxSelectionWords)}
	}
	chosen := map[string]bool{}
	for _, word := range words {
		name, removing := strings.CutPrefix(word, "-")
		sigs, err := c.wordSignatures(name, removing)
		if err != nil {
			return nil, err
		}
		for _, s := range sigs {
			if removing {
				delete(chosen, s.ID)
			} else {
				chosen[s.ID] = true
			}
		}
	}
	out := make([]*Signature, 0, len(chosen))
	for _, s := range c.signatures {
		if chosen[s.ID] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, &SelectionError{Word: expr, Reason: fmt.Sprintf("the selection %q selects no signature", expr)}
	}
	return out, nil
}

// selectionWords splits a selection at commas, trims each word, lowers
// its ASCII letters (asciiLower: the one rule the keyword gate and the
// declared keywords follow; ids, groups and tags are kebab-case ASCII,
// so a word with another letter matches nothing either way and is
// echoed as the user wrote it) and drops empty words.
func selectionWords(expr string) []string {
	var words []string
	for _, w := range strings.Split(expr, ",") {
		if w = asciiLower(strings.TrimSpace(w)); w != "" {
			words = append(words, w)
		}
	}
	return words
}

// wordSignatures returns the active signatures one word names. removing
// is true for a word after "-": removing a disabled signature is no
// error, since it is not there to remove.
func (c *Catalogue) wordSignatures(name string, removing bool) ([]*Signature, error) {
	switch {
	case name == "" || name == groupPrefix || name == tagPrefix:
		return nil, &SelectionError{Word: name, Reason: "the selection has an empty word"}
	case name == wordAll:
		return c.signatures, nil
	case name == wordDefault:
		return c.DefaultSet(), nil
	case strings.HasPrefix(name, groupPrefix):
		return c.groupSignatures(strings.TrimPrefix(name, groupPrefix))
	case strings.HasPrefix(name, tagPrefix):
		return c.tagSignatures(strings.TrimPrefix(name, tagPrefix))
	}
	if s, ok := c.byID[name]; ok {
		return []*Signature{s}, nil
	}
	if s, ok := c.disabledByID[name]; ok {
		if removing {
			return nil, nil
		}
		return nil, &SelectionError{Word: name, Reason: fmt.Sprintf("signature %s is disabled by %s", name, s.DisabledBy)}
	}
	if errs := c.refused[name]; len(errs) > 0 {
		return nil, &SelectionError{Word: name, Reason: fmt.Sprintf("signature %s was not loaded", name), LoadErrors: slices.Clone(errs)}
	}
	if c.hasGroup(name) {
		return c.groupSignatures(name)
	}
	if c.hasTag(name) {
		return c.tagSignatures(name)
	}
	return nil, &SelectionError{
		Word:        name,
		Reason:      fmt.Sprintf("unknown signature, group or tag %q", name),
		Suggestions: CloseNames(name, c.allNames()),
	}
}

// groupSignatures returns the active signatures of a group.
func (c *Catalogue) groupSignatures(group string) ([]*Signature, error) {
	if !c.hasGroup(group) {
		return nil, &SelectionError{Word: groupPrefix + group, Reason: fmt.Sprintf("unknown group %q", group),
			Suggestions: CloseNames(group, c.groupNames())}
	}
	var out []*Signature
	for _, s := range c.signatures {
		if s.Group == group {
			out = append(out, s)
		}
	}
	return out, nil
}

// tagSignatures returns the active signatures with a tag.
func (c *Catalogue) tagSignatures(tag string) ([]*Signature, error) {
	if !c.hasTag(tag) {
		return nil, &SelectionError{Word: tagPrefix + tag, Reason: fmt.Sprintf("unknown tag %q", tag),
			Suggestions: CloseNames(tag, c.tagNames())}
	}
	var out []*Signature
	for _, s := range c.signatures {
		if slices.Contains(s.Tags, tag) {
			out = append(out, s)
		}
	}
	return out, nil
}

// hasGroup reports whether a group of that name is declared.
func (c *Catalogue) hasGroup(name string) bool {
	return slices.Contains(c.groupNames(), name)
}

// hasTag reports whether a signature, active or disabled, has the tag.
func (c *Catalogue) hasTag(tag string) bool {
	return slices.Contains(c.tagNames(), tag)
}

// groupNames lists the group names in order.
func (c *Catalogue) groupNames() []string {
	names := make([]string, len(c.groups))
	for i, g := range c.groups {
		names[i] = g.Name
	}
	return names
}

// tagNames lists every tag of the active and disabled signatures, once
// each, sorted.
func (c *Catalogue) tagNames() []string {
	var tags []string
	for _, list := range [][]*Signature{c.signatures, c.disabled} {
		for _, s := range list {
			for _, t := range s.Tags {
				if !slices.Contains(tags, t) {
					tags = append(tags, t)
				}
			}
		}
	}
	slices.Sort(tags)
	return tags
}

// allNames lists every word a selection can use: ids, groups, tags,
// all and default.
func (c *Catalogue) allNames() []string {
	names := []string{wordAll, wordDefault}
	for _, list := range [][]*Signature{c.signatures, c.disabled} {
		for _, s := range list {
			names = append(names, s.ID)
		}
	}
	names = append(names, c.groupNames()...)
	return append(names, c.tagNames()...)
}

// maxSuggestions is how many close names an error lists.
const maxSuggestions = 5

// maxSuggestWordBytes is the longest word close names are looked for:
// a longer word is far from every name (a name is at most 64 bytes),
// and the edit distance costs its length times a name's.
const maxSuggestWordBytes = 128

// CloseNames returns up to five of names close to word: those within a
// small edit distance (at most 2, or a third of the word's length when
// that is more), and those that contain the word when it is 3 bytes or
// longer. They are ordered by distance, then name.
func CloseNames(word string, names []string) []string {
	if len(word) > maxSuggestWordBytes {
		return nil
	}
	limit := max(2, len(word)/3)
	type scored struct {
		name     string
		distance int
	}
	var near []scored
	for _, name := range names {
		d := editDistance(word, name)
		if d <= limit || (len(word) >= minKeywordBytes && strings.Contains(name, word)) {
			if !slices.ContainsFunc(near, func(s scored) bool { return s.name == name }) {
				near = append(near, scored{name, d})
			}
		}
	}
	slices.SortFunc(near, func(a, b scored) int {
		if c := cmp.Compare(a.distance, b.distance); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})
	out := make([]string, 0, min(len(near), maxSuggestions))
	for i := 0; i < len(near) && i < maxSuggestions; i++ {
		out = append(out, near[i].name)
	}
	return out
}

// editDistance is the Levenshtein distance between a and b, in bytes:
// the fewest insertions, deletions and substitutions that turn one into
// the other. It keeps two rows of the table, so it uses O(len(b))
// memory.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
