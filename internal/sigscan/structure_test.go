package sigscan

import (
	"strings"
	"testing"
)

// repeatLines is n lines of size bytes each (without the line ending).
func repeatLines(n, size int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Repeat("x", size)
	}
	return out
}

// around returns before lines, then middle, then after lines.
func around(before []string, middle string, after []string) []string {
	return append(append(append([]string{}, before...), middle), after...)
}

// A 5,000-byte line among 80-byte lines is long; the same line among
// 4,000-byte lines is not; a line under min_bytes never is, however
// much longer than its neighbors.
func TestScan_LongLineMeasure(t *testing.T) {
	sigs := loadSigs(t, sigFile(measureSig("long", "long-line", "")), "long")
	cases := []struct {
		name string
		text string
		want []lineSpan
	}{
		{"among short lines", lines(around(repeatLines(30, 80), strings.Repeat("y", 5000), repeatLines(30, 80))...), []lineSpan{{31, 31}}},
		{"among long lines", lines(around(repeatLines(30, 4000), strings.Repeat("y", 5000), repeatLines(30, 4000))...), nil},
		{"under min_bytes", lines(around(repeatLines(30, 10), strings.Repeat("y", 900), repeatLines(30, 10))...), nil},
		{"first line, neighbors on one side", lines(around(nil, strings.Repeat("y", 5000), repeatLines(30, 80))...), []lineSpan{{1, 1}}},
		{"a text of one line", lines(strings.Repeat("y", 5000)), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := scanOne(t, tc.text, sigs, Options{LineBytes: 1024})
			if got := spans(res, "long"); len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
				t.Errorf("findings %v, want %v", got, tc.want)
			}
		})
	}
}

// The median of an even count of neighbors is the lower middle value: a
// 1,500-byte line between a 100-byte and a 2,000-byte line, with a
// window of 1 and a factor of 10, is long (10 × 100 ≤ 1,500).
func TestScan_LongLineLowerMedian(t *testing.T) {
	fx := one("lower median", measureSig("long", "long-line", "window: 1, factor: 10, min_bytes: 1"),
		lines(strings.Repeat("a", 100), strings.Repeat("b", 1500), strings.Repeat("c", 2000)), lineSpan{2, 2}).
		withOpts(Options{LineBytes: 1024})
	fx.run(t)
}

// longLineFixture: long lines near each other and near the text's ends,
// with a window of 3, so that windows cross chunk edges on every
// chunking.
var longLineFixture = func() fixture {
	ls := []string{
		strings.Repeat("L", 400), // 1: long among short lines (window cut at the start)
		"short a", "short b", "short c",
		strings.Repeat("M", 90), // 5: under min_bytes
		"short d",
		strings.Repeat("N", 1500), // 7: long, longer than the view
		strings.Repeat("N", 1500), // 8: long against 7 and the short lines
		"short e", "short f", "short g", "short h",
		strings.Repeat("P", 200), // 13: long
		"s", "s", "s",
		strings.Repeat("Q", 300), // 17: long, last line (window cut at the end)
	}
	return one("long lines", measureSig("long", "long-line", "window: 3, factor: 4, min_bytes: 100"),
		lines(ls...), lineSpan{1, 1}, lineSpan{7, 7}, lineSpan{8, 8}, lineSpan{13, 13}, lineSpan{17, 17}).
		withOpts(Options{LineBytes: 1024})
}()

// Long lines whose windows cross chunk edges are found.
func TestScan_LongLineWindows(t *testing.T) {
	longLineFixture.run(t)
}

// jsonBlockFixtures are the cases of the old json-blob-multiline
// detector, with the same ranges: a simple object, a nested object, an
// array, strings holding brackets and escaped quotes, and an object
// that never closes, cut at max_lines 5.
var jsonBlockFixtures = []fixture{
	one("simple object", measureSig("json", "json-block", ""), lines(
		"service starting up", "request received id=abc", "{", `  "user": "alice",`, `  "action": "login"`, "}", "continuing processing",
	), lineSpan{3, 6}),
	one("nested object", measureSig("json", "json-block", ""), lines(
		"processing event", "{", `  "id": 42,`, `  "metadata": {`, `    "source": "sensor-1",`, `    "tags": ["a", "b"]`, "  },",
		`  "values": [1, 2, 3]`, "}", "next event line",
	), lineSpan{2, 9}),
	one("array", measureSig("json", "json-block", ""), lines(
		"items dump follows", "[", `  "one",`, `  "two",`, `  "three"`, "]", "dump complete",
	), lineSpan{2, 6}),
	one("strings with brackets", measureSig("json", "json-block", ""), lines(
		"payload received", "{", `  "template": "hello {name}, you have [count] messages",`, `  "regex": "\\{.*\\}",`,
		`  "quoted": "she said \"{ok}\"",`, `  "payload": "[1,2,3]"`, "}", "payload done",
	), lineSpan{2, 7}),
	one("unterminated, cut at max_lines", measureSig("json", "json-block", "max_lines: 5"), lines(
		"streaming write failure follows", "{", `  "user": "alice",`, `  "nested": {`, `    "deep": {`, `      "more": "data",`,
		`      "list": [1, 2, 3,`,
	), lineSpan{2, 6}),
}

// jsonBlockMoreFixture: what is not a block (more closers than
// openers, which abandons the block so the next opener starts one; an
// opener with more on its line, a closer under the count, a
// mismatched closer, a closer at another indent), two blocks back to
// back, and one the end of the text closes.
var jsonBlockMoreFixture = one("json blocks and non-blocks", measureSig("json", "json-block", ""), lines(
	"{", "}} too many", "{", `  "a": 1`, "}",
	`request: { "action": "foo" }`,
	"{ inline }",
	"{", "} extra stuff here", "done",
	"{", `  "key": "value"`, "]", "done",
	"{", `  "nested": {`, `    "inner": "value"`, "  }", "    }", "done",
	"first event", "{", `  "id": 1`, "}", "second event", "[", `  "a",`, `  "b"`, "]",
	"{", `  "key": "value"`,
), lineSpan{3, 5}, lineSpan{22, 24}, lineSpan{26, 29}, lineSpan{30, 31})

// The old detector's cases give the same ranges; malformed blocks are no
// finding, and the end of the text closes an open block.
func TestScan_JSONBlockMeasure(t *testing.T) {
	for _, fx := range append(jsonBlockFixtures, jsonBlockMoreFixture) {
		t.Run(fx.name, fx.run)
	}
}

// The bracket count ignores brackets inside strings and follows escapes.
func TestJSONBrackets(t *testing.T) {
	cases := []struct {
		in            string
		opens, closes int
	}{
		{`{`, 1, 0}, {`}`, 0, 1}, {`{}`, 1, 1}, {`[[]]`, 2, 2},
		{`"{}"`, 0, 0}, {`"\""`, 0, 0}, {`"\\"`, 0, 0}, {`"\\{"`, 0, 0},
		{`"a"{`, 1, 0}, {`"{" }`, 0, 1}, {`\"{\"`, 0, 0}, {`"\"{"`, 0, 0},
	}
	for _, tc := range cases {
		if opens, closes := jsonBrackets([]byte(tc.in)); opens != tc.opens || closes != tc.closes {
			t.Errorf("%s: %d opens, %d closes, want %d, %d", tc.in, opens, closes, tc.opens, tc.closes)
		}
	}
}
