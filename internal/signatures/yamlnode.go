package signatures

import (
	"fmt"
	"math"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// A signature file is decoded into a tree of yaml.Node values rather
// than straight into Go structs, for three reasons:
//
//   - every node carries the line it was written on, so each error and
//     each signature's source can name its line;
//   - the walk below sees every key, so an unknown field or a key given
//     twice is reported (yaml.v3 keeps both copies of a repeated key in
//     a node tree and has no strict mode for one);
//   - aliases stay aliases (yaml.AliasNode, pointing at the anchored
//     node) until the walk follows them, so the loader can count what
//     following them would cost before it decodes anything.

// fieldError is a problem with one place in a file: its line and what
// is wrong. wholeFile marks a limit, which refuses the whole file
// rather than one signature.
type fieldError struct {
	line      int
	msg       string
	wholeFile bool
	// id is the signature a wholeFile problem was found in, when known.
	id string
}

// newFieldError formats a problem at the line of node n.
func newFieldError(n *yaml.Node, format string, args ...any) *fieldError {
	return &fieldError{line: n.Line, msg: fmt.Sprintf(format, args...)}
}

// aliasExpansionFits reports whether following every alias of the
// document adds at most maxAliasNodes nodes.
//
// SECURITY: a few lines of anchors and aliases can stand for billions
// of nodes ("billion laughs"), and an alias can name a node that holds
// it (`x: &x [1, *x]`), which never ends. The walk counts each node an
// alias would copy and stops at the budget, so its own work is the size
// of the file plus the budget, however the aliases nest.
func aliasExpansionFits(doc *yaml.Node) bool {
	budget := maxAliasNodes
	return countAliasCopies(doc, false, &budget)
}

// countAliasCopies walks n, counting the nodes reached through an alias
// (copied) against the budget. It returns false once the budget is
// spent.
func countAliasCopies(n *yaml.Node, copied bool, budget *int) bool {
	if copied {
		*budget--
		if *budget < 0 {
			return false
		}
	}
	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return true
		}
		return countAliasCopies(n.Alias, true, budget)
	}
	for _, child := range n.Content {
		if !countAliasCopies(child, copied, budget) {
			return false
		}
	}
	return true
}

// resolve follows an alias to the node it names. Once
// aliasExpansionFits has passed, following aliases is bounded, so the
// loop ends.
func resolve(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// pair is one key and its value in a YAML mapping.
type pair struct {
	key   string
	keyAt *yaml.Node
	value *yaml.Node
}

// mappingPairs returns the pairs of a mapping node in order. A key
// given twice, a merge key (<<) and a key that is not a plain scalar
// are errors: the first refuses the meaning of the mapping, the second
// is not supported (repeat the fields instead), and the third cannot
// name a field.
func mappingPairs(n *yaml.Node, what string) ([]pair, *fieldError) {
	m := resolve(n)
	if m.Kind != yaml.MappingNode {
		return nil, newFieldError(n, "%s must be a mapping of fields", what)
	}
	pairs := make([]pair, 0, len(m.Content)/2)
	seen := map[string]bool{}
	// Go note: Content of a mapping node alternates key and value, so
	// the loop steps by two.
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, value := resolve(m.Content[i]), m.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return nil, newFieldError(m.Content[i], "%s has a key that is not a name", what)
		}
		if key.Tag == "!!merge" || key.Value == "<<" {
			return nil, newFieldError(m.Content[i], "merge keys (<<) are not supported: write the fields out")
		}
		if seen[key.Value] {
			return nil, newFieldError(m.Content[i], "%q is given twice", key.Value)
		}
		seen[key.Value] = true
		pairs = append(pairs, pair{key: key.Value, keyAt: m.Content[i], value: value})
	}
	return pairs, nil
}

// sequenceItems returns the items of a sequence node.
func sequenceItems(n *yaml.Node, what string) ([]*yaml.Node, *fieldError) {
	s := resolve(n)
	if s.Kind != yaml.SequenceNode {
		return nil, newFieldError(n, "%s must be a list", what)
	}
	return s.Content, nil
}

// isNull reports whether n is a YAML null (`~`, `null` or nothing).
func isNull(n *yaml.Node) bool {
	r := resolve(n)
	return r.Kind == yaml.ScalarNode && r.ShortTag() == "!!null"
}

// textValue reads a scalar as text. Any scalar but null is accepted, so
// `name: 404` is the text "404".
func textValue(n *yaml.Node, field string) (string, *fieldError) {
	r := resolve(n)
	if r.Kind != yaml.ScalarNode || r.ShortTag() == "!!null" {
		return "", newFieldError(n, "%s must be text", field)
	}
	return r.Value, nil
}

// nonEmptyText reads a scalar as text that must not be empty.
func nonEmptyText(n *yaml.Node, field string) (string, *fieldError) {
	text, err := textValue(n, field)
	if err == nil && text == "" {
		err = newFieldError(n, "%s is empty", field)
	}
	return text, err
}

// boolValue reads `true` or `false`.
func boolValue(n *yaml.Node, field string) (bool, *fieldError) {
	r := resolve(n)
	var b bool
	if r.Kind != yaml.ScalarNode || r.ShortTag() != "!!bool" || r.Decode(&b) != nil {
		return false, newFieldError(n, "%s must be true or false", field)
	}
	return b, nil
}

// intValue reads a whole number within [lo, hi].
func intValue(n *yaml.Node, field string, lo, hi int) (int, *fieldError) {
	r := resolve(n)
	var v int64
	if r.Kind != yaml.ScalarNode || r.ShortTag() != "!!int" || r.Decode(&v) != nil {
		return 0, newFieldError(n, "%s must be a whole number", field)
	}
	if v < int64(lo) || v > int64(hi) {
		return 0, newFieldError(n, "%s: %d is out of range %d to %d", field, v, lo, hi)
	}
	return int(v), nil
}

// floatValue reads a number, whole or not, within [lo, hi].
func floatValue(n *yaml.Node, field string, lo, hi float64) (float64, *fieldError) {
	r := resolve(n)
	var v float64
	tag := r.ShortTag()
	// math.IsNaN: YAML's .nan is a float that every range check lets
	// through, since no comparison with NaN is true.
	if r.Kind != yaml.ScalarNode || (tag != "!!int" && tag != "!!float") || r.Decode(&v) != nil || math.IsNaN(v) {
		return 0, newFieldError(n, "%s must be a number", field)
	}
	if v < lo || v > hi {
		return 0, newFieldError(n, "%s: %s is out of range %s to %s", field,
			strconv.FormatFloat(v, 'g', -1, 64), strconv.FormatFloat(lo, 'g', -1, 64), strconv.FormatFloat(hi, 'g', -1, 64))
	}
	return v, nil
}

// textList reads a list of text values; an empty list is an empty,
// non-nil slice.
func textList(n *yaml.Node, field string) ([]string, *fieldError) {
	items, err := sequenceItems(n, field)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, err := textValue(item, field)
		if err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
}

// textMap reads a mapping of text to text.
func textMap(n *yaml.Node, field string) (map[string]string, *fieldError) {
	pairs, err := mappingPairs(n, field)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		value, err := textValue(p.value, field+"."+p.key)
		if err != nil {
			return nil, err
		}
		out[p.key] = value
	}
	return out, nil
}
