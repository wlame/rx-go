package clicommand

import (
	"regexp"

	"github.com/wlame/rx-go/internal/signatures"
)

// The JSON shape of `rx signatures --json` and `rx signatures show
// --json`. The names follow the catalogue the HTTP API serves: every
// field of a signature, its kind, source, keyword clauses, line pass
// and marks; the groups with their order and count; the levels; the
// load errors; the search path; and the user files read.

// catalogueJSON is the document `rx signatures --json` prints.
type catalogueJSON struct {
	Signatures []signatureJSON        `json:"signatures"`
	Groups     []groupJSON            `json:"groups"`
	Levels     []signatures.Level     `json:"levels"`
	Errors     []signatures.LoadError `json:"errors"`
	SearchPath []string               `json:"search_path"`
	Files      []string               `json:"files"`
}

// groupJSON is one group: its order and how many active signatures it
// has.
type groupJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Order       int    `json:"order"`
	Count       int    `json:"count"`
	Builtin     bool   `json:"builtin"`
}

// sourceJSON is where a signature was defined: {"builtin": true}, or
// {"file": …, "line": …}.
type sourceJSON struct {
	Builtin bool   `json:"builtin,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// signatureJSON is one signature. Exactly one of regex, block, run and
// measure is not null: the one kind names.
type signatureJSON struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Group            string            `json:"group"`
	Description      string            `json:"description"`
	Tags             []string          `json:"tags"`
	Level            signatures.Level  `json:"level"`
	Links            []string          `json:"links"`
	Metadata         map[string]string `json:"metadata"`
	Default          bool              `json:"default"`
	Redact           bool              `json:"redact"`
	Kind             signatures.Kind   `json:"kind"`
	Keywords         [][]string        `json:"keywords"`
	KeywordsDeclared bool              `json:"keywords_declared"`
	LinePass         bool              `json:"line_pass"`
	Source           sourceJSON        `json:"source"`
	Overrides        bool              `json:"overrides"`
	DisabledBy       *sourceJSON       `json:"disabled_by"`
	Examples         examplesJSON      `json:"examples"`
	Regex            *regexJSON        `json:"regex"`
	Block            *blockJSON        `json:"block"`
	Run              *runJSON          `json:"run"`
	Measure          *measureJSON      `json:"measure"`
}

// examplesJSON are a signature's examples.
type examplesJSON struct {
	Match   []string `json:"match"`
	NoMatch []string `json:"no_match"`
}

// regexJSON is the definition of a regex signature.
type regexJSON struct {
	Regex    string   `json:"regex"`
	Not      *string  `json:"not"`
	Capture  int      `json:"capture"`
	Entropy  *float64 `json:"entropy"`
	Validate *string  `json:"validate"`
}

// blockJSON is the definition of a block signature: the anchor and the
// rules (the simple form compiled), or a lines block.
type blockJSON struct {
	Start    string          `json:"start"`
	States   []stateRuleJSON `json:"states"`
	Lines    int             `json:"lines"`
	MinLines int             `json:"min_lines"`
	MaxLines int             `json:"max_lines"`
	Not      *string         `json:"not"`
}

// stateRuleJSON is one rule of a block.
type stateRuleJSON struct {
	State string  `json:"state"`
	Regex string  `json:"regex"`
	Next  *string `json:"next"`
	End   bool    `json:"end"`
}

// runJSON is the definition of a run signature.
type runJSON struct {
	Match *string `json:"match"`
	Not   *string `json:"not"`
	Blank bool    `json:"blank"`
	Same  bool    `json:"same"`
	Min   int     `json:"min"`
}

// measureJSON is the definition of a measure signature: its name and
// its parameters with defaults applied.
type measureJSON struct {
	Name   string         `json:"name"`
	Params map[string]any `json:"params"`
}

// catalogueView builds the --json document of the listed signatures.
// With --group, only that group is listed.
func catalogueView(cat *signatures.Catalogue, listed []*signatures.Signature, onlyGroup string) catalogueJSON {
	view := catalogueJSON{
		Signatures: make([]signatureJSON, 0, len(listed)),
		Groups:     []groupJSON{},
		Levels:     signatures.Levels(),
		Errors:     cat.Errors(),
		SearchPath: cat.Paths(),
		Files:      cat.Files(),
	}
	for _, s := range listed {
		view.Signatures = append(view.Signatures, signatureViewOf(s))
	}
	for _, g := range cat.Groups() {
		if onlyGroup != "" && g.Name != onlyGroup {
			continue
		}
		count := 0
		for _, s := range cat.Signatures() {
			if s.Group == g.Name {
				count++
			}
		}
		view.Groups = append(view.Groups, groupJSON{Name: g.Name, Description: g.Description, Order: g.Order, Count: count, Builtin: g.Builtin})
	}
	if view.Errors == nil {
		view.Errors = []signatures.LoadError{}
	}
	return view
}

// signatureViewOf builds the JSON of one signature.
func signatureViewOf(s *signatures.Signature) signatureJSON {
	view := signatureJSON{
		ID: s.ID, Name: s.Name, Group: s.Group, Description: s.Description,
		Tags: s.Tags, Level: s.Level, Links: s.Links, Metadata: s.Metadata,
		Default: s.Default, Redact: s.Redact, Kind: s.Kind,
		Keywords: s.Clauses, KeywordsDeclared: s.KeywordsDeclared, LinePass: s.LinePass(),
		Source: sourceView(s.Source), Overrides: s.Overrides,
		Examples: examplesJSON{Match: s.Examples.Match, NoMatch: s.Examples.NoMatch},
	}
	if view.Keywords == nil {
		view.Keywords = [][]string{}
	}
	if s.DisabledBy != nil {
		by := sourceView(*s.DisabledBy)
		view.DisabledBy = &by
	}
	switch s.Kind {
	case signatures.KindRegex:
		view.Regex = &regexJSON{Regex: s.Regex.Regex.String(), Not: regexText(s.Regex.Not), Capture: s.Regex.Capture,
			Entropy: s.Regex.Entropy, Validate: optionalText(s.Regex.Validate)}
	case signatures.KindBlock:
		view.Block = blockView(s.Block)
	case signatures.KindRun:
		view.Run = &runJSON{Match: regexText(s.Run.Match), Not: regexText(s.Run.Not), Blank: s.Run.Blank, Same: s.Run.Same, Min: s.Run.Min}
	case signatures.KindMeasure:
		params := map[string]any{}
		for _, p := range s.Measure.Params.Fields() {
			params[p.Name] = p.Value
		}
		view.Measure = &measureJSON{Name: s.Measure.Name, Params: params}
	}
	return view
}

// blockView builds the JSON of a block.
func blockView(b *signatures.BlockSpec) *blockJSON {
	view := &blockJSON{States: []stateRuleJSON{}, Lines: b.Lines, MinLines: b.MinLines, MaxLines: b.MaxLines, Not: regexText(b.Not)}
	if b.Start != nil {
		view.Start = b.Start.String()
	}
	for _, r := range b.States {
		rule := stateRuleJSON{State: r.State, Regex: r.Regex.String(), End: r.End}
		if !r.End {
			rule.Next = optionalText(r.Next)
		}
		view.States = append(view.States, rule)
	}
	return view
}

// sourceView builds the JSON of a source.
func sourceView(s signatures.Source) sourceJSON {
	if s.Builtin {
		return sourceJSON{Builtin: true}
	}
	return sourceJSON{File: s.File, Line: s.Line}
}

// regexText is a regex's source, or nil for no regex.
func regexText(re *regexp.Regexp) *string {
	if re == nil {
		return nil
	}
	return optionalText(re.String())
}

// optionalText is s, or nil for "".
func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
