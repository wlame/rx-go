package signatures

import (
	"regexp"
	"strconv"
)

// Level says how much a finding of a signature matters. The five
// values are ordered from info (lowest) to critical (highest).
type Level string

// The levels a signature may declare.
const (
	LevelInfo     Level = "info"
	LevelLow      Level = "low"
	LevelMedium   Level = "medium"
	LevelHigh     Level = "high"
	LevelCritical Level = "critical"
)

// Levels returns every level, lowest first. It returns a new slice on
// each call, so a caller may keep or change it.
func Levels() []Level {
	return []Level{LevelInfo, LevelLow, LevelMedium, LevelHigh, LevelCritical}
}

// Kind is the shape of a signature: which of the four definition keys
// (regex, block, run, measure) it holds. Every loaded signature has
// exactly one.
type Kind string

// The four kinds of signature.
const (
	KindRegex   Kind = "regex"
	KindBlock   Kind = "block"
	KindRun     Kind = "run"
	KindMeasure Kind = "measure"
)

// Source says where a signature, a group declaration or a disable entry
// was defined: built into the binary, or a user file and the line in
// it.
//
// A built-in keeps its embedded file name and line too (File is
// "builtin/traceback.yml", for instance), so an error in a built-in
// can name its place; Builtin is what tells the two apart.
type Source struct {
	Builtin bool
	File    string
	Line    int
}

// String is the source as rx prints it: "built-in" for a built-in,
// "file:line" for a user file.
func (s Source) String() string {
	if s.Builtin {
		return "built-in"
	}
	return s.File + ":" + strconv.Itoa(s.Line)
}

// Signature is one loaded signature: a named definition of something
// worth noticing in a long file, with its defaults applied, its regexes
// compiled and its keyword clauses derived.
//
// Exactly one of Regex, Block, Run and Measure is set, the one Kind
// names. A Signature is never changed after Load returns it, so it may
// be shared between goroutines.
type Signature struct {
	ID, Name, Group, Description string
	Tags                         []string
	Level                        Level
	Links                        []string
	Metadata                     map[string]string
	// Default is false for an opt-in signature: it runs only when a
	// selection names it (by id, group, tag or "all").
	Default bool
	// Redact masks the value of every finding in every output and in
	// the cache.
	Redact bool
	// Clauses are the keyword clauses of the anchor, every literal
	// lowered to ASCII lower case. A line is tested against the anchor
	// only when every clause has a member on the line (compared ASCII
	// case-insensitively), or when it holds the Kelvin sign or the long
	// s, which a case-insensitive k or s matches and no clause holds.
	// Nil means no gate: the signature is tested on every line
	// (LinePass).
	Clauses [][]string
	// KeywordsDeclared is true when Clauses come from the signature's
	// own `keywords` field rather than from its anchor regex.
	KeywordsDeclared bool
	Examples         Examples
	Source           Source
	// Overrides is true for a user signature that replaced the built-in
	// of the same id.
	Overrides bool
	// DisabledBy is set on a built-in that a user file switched off
	// with `disable`: the place of that entry. A disabled built-in is
	// listed (Catalogue.Disabled) but never selected or run.
	DisabledBy *Source
	Kind       Kind
	Regex      *RegexSpec // exactly one of these four is set
	Block      *BlockSpec
	Run        *RunSpec
	Measure    *MeasureSpec

	// seq is the signature's position in the load: built-ins first, in
	// embedded file order, then user files in search-path order. The
	// catalogue orders signatures by group, then by seq.
	seq int
	// simpleBlock keeps a block's simple form as it was written, for
	// DefinitionYAML; nil for a block written in the states form and for
	// every other kind.
	simpleBlock *simpleBlockForm
	// examplesLine is where a failing example is reported: the line of
	// the `examples` key, or of the signature without one.
	examplesLine int
}

// Examples are texts a signature must find (Match) and must not find
// (NoMatch). Each text is one or more lines.
type Examples struct {
	Match, NoMatch []string
}

// RegexSpec is the definition of a `regex` signature: a line that the
// anchor matches and `not` does not.
type RegexSpec struct {
	// Regex is the anchor. Not, when set, must not match the line.
	Regex, Not *regexp.Regexp
	// Capture is the group whose span is the finding's value; 0 is the
	// whole match. The first match on the line is the one used.
	Capture int
	// Entropy, when set, is the least Shannon entropy (bits per byte)
	// the value must have.
	Entropy *float64
	// Validate is a check on the value after the regex: "" (none),
	// "luhn", "iban", "ipv4" or "ipv6".
	Validate string
}

// The values `validate` accepts.
const (
	ValidateLuhn = "luhn"
	ValidateIBAN = "iban"
	ValidateIPv4 = "ipv4"
	ValidateIPv6 = "ipv6"
)

// StateRule is one rule of a block's states form. While a block is in
// State, each following line is tried against the rules of that state
// in order; the first rule whose Regex matches joins the line to the
// block and then either closes the block after the line (End) or moves
// it to Next. When no rule of the state matches, the block closes
// before the line.
//
// The rules whose State is "start" are tried on a line no block of the
// signature holds: they open a block.
type StateRule struct {
	State string
	Regex *regexp.Regexp
	Next  string
	End   bool
}

// The state names a block uses. Every block opens in StateStart. A
// block written in the simple form (`start`, `continue`, `end`) is
// compiled to these rules:
//
//	start: the start regex             → StateBody
//	body:  the end regex (if given)    → close after the line
//	body:  the continue regex          → StateBody
//	       (with `end` and no `continue`: every line → StateBody)
//
// so a line after the start joins when `end` matches (and the block
// closes after it), else when `continue` matches; otherwise the block
// closes before the line.
const (
	StateStart = "start"
	StateBody  = "body"
)

// BlockSpec is the definition of a `block` signature: a run of lines
// that starts at a line the anchor matches.
type BlockSpec struct {
	// States are the rules of the block, in order; the simple form is
	// compiled to them (see StateStart). Empty when Lines > 0.
	States []StateRule
	// Start is the anchor: the regexes of the "start" rules joined into
	// one, or the `start` regex of a `lines` block.
	Start *regexp.Regexp
	// Lines > 0 makes the block the start line plus the next Lines
	// lines, whatever they hold.
	Lines int
	// MinLines is the shortest block that is a finding; MaxLines closes
	// a block after that many lines, the start included.
	MinLines, MaxLines int
	// Not, when set, must not match the start line.
	Not *regexp.Regexp
}

// RunSpec is the definition of a `run` signature: at least Min
// consecutive lines that each qualify.
type RunSpec struct {
	// Match is the regex a qualifying line matches (and Not does not);
	// nil when Blank or Same is set.
	Match, Not *regexp.Regexp
	// Blank: a line qualifies when it is empty or holds only spaces and
	// tabs. Same: a line qualifies when it equals the line before it.
	Blank, Same bool
	Min         int
}

// MeasureSpec is the definition of a `measure` signature: Go code with
// parameters.
type MeasureSpec struct {
	// Name is one of "long-line", "binary", "entropy", "json-block",
	// "unprefixed", "base64-run", "density", "time-gap",
	// "time-backwards".
	Name string
	// Params holds the measure's parameters with their defaults applied
	// and their ranges checked: one concrete type per name
	// (LongLineParams for "long-line", and so on).
	Params MeasureParams
}

// Group is a bucket of signatures, for listing, color and selection.
// The built-in groups come first, in the order of the embedded
// groups.yml; groups declared by user files follow in load order.
type Group struct {
	Name, Description string
	// Order is the group's 1-based position: built-ins first.
	Order   int
	Builtin bool
}

// LoadError is one problem found while loading signatures: the file
// and line it is at, the id of the signature it refused (empty for a
// problem of the whole file, a group or a disable entry), and what is
// wrong.
type LoadError struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	ID      string `json:"id"`
	Message string `json:"message"`
}

// Error formats the problem as "file:line: id: message", leaving out
// the id when there is none.
func (e LoadError) Error() string {
	place := e.File + ":" + strconv.Itoa(e.Line) + ": "
	if e.ID == "" {
		return place + e.Message
	}
	return place + e.ID + ": " + e.Message
}

// Anchor returns the regex that opens a finding of the signature: the
// regex of a `regex` signature, the start of a block, the match of a
// run. It returns nil for a measure and for a blank or same run, which
// have no anchor.
func (s *Signature) Anchor() *regexp.Regexp {
	switch s.Kind {
	case KindRegex:
		return s.Regex.Regex
	case KindBlock:
		return s.Block.Start
	case KindRun:
		return s.Run.Match
	}
	return nil
}

// LinePass reports whether the signature has no keyword gate: it is
// tested on every line of a scan. That is so for a signature with an
// anchor from which no keyword clause could be derived (and none was
// declared), and always for a measure and a blank or same run.
func (s *Signature) LinePass() bool {
	return len(s.Clauses) == 0
}
