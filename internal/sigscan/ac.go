package sigscan

import (
	"fmt"
	"slices"
)

// maxDFACells bounds the transition table of the keyword automaton:
// states times byte classes, 4 bytes each, so at most 256 MiB. The
// keywords come from the loaded signatures, whose own limits (1,000
// user signatures, 4 KiB per regex, 32 clauses of at most 64 literals
// per anchor) keep any real catalogue far below it; a selection past it
// is refused with an error rather than allocated.
const maxDFACells = 1 << 26

// keywordDFA is an Aho-Corasick automaton over a set of keywords,
// compiled to a full DFA: from every state, every input byte leads to
// exactly one next state, so a scan does one table lookup per byte and
// never follows a failure link.
//
// Two tricks keep it small and fast:
//
//   - Byte classes. Only the bytes that occur in some keyword need
//     their own column; every other byte shares class 0, which always
//     leads back towards the root. The table is states × classes, not
//     states × 256.
//   - ASCII case folding in the class table. Keywords are lowered, and
//     an upper-case ASCII letter maps to the class of its lower case,
//     so a scan matches without case and never copies or lowers the
//     line. Every other byte (UTF-8 included) is matched exactly.
//
// Every state knows all the keywords that end there, those that are a
// suffix of a longer one included ("err" inside "error"), so the scan
// reports overlapping matches: every keyword present on the line.
//
// A keywordDFA is never changed after it is built, so many goroutines
// may scan with one.
type keywordDFA struct {
	// class maps a byte to its column.
	class [256]uint8
	// nclass is the number of columns.
	nclass int
	// trans is the table, row by row. A row starts at state*nclass; the
	// entry for a byte is nextRow<<1 | 1 when the next state has an
	// output, nextRow<<1 when it has none. Keeping the next row (not the
	// next state number) saves a multiplication per byte.
	trans []uint32
	// outStart and outIDs hold each state's output: the ids attached to
	// every keyword that ends in the state, sorted and without
	// duplicates, at outIDs[outStart[state]:outStart[state+1]].
	outStart []int32
	outIDs   []int32
}

// buildDFA compiles keywords (lowered to ASCII lower case) into an
// automaton. ids[i] are the ids reported when keyword i is present (the
// keyword clauses it belongs to). It fails when the table would be
// larger than maxDFACells.
func buildDFA(keywords []string, ids [][]int32) (*keywordDFA, error) {
	// The trie has at most one state per keyword byte (plus the root),
	// so this bound keeps every state number far inside an int32 before
	// the table's own bound is checked.
	total := 0
	for _, k := range keywords {
		total += len(k)
	}
	if total > maxDFACells {
		return nil, fmt.Errorf("the keywords of the selected signatures hold %d bytes, more than %d: select fewer signatures", total, maxDFACells)
	}
	d := &keywordDFA{}
	classOf := assignClasses(keywords, &d.class)
	d.nclass = classOf.count

	nodes := buildTrie(keywords, ids, classOf)
	if cells := len(nodes) * d.nclass; cells > maxDFACells {
		return nil, fmt.Errorf("the keywords of the selected signatures need an automaton of %d states × %d byte classes, more than %d cells: select fewer signatures",
			len(nodes), d.nclass, maxDFACells)
	}
	next := linkTrie(nodes, d.nclass)
	if err := d.fillTables(nodes, next); err != nil {
		return nil, err
	}
	return d, nil
}

// classMap numbers the bytes that occur in the keywords.
type classMap struct {
	of    [256]int
	count int
}

// assignClasses gives every byte that occurs in a keyword a class from
// 1 up, and writes the class table: an upper-case ASCII letter gets the
// class of its lower case, and a byte in no keyword class 0.
func assignClasses(keywords []string, table *[256]uint8) classMap {
	var m classMap
	m.count = 1
	for _, k := range keywords {
		for i := 0; i < len(k); i++ {
			c := asciiLower(k[i])
			if m.of[c] == 0 {
				m.of[c] = m.count
				m.count++
			}
		}
	}
	for b := 0; b < 256; b++ {
		// #nosec G115 -- at most 231 classes: the 256 byte values less
		// the 26 upper-case letters, which share their lower case's
		// class, plus class 0.
		table[b] = uint8(m.of[asciiLower(byte(b))])
	}
	return m
}

// asciiLower lowers an ASCII upper-case letter and leaves every other
// byte as it is.
func asciiLower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// trieNode is one state of the keyword trie while it is built.
type trieNode struct {
	children map[int]int32 // class → child state
	fail     int32         // the longest proper suffix that is also a state
	out      []int32       // ids of the keywords ending here (and, after linking, along the fail chain)
}

// buildTrie puts every keyword into a trie of byte classes and attaches
// its ids to the state where it ends.
func buildTrie(keywords []string, ids [][]int32, classOf classMap) []trieNode {
	nodes := []trieNode{{children: map[int]int32{}}}
	for ki, k := range keywords {
		s := int32(0)
		for i := 0; i < len(k); i++ {
			c := classOf.of[asciiLower(k[i])]
			child, ok := nodes[s].children[c]
			if !ok {
				nodes = append(nodes, trieNode{children: map[int]int32{}})
				child = int32(len(nodes) - 1) // #nosec G115 -- at most maxDFACells+1 states (buildDFA)
				nodes[s].children[c] = child
			}
			s = child
		}
		nodes[s].out = append(nodes[s].out, ids[ki]...)
	}
	return nodes
}

// linkTrie computes the failure links breadth first and returns the
// full transition table (next[state*nclass+class] = state). Each state's
// output takes in the output of its failure state, so a keyword that is
// a suffix of another is reported where the longer one ends too.
func linkTrie(nodes []trieNode, nclass int) []int32 {
	next := make([]int32, len(nodes)*nclass)
	queue := make([]int32, 0, len(nodes))
	for c := 0; c < nclass; c++ {
		if child, ok := nodes[0].children[c]; ok {
			next[c] = child
			nodes[child].fail = 0
			queue = append(queue, child)
		}
	}
	// A breadth-first order visits a state after its failure state
	// (which is shallower), so that state's output and row are final.
	for head := 0; head < len(queue); head++ {
		s := queue[head]
		f := nodes[s].fail
		nodes[s].out = append(nodes[s].out, nodes[f].out...)
		for c := 0; c < nclass; c++ {
			if child, ok := nodes[s].children[c]; ok {
				nodes[child].fail = next[int(f)*nclass+c]
				next[int(s)*nclass+c] = child
				queue = append(queue, child)
			} else {
				next[int(s)*nclass+c] = next[int(f)*nclass+c]
			}
		}
	}
	return next
}

// fillTables turns the linked trie into the scan tables. It fails when
// the outputs of all states together hold more than maxDFACells ids.
func (d *keywordDFA) fillTables(nodes []trieNode, next []int32) error {
	d.outStart = make([]int32, len(nodes)+1)
	for s := range nodes {
		out := nodes[s].out
		slices.Sort(out)
		out = slices.Compact(out)
		d.outIDs = append(d.outIDs, out...)
		if len(d.outIDs) > maxDFACells {
			return fmt.Errorf("the keywords of the selected signatures overlap in more than %d ways: select fewer signatures", maxDFACells)
		}
		d.outStart[s+1] = int32(len(d.outIDs)) // #nosec G115 -- at most maxDFACells, checked above
	}
	d.trans = make([]uint32, len(next))
	for i, t := range next {
		// #nosec G115 -- a row starts below maxDFACells (2^26), so the
		// row shifted left by one fits in 32 bits.
		v := uint32(int(t)*d.nclass) << 1
		if d.outStart[t+1] > d.outStart[t] {
			v |= 1
		}
		d.trans[i] = v
	}
	return nil
}

// outputOfRow returns the ids of the keywords that end in the state
// whose row starts at row.
func (d *keywordDFA) outputOfRow(row uint32) []int32 {
	s := int(row) / d.nclass
	return d.outIDs[d.outStart[s]:d.outStart[s+1]]
}
