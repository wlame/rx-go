package webapi

import (
	"regexp"
	"slices"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// SignaturesView builds the catalogue document of GET /v1/signatures and
// `rx signatures --json` from a loaded catalogue: the listed signatures
// (ListedSignatures), the groups with their counts (only onlyGroup when
// it is set), the levels, the load errors, the search path and the user
// files. Every list is present, empty rather than null.
func SignaturesView(cat *signatures.Catalogue, listed []*signatures.Signature, onlyGroup string) rxtypes.SignaturesResponse {
	view := rxtypes.SignaturesResponse{
		Signatures: make([]rxtypes.SignatureInfo, 0, len(listed)),
		Groups:     []rxtypes.GroupInfo{},
		Levels:     []string{},
		Errors:     []rxtypes.LoadErrorInfo{},
		SearchPath: nonNil(cat.Paths()),
		Files:      nonNil(cat.Files()),
	}
	for _, level := range signatures.Levels() {
		view.Levels = append(view.Levels, string(level))
	}
	for _, s := range listed {
		view.Signatures = append(view.Signatures, SignatureView(s))
	}
	active := cat.Signatures()
	for _, g := range cat.Groups() {
		if onlyGroup != "" && g.Name != onlyGroup {
			continue
		}
		count := 0
		for _, s := range active {
			if s.Group == g.Name {
				count++
			}
		}
		view.Groups = append(view.Groups, rxtypes.GroupInfo{
			Name: g.Name, Description: g.Description, Order: g.Order, Count: count, Builtin: g.Builtin,
		})
	}
	for _, e := range cat.Errors() {
		view.Errors = append(view.Errors, rxtypes.LoadErrorInfo{File: e.File, Line: e.Line, ID: e.ID, Message: e.Message})
	}
	return view
}

// ListedSignatures are the active and the disabled signatures of cat
// that the filters keep (a group, a tag; "" keeps every one), in
// catalogue order: group by group, the disabled ones of a group after
// its active ones.
func ListedSignatures(cat *signatures.Catalogue, group, tag string) []*signatures.Signature {
	var out []*signatures.Signature
	lists := [][]*signatures.Signature{cat.Signatures(), cat.Disabled()}
	for _, g := range cat.Groups() {
		if group != "" && g.Name != group {
			continue
		}
		for _, list := range lists {
			for _, s := range list {
				if s.Group == g.Name && (tag == "" || slices.Contains(s.Tags, tag)) {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// SignatureView builds the document of one signature: every field of its
// definition, its kind's definition (the other kinds' members null), its
// source and its keyword clauses.
func SignatureView(s *signatures.Signature) rxtypes.SignatureInfo {
	view := rxtypes.SignatureInfo{
		ID: s.ID, Name: s.Name, Group: s.Group, Description: s.Description,
		Tags: nonNil(s.Tags), Level: string(s.Level), Links: nonNil(s.Links), Metadata: s.Metadata,
		Default: s.Default, Redact: s.Redact, Kind: string(s.Kind),
		Keywords: s.Clauses, KeywordsDeclared: s.KeywordsDeclared, LinePass: s.LinePass(),
		Source: sourceView(s.Source), Overrides: s.Overrides,
		Examples: rxtypes.SignatureExamples{Match: nonNil(s.Examples.Match), NoMatch: nonNil(s.Examples.NoMatch)},
	}
	if view.Metadata == nil {
		view.Metadata = map[string]string{}
	}
	if view.Keywords == nil {
		view.Keywords = [][]string{}
	}
	if s.DisabledBy != nil {
		view.DisabledBy = &rxtypes.DisabledBy{File: s.DisabledBy.File, Line: s.DisabledBy.Line}
	}
	switch s.Kind {
	case signatures.KindRegex:
		view.Regex = &rxtypes.RegexDefinition{Regex: s.Regex.Regex.String(), Not: regexText(s.Regex.Not),
			Capture: s.Regex.Capture, Entropy: s.Regex.Entropy, Validate: optionalText(s.Regex.Validate)}
	case signatures.KindBlock:
		view.Block = blockView(s.Block)
	case signatures.KindRun:
		view.Run = &rxtypes.RunDefinition{Match: regexText(s.Run.Match), Not: regexText(s.Run.Not),
			Blank: s.Run.Blank, Same: s.Run.Same, Min: s.Run.Min}
	case signatures.KindMeasure:
		params := map[string]any{}
		for _, p := range s.Measure.Params.Fields() {
			params[p.Name] = p.Value
		}
		view.Measure = &rxtypes.MeasureDefinition{Name: s.Measure.Name, Params: params}
	}
	return view
}

// blockView builds the definition of a block.
func blockView(b *signatures.BlockSpec) *rxtypes.BlockDefinition {
	view := &rxtypes.BlockDefinition{States: []rxtypes.StateRuleInfo{}, Lines: b.Lines, MinLines: b.MinLines,
		MaxLines: b.MaxLines, Not: regexText(b.Not)}
	if b.Start != nil {
		view.Start = b.Start.String()
	}
	for _, r := range b.States {
		rule := rxtypes.StateRuleInfo{State: r.State, Regex: r.Regex.String(), End: r.End}
		if !r.End {
			rule.Next = optionalText(r.Next)
		}
		view.States = append(view.States, rule)
	}
	return view
}

// sourceView builds where a signature was defined: {"builtin": true},
// or the file and the line.
func sourceView(s signatures.Source) rxtypes.SignatureSource {
	if s.Builtin {
		return rxtypes.SignatureSource{Builtin: true}
	}
	return rxtypes.SignatureSource{File: s.File, Line: s.Line}
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

// nonNil is items, or an empty slice for nil, so the list is [] on the
// wire rather than null.
func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
