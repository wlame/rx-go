package signatures

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// DefinitionYAML renders the signature as a signature file defines it,
// with every default filled in: the common fields, the declared
// keywords, the kind's fields and the examples. A regex given as a list
// is shown joined, as it runs; a block is shown in the form it was
// written in. The text is a valid signature entry: placed under
// `signatures:`, it loads into the same signature.
func (s *Signature) DefinitionYAML() []byte {
	m := newMapping(yaml.Style(0))
	m.add("id", plain(s.ID))
	m.add("name", text(s.Name))
	m.add("group", plain(s.Group))
	m.add("description", text(s.Description))
	m.add("tags", flowList(s.Tags))
	m.add("level", plain(string(s.Level)))
	m.add("links", flowList(s.Links))
	m.add("metadata", flowMap(s.Metadata))
	m.add("default", boolean(s.Default))
	m.add("redact", boolean(s.Redact))
	if s.KeywordsDeclared {
		var declared []string
		if len(s.Clauses) > 0 {
			declared = s.Clauses[0]
		}
		m.add("keywords", flowList(declared))
	}
	s.addKindFields(m)
	if len(s.Examples.Match)+len(s.Examples.NoMatch) > 0 {
		examples := newMapping(yaml.Style(0))
		examples.add("match", blockList(s.Examples.Match))
		examples.add("no_match", blockList(s.Examples.NoMatch))
		m.add("examples", examples.node)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	// Encoding a node tree built here cannot fail: every node is a
	// scalar, a list or a mapping with text keys.
	_ = enc.Encode(m.node)
	_ = enc.Close()
	return out.Bytes()
}

// addKindFields adds the fields of the signature's kind: the regex
// fields at the top level, or a block, run or measure mapping.
func (s *Signature) addKindFields(m *mapping) {
	switch s.Kind {
	case KindRegex:
		m.add("regex", regexNode(s.Regex.Regex))
		if s.Regex.Not != nil {
			m.add("not", regexNode(s.Regex.Not))
		}
		m.add("capture", integer(s.Regex.Capture))
		if s.Regex.Entropy != nil {
			m.add("entropy", number(*s.Regex.Entropy))
		}
		if s.Regex.Validate != "" {
			m.add("validate", plain(s.Regex.Validate))
		}
	case KindBlock:
		m.add("block", s.blockNode())
	case KindRun:
		run := newMapping(yaml.Style(0))
		switch {
		case s.Run.Blank:
			run.add("blank", boolean(true))
		case s.Run.Same:
			run.add("same", boolean(true))
		default:
			run.add("match", regexNode(s.Run.Match))
		}
		run.add("min", integer(s.Run.Min))
		if s.Run.Not != nil {
			run.add("not", regexNode(s.Run.Not))
		}
		m.add("run", run.node)
	case KindMeasure:
		measure := newMapping(yaml.FlowStyle)
		measure.add("name", plain(s.Measure.Name))
		for _, p := range s.Measure.Params.Fields() {
			measure.add(p.Name, paramNode(p.Value))
		}
		m.add("measure", measure.node)
	}
}

// blockNode renders a block: the simple form as written, a lines block,
// or the states.
func (s *Signature) blockNode() *yaml.Node {
	b := s.Block
	block := newMapping(yaml.Style(0))
	switch {
	case b.Lines > 0:
		block.add("start", regexNode(b.Start))
		block.add("lines", integer(b.Lines))
	case s.simpleBlock != nil:
		block.add("start", regexNode(s.simpleBlock.start))
		if s.simpleBlock.cont != nil {
			block.add("continue", regexNode(s.simpleBlock.cont))
		}
		if s.simpleBlock.end != nil {
			block.add("end", regexNode(s.simpleBlock.end))
		}
	default:
		states := &yaml.Node{Kind: yaml.SequenceNode}
		for _, r := range b.States {
			rule := newMapping(yaml.FlowStyle)
			rule.add("state", plain(r.State))
			rule.add("regex", regexNode(r.Regex))
			if r.End {
				rule.add("end", boolean(true))
			} else {
				rule.add("next", plain(r.Next))
			}
			states.Content = append(states.Content, rule.node)
		}
		block.add("states", states)
	}
	block.add("min_lines", integer(b.MinLines))
	block.add("max_lines", integer(b.MaxLines))
	if b.Not != nil {
		block.add("not", regexNode(b.Not))
	}
	return block.node
}

// mapping builds a YAML mapping node in a fixed key order.
type mapping struct{ node *yaml.Node }

// newMapping starts an empty mapping in the given style (0 for block).
func newMapping(style yaml.Style) *mapping {
	return &mapping{node: &yaml.Node{Kind: yaml.MappingNode, Style: style}}
}

// add appends one key and its value.
func (m *mapping) add(key string, value *yaml.Node) {
	m.node.Content = append(m.node.Content, plain(key), value)
}

// plain is a text scalar; the encoder quotes it when it would read back
// as another type (a number, true, null).
func plain(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// text is free text: a block of several lines as a literal (`|`) block.
func text(s string) *yaml.Node {
	n := plain(s)
	if strings.Contains(s, "\n") && printableLines(s) {
		n.Style = yaml.LiteralStyle
	}
	return n
}

// regexNode is a regex source, single-quoted so its backslashes read as
// written; one with a control character is double-quoted, the only
// style that can escape it.
func regexNode(re *regexp.Regexp) *yaml.Node {
	n := plain(re.String())
	n.Style = yaml.SingleQuotedStyle
	if strings.IndexFunc(n.Value, unicode.IsControl) >= 0 {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

// printableLines reports whether every line of s is free of control
// characters other than the line breaks, which a literal block needs.
func printableLines(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) < 0
}

// boolean is a true/false scalar.
func boolean(b bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(b)}
}

// integer is a whole-number scalar.
func integer(n int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n)}
}

// number is a number scalar in its shortest exact form.
func number(f float64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(f, 'g', -1, 64)}
}

// paramNode is a measure parameter value: an int, a float64, or a
// duration as text.
func paramNode(v any) *yaml.Node {
	switch x := v.(type) {
	case int:
		return integer(x)
	case float64:
		return number(x)
	}
	return plain(paramText(v))
}

// flowList is a list on one line: [a, b].
func flowList(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, item := range items {
		n.Content = append(n.Content, plain(item))
	}
	return n
}

// blockList is a list with one item per line; an item of several lines
// is a literal block.
func blockList(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode}
	for _, item := range items {
		n.Content = append(n.Content, text(item))
	}
	return n
}

// flowMap is a text-to-text map on one line, its keys sorted: {a: b}.
func flowMap(m map[string]string) *yaml.Node {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := newMapping(yaml.FlowStyle)
	for _, k := range keys {
		out.add(k, plain(m[k]))
	}
	return out.node
}
