package rxtypes

// SignaturesResponse is the signature catalogue: the body of
// GET /v1/signatures and the document `rx signatures --json` prints.
//
// Signatures lists the active signatures and the built-ins a user file
// switched off (DisabledBy set), group by group in group order; a
// disabled built-in is listed, never selected or run.
type SignaturesResponse struct {
	Signatures []SignatureInfo `json:"signatures" nullable:"false" doc:"Every loaded signature, group by group in group order, then in load order; the built-ins a user file disabled are listed too, with disabled_by set."`
	Groups     []GroupInfo     `json:"groups" nullable:"false" doc:"The groups in their order: the built-in ones, then those user files declare."`
	Levels     []string        `json:"levels" nullable:"false" doc:"The levels from the lowest to the highest: info, low, medium, high, critical."`
	Errors     []LoadErrorInfo `json:"errors" nullable:"false" doc:"Every problem the load found, by file, then line. A refused signature is not in signatures; a selection that names it is refused with its errors."`
	SearchPath []string        `json:"search_path" nullable:"false" doc:"Where the user signature files were looked for: RX_SIGNATURES_PATH, or the configuration directory."`
	Files      []string        `json:"files" nullable:"false" doc:"The user signature files found on the search path, in load order, whether they loaded or were refused."`
}

// SignatureInfo is one signature as it is loaded: every field of its
// definition with the defaults applied, its kind and definition, where
// it was defined, its keyword clauses and whether it replaced a
// built-in.
type SignatureInfo struct {
	ID               string             `json:"id" doc:"Unique kebab-case id."`
	Name             string             `json:"name" doc:"Short title for lists."`
	Group            string             `json:"group" doc:"The signature's one group."`
	Description      string             `json:"description" doc:"Free text, possibly several lines."`
	Tags             []string           `json:"tags" nullable:"false" doc:"Extra words for selection."`
	Level            string             `json:"level" enum:"info,low,medium,high,critical" doc:"How much a finding matters."`
	Links            []string           `json:"links" nullable:"false" doc:"URLs about the signature, shown, never fetched."`
	Metadata         map[string]string  `json:"metadata" doc:"Free key-value pairs, shown, never interpreted."`
	Default          bool               `json:"default" doc:"In the default set: false for an opt-in signature, which runs only when a selection names it."`
	Redact           bool               `json:"redact" doc:"The values it finds are masked in every output and in the cache."`
	Kind             string             `json:"kind" enum:"regex,block,run,measure" doc:"Which of regex, block, run and measure defines it; the member of that name holds the definition."`
	Keywords         [][]string         `json:"keywords" nullable:"false" doc:"The keyword clauses of its anchor, lowered: a line is tested only when every clause has a member on it. Empty when the signature runs on every line (line_pass)."`
	KeywordsDeclared bool               `json:"keywords_declared" doc:"The clauses come from the signature's own keywords field rather than from its anchor."`
	LinePass         bool               `json:"line_pass" doc:"No keyword gate: the signature is tested on every line of a scan."`
	Source           SignatureSource    `json:"source" doc:"Where the signature was defined: {\"builtin\": true}, or the user file and the line."`
	Overrides        bool               `json:"overrides" doc:"A user signature that replaced the built-in of the same id."`
	DisabledBy       *DisabledBy        `json:"disabled_by" doc:"For a built-in a user file switched off: where. Null otherwise."`
	Examples         SignatureExamples  `json:"examples" doc:"Texts the signature must find and must not find."`
	Regex            *RegexDefinition   `json:"regex" doc:"The definition of a regex signature; null for another kind."`
	Block            *BlockDefinition   `json:"block" doc:"The definition of a block signature; null for another kind."`
	Run              *RunDefinition     `json:"run" doc:"The definition of a run signature; null for another kind."`
	Measure          *MeasureDefinition `json:"measure" doc:"The definition of a measure signature; null for another kind."`
}

// SignatureSource says where a signature was defined: {"builtin":
// true}, or {"file": …, "line": …}.
type SignatureSource struct {
	Builtin bool   `json:"builtin,omitempty" doc:"True for a built-in signature."`
	File    string `json:"file,omitempty" doc:"The user file that defines the signature."`
	Line    int    `json:"line,omitempty" doc:"The line of the definition in that file."`
}

// DisabledBy is where a user file switched a built-in off: the file and
// the line of its disable entry.
type DisabledBy struct {
	_    struct{} `nullable:"true"`
	File string   `json:"file,omitempty" doc:"The user file whose disable entry switched the built-in off."`
	Line int      `json:"line,omitempty" doc:"The line of that entry."`
}

// SignatureExamples are a signature's examples: texts it must find and
// must not find, each one or more lines.
type SignatureExamples struct {
	Match   []string `json:"match" nullable:"false" doc:"Texts the signature must find."`
	NoMatch []string `json:"no_match" nullable:"false" doc:"Texts the signature must not find."`
}

// RegexDefinition is the definition of a regex signature.
type RegexDefinition struct {
	_        struct{} `nullable:"true"`
	Regex    string   `json:"regex" doc:"The anchor, a Go RE2 regex matched against each line."`
	Not      *string  `json:"not" doc:"A regex the line must not match; null for none."`
	Capture  int      `json:"capture" doc:"Which group of the anchor is the finding's value; 0 is the whole match."`
	Entropy  *float64 `json:"entropy" doc:"The least Shannon entropy (bits per byte) of the value; null for none."`
	Validate *string  `json:"validate" doc:"A check of the value: luhn, iban, ipv4 or ipv6; null for none."`
}

// BlockDefinition is the definition of a block signature: its anchor
// and its rules (the simple form compiled), or a block of a fixed
// number of lines.
type BlockDefinition struct {
	_        struct{}        `nullable:"true"`
	Start    string          `json:"start" doc:"The anchor: the regex that opens a block."`
	States   []StateRuleInfo `json:"states" nullable:"false" doc:"The rules that join the following lines to the block; empty for a lines block."`
	Lines    int             `json:"lines" doc:"For a lines block, the lines after the anchor it holds; 0 otherwise."`
	MinLines int             `json:"min_lines" doc:"The fewest lines a block must have to be a finding."`
	MaxLines int             `json:"max_lines" doc:"The most lines a block holds."`
	Not      *string         `json:"not" doc:"A regex the anchor line must not match; null for none."`
}

// StateRuleInfo is one rule of a block: in state State, a line that
// matches Regex joins the block and moves it to Next, or ends it.
type StateRuleInfo struct {
	State string  `json:"state" doc:"The state the rule applies in."`
	Regex string  `json:"regex" doc:"The regex a line must match."`
	Next  *string `json:"next" doc:"The state after a matching line; null when the rule ends the block."`
	End   bool    `json:"end" doc:"A matching line is the block's last."`
}

// RunDefinition is the definition of a run signature.
type RunDefinition struct {
	_     struct{} `nullable:"true"`
	Match *string  `json:"match" doc:"The regex every line of a run matches; null for a blank or same run."`
	Not   *string  `json:"not" doc:"A regex a line of the run must not match; null for none."`
	Blank bool     `json:"blank" doc:"A run of blank lines."`
	Same  bool     `json:"same" doc:"A run of identical lines."`
	Min   int      `json:"min" doc:"The fewest lines a run must have to be a finding."`
}

// MeasureDefinition is the definition of a measure signature: the
// measure's name and its parameters with defaults applied.
type MeasureDefinition struct {
	_      struct{}       `nullable:"true"`
	Name   string         `json:"name" doc:"The measure: long-line, binary, entropy, json-block, unprefixed, base64-run, density, time-gap or time-backwards."`
	Params map[string]any `json:"params" doc:"The measure's parameters, defaults applied."`
}

// GroupInfo is one group of the catalogue.
type GroupInfo struct {
	Name        string `json:"name" doc:"The group's kebab-case name."`
	Description string `json:"description" doc:"What the group holds."`
	Order       int    `json:"order" doc:"The group's 1-based place: the built-in groups first. The order sets a group's color."`
	Count       int    `json:"count" doc:"How many active signatures the group has."`
	Builtin     bool   `json:"builtin" doc:"A built-in group; false for one a user file declares."`
}

// LoadErrorInfo is one problem found while loading signatures.
type LoadErrorInfo struct {
	File    string `json:"file" doc:"The file the problem is in (a built-in's embedded name, or a user file)."`
	Line    int    `json:"line" doc:"The line of the problem."`
	ID      string `json:"id" doc:"The id of the signature it refused; empty for a problem of the whole file, a group or a disable entry."`
	Message string `json:"message" doc:"What is wrong."`
}
