package signatures

import "slices"

// Catalogue is the result of one Load: the signatures that loaded, the
// built-ins a user file switched off, the groups, and every problem
// found. It is never changed after Load returns it, so it may be shared
// between goroutines.
type Catalogue struct {
	// signatures are the active signatures (loaded, not disabled), in
	// group order, then load order.
	signatures []*Signature
	byID       map[string]*Signature
	// disabled are the built-ins a user file switched off, in the same
	// order.
	disabled     []*Signature
	disabledByID map[string]*Signature
	groups       []Group
	errors       []LoadError
	// refused holds, for each id that no loaded signature has, the
	// errors that refused its definitions: a selection that names it
	// repeats them.
	refused map[string][]LoadError
	// paths is the search path the catalogue was loaded from; files are
	// the user files found on it, in load order.
	paths, files []string
	// examplesChecked is true when the load ran LoadOptions.CheckExamples.
	examplesChecked bool
}

// Signatures returns the active signatures: every loaded signature that
// no user file disabled, in group order, then load order (built-ins
// first, then user files in search-path order).
func (c *Catalogue) Signatures() []*Signature {
	return slices.Clone(c.signatures)
}

// Groups returns the groups: the built-in ones in their order, then the
// ones user files declared, in load order.
func (c *Catalogue) Groups() []Group {
	return slices.Clone(c.groups)
}

// Errors returns every problem the load found, by file (built-ins
// first, then user files in load order), then line.
func (c *Catalogue) Errors() []LoadError {
	return slices.Clone(c.errors)
}

// DefaultSet returns the active signatures without `default: false`, in
// catalogue order.
func (c *Catalogue) DefaultSet() []*Signature {
	out := make([]*Signature, 0, len(c.signatures))
	for _, s := range c.signatures {
		if s.Default {
			out = append(out, s)
		}
	}
	return out
}

// Lookup returns the active signature with the given id.
func (c *Catalogue) Lookup(id string) (*Signature, bool) {
	s, ok := c.byID[id]
	return s, ok
}

// Disabled returns the built-ins a user file switched off with
// `disable`, each with DisabledBy set, in catalogue order. They are
// listed and shown, never selected or run.
func (c *Catalogue) Disabled() []*Signature {
	return slices.Clone(c.disabled)
}

// LookupDisabled returns the disabled built-in with the given id.
func (c *Catalogue) LookupDisabled(id string) (*Signature, bool) {
	s, ok := c.disabledByID[id]
	return s, ok
}

// Refused returns the errors that refused every definition of id, when
// no loaded signature has that id; nil otherwise.
func (c *Catalogue) Refused(id string) []LoadError {
	return slices.Clone(c.refused[id])
}

// Paths returns the search path the catalogue was loaded from: the
// paths LoadOptions gave, or SearchPath().
func (c *Catalogue) Paths() []string {
	return slices.Clone(c.paths)
}

// Files returns the user files found on the search path, in load order,
// whether they loaded or were refused.
func (c *Catalogue) Files() []string {
	return slices.Clone(c.files)
}

// ExamplesChecked reports whether the load ran the examples check
// (LoadOptions.CheckExamples was set).
func (c *Catalogue) ExamplesChecked() bool {
	return c.examplesChecked
}
