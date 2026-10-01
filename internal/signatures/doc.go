// Package signatures is the catalogue of signatures: named definitions
// of something worth noticing in a long file (a traceback, a secret, a
// run of blank lines), built into the binary or written by the user in
// YAML.
//
// Load reads the built-in files (embedded with //go:embed) and the
// user files of the search path (SearchPath: RX_SIGNATURES_PATH, else
// $XDG_CONFIG_HOME/rx/signatures/, else ~/.config/rx/signatures/), and
// returns a Catalogue: the signatures that loaded, with their defaults
// applied, their regexes compiled and their keyword clauses derived;
// the groups; and every problem found, each naming its file, line and
// signature id. Load never fails as a whole: a bad file or signature is
// left out and reported.
//
// Each signature has one kind: a regex (one line), a block (a start
// line and the lines that follow; the simple form is compiled to the
// states form here), a run (consecutive lines that qualify) or a
// measure (Go code with typed parameters). Matching lines is not done
// here: the scan engine takes the compiled signatures and a
// Catalogue.Resolve selection.
//
// The keyword clauses (Signature.Clauses) are part of a signature's
// meaning: a line is tested against the anchor only when every clause
// has a member on the line, or when the line holds the Kelvin sign or
// the long s (a case-insensitive k or s matches them, and no clause
// holds them). They come from a walk of the anchor's regexp/syntax tree
// (keywords.go) or from the signature's own `keywords` field.
package signatures
