package sigscan

import (
	"bytes"

	"github.com/wlame/rx-go/internal/signatures"
)

// The json-block measure: a pretty-printed JSON object or array in a
// log. It opens on a line that is only `{` or `[` (spaces and tabs
// around it allowed) and closes on the line where the brackets balance
// again, when that line ends with the matching closer at the opener's
// indent. Brackets that would leave the count below zero, or a balance
// on any other line, abandon the block: it is not a finding, and the
// lines it took are not tried as new openers.
//
// It runs as a block (block.go), so a block that crosses a chunk edge
// is finished by the merge's replay, the lines of one block are never
// shared with another, max_lines closes it (a finding of max_lines
// lines, trailing blank lines dropped) and the end of the text closes
// it. Only its opening and its rules are its own.
//
// The bracket count is string-aware, one line at a time: a bracket in a
// JSON string ("…") does not count, and a backslash in a string escapes
// the next character. A string is not carried over to the next line:
// a JSON string holds no raw line break.

// compileJSONBlock returns the block program of a json-block measure.
func compileJSONBlock(p signatures.JSONBlockParams) *blockProgram {
	return &blockProgram{json: true, minLines: 1, maxLines: p.MaxLines}
}

// jsonOpener reports whether a line view opens a JSON block, and
// returns the closer the block waits for and the opener's indent.
func jsonOpener(view []byte) (closer byte, indent int, ok bool) {
	trimmed := bytes.TrimSpace(view)
	if len(trimmed) != 1 {
		return 0, 0, false
	}
	switch trimmed[0] {
	case '{':
		return '}', indentOf(view), true
	case '[':
		return ']', indentOf(view), true
	}
	return 0, 0, false
}

// indentOf counts the spaces and tabs a line view starts with.
func indentOf(view []byte) int {
	n := 0
	for n < len(view) && (view[n] == ' ' || view[n] == '\t') {
		n++
	}
	return n
}

// jsonBrackets counts the opening (`{`, `[`) and closing (`}`, `]`)
// brackets of a line view outside JSON strings.
func jsonBrackets(view []byte) (opens, closes int) {
	inString, escaped := false, false
	for _, c := range view {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			opens++
		case '}', ']':
			closes++
		}
	}
	return opens, closes
}

// tryOpenJSON opens a JSON block on line l when the line is an opener.
func (b *blockWalker) tryOpenJSON(l *line, idx int64, out sink) {
	closer, indent, ok := jsonOpener(l.view)
	if !ok {
		return
	}
	st := blockState{startIdx: idx, startOff: l.start, lines: 1, lastIdx: idx, lastEnd: l.next, vs: -1, ve: -1,
		depth: 1, indent: indent, closer: closer, tm: b.times.at(l.start)}
	if out.mayStore() {
		st.hasEx = true
		st.excerpt, st.masked = b.ex.build(b.st.excerpt[:0], l.view)
	}
	b.open, b.st, b.joinedAt = true, st, idx
}

// feedJSON offers line l to the open JSON block. The block always takes
// the line: it joins, closes the block with its balance, or abandons it.
func (b *blockWalker) feedJSON(l *line, idx int64, out sink) bool {
	opens, closes := jsonBrackets(l.view)
	b.st.depth += opens - closes
	switch {
	case b.st.depth < 0:
		b.abandon(idx)
	case b.st.depth == 0:
		trimmed := bytes.TrimSpace(l.view)
		if len(trimmed) == 0 || trimmed[len(trimmed)-1] != b.st.closer || indentOf(l.view) != b.st.indent {
			b.abandon(idx)
			return true
		}
		b.join(l, idx)
		b.close(out)
	default:
		b.join(l, idx)
		if b.st.lines >= b.prog.maxLines {
			b.close(out)
		}
	}
	return true
}

// abandon drops the open block without a finding. The line that
// abandoned it was taken, so it is not tried as a new opener.
func (b *blockWalker) abandon(idx int64) {
	b.open = false
	b.joinedAt = idx
}
