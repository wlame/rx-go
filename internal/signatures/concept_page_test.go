package signatures

// The concept page docs/concepts/signatures.md lists every measure
// parameter with its default and range. This test reads that table and
// holds it to the measure table, so the page cannot say something the
// loader does not do.

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// conceptPage is the page, relative to this package's directory.
var conceptPage = filepath.Join("..", "..", "docs", "concepts", "signatures.md")

// measureTableHeader is the header row of the page's parameter table.
var measureTableHeader = []string{"Measure", "Parameter", "Default", "Range"}

// readPageTable returns the body rows of the first Markdown table of the
// page whose header is header, each cell trimmed of spaces and, when
// the whole cell is one code span, of its backticks.
func readPageTable(t *testing.T, header []string) [][]string {
	t.Helper()
	file, err := os.Open(conceptPage)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var rows [][]string
	inTable := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i, c := range cells {
			c = strings.TrimSpace(c)
			if strings.Count(c, "`") == 2 && strings.HasPrefix(c, "`") && strings.HasSuffix(c, "`") {
				c = strings.Trim(c, "`")
			}
			cells[i] = c
		}
		switch {
		case !inTable && slices.Equal(cells, header):
			inTable = true
		case inTable && !strings.HasPrefix(cells[0], "---"):
			rows = append(rows, cells)
		}
	}
	if !inTable {
		t.Fatalf("no table with the header %v in %s", header, conceptPage)
	}
	return rows
}

// The page's parameter table lists exactly the measure table's
// parameters, in its order, with the same defaults and ranges.
func TestConceptPage_MeasureTable(t *testing.T) {
	var want [][]string
	for _, name := range pageMeasureOrder {
		def := measureDefs[name]
		if len(def.params) == 0 {
			want = append(want, []string{name, "—", "—", "—"})
			continue
		}
		for _, p := range def.params {
			want = append(want, []string{name, p.name, pageDefault(p), pageRange(p)})
		}
	}
	got := readPageTable(t, measureTableHeader)
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("the page's table:\n%v\nwant:\n%v", got, want)
	}
	if len(pageMeasureOrder) != len(measureDefs) {
		t.Errorf("the page lists %d measures, the table has %d", len(pageMeasureOrder), len(measureDefs))
	}
}

// pageMeasureOrder is the order the page lists the measures in.
var pageMeasureOrder = []string{"long-line", "binary", "entropy", "json-block", "unprefixed", "base64-run", "density", "time-gap", "time-backwards"}

// pageDefault is a parameter's default as the page writes it.
func pageDefault(p paramSpec) string {
	switch p.kind {
	case paramBucket:
		if p.def == 0 {
			return "auto"
		}
		return formatDuration(secondsDuration(p.def))
	case paramDuration:
		return formatDuration(secondsDuration(p.def))
	}
	return strconv.FormatFloat(p.def, 'f', -1, 64)
}

// pageRange is a parameter's range as the page writes it.
func pageRange(p paramSpec) string {
	switch p.kind {
	case paramBucket:
		return "`auto`, or " + formatDuration(secondsDuration(p.min)) + " to " + formatDuration(secondsDuration(p.max))
	case paramDuration:
		return formatDuration(secondsDuration(p.min)) + " to " + formatDuration(secondsDuration(p.max))
	}
	return strconv.FormatFloat(p.min, 'f', -1, 64) + " to " + strconv.FormatFloat(p.max, 'f', -1, 64)
}
