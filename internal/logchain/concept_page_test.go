package logchain

// The concept page docs/concepts/log-chains.md shows the name-template
// table and a table of example names with what the templates make of
// them. These tests read both tables and hold them to Templates and
// matchName, so the page cannot say something the code does not do.

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// conceptPage is the page, relative to this package's directory.
var conceptPage = filepath.Join("..", "..", "docs", "concepts", "log-chains.md")

// The header rows of the page's two tables, which find them.
var (
	templateTableHeader = []string{"Template", "Shape", "Chain name", "Key", "Newest part", "Needs the active file"}
	exampleTableHeader  = []string{"Name", "Template", "Chain name", "Key", "Key kind"}
)

// The words the page writes for a template's key kind and direction.
var (
	keyKindWords   = map[string]KeyKind{"number": KeyNumber, "date": KeyDate}
	newestLowWords = map[string]bool{"lowest number": true, "latest date": false}
	yesNoWords     = map[string]bool{"yes": true, "no": false}
)

// pageTable returns the body rows of the first Markdown table in page
// whose header row is header, each row's cells trimmed of spaces and of
// one pair of backticks. It fails the test when there is none.
func pageTable(t *testing.T, page string, header []string) [][]string {
	t.Helper()
	file, err := os.Open(page)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	var rows [][]string
	inTable, found := false, false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "|") {
			if found {
				break
			}
			inTable = false
			continue
		}
		cells := tableCells(line)
		switch {
		case found:
			if !strings.HasPrefix(cells[0], "---") && !strings.HasPrefix(cells[0], ":-") {
				rows = append(rows, cells)
			}
		case !inTable && slices.Equal(cells, header):
			found = true
		}
		inTable = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	if !found {
		t.Fatalf("%s has no table headed %q", page, header)
	}
	return rows
}

// tableCells splits one Markdown table row into its cells.
func tableCells(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, len(parts))
	for i, part := range parts {
		cell := strings.TrimSpace(part)
		if len(cell) >= 2 && strings.HasPrefix(cell, "`") && strings.HasSuffix(cell, "`") &&
			strings.Count(cell, "`") == 2 {
			cell = cell[1 : len(cell)-1]
		}
		cells[i] = cell
	}
	return cells
}

// The page's template table lists the templates in the order they are
// tried, each with its key kind, its direction, whether it needs the
// active file and the groups its chain name is built from.
func TestConceptPage_TemplateTableIsTheTemplates(t *testing.T) {
	rows := pageTable(t, conceptPage, templateTableHeader)
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row[0]
	}
	want := make([]string, len(Templates))
	for i, tpl := range Templates {
		want[i] = tpl.ID
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("the page lists the templates %q, the code tries %q", ids, want)
	}
	for i, row := range rows {
		tpl := Templates[i]
		if kind, ok := keyKindWords[row[3]]; !ok || kind != tpl.KeyKind {
			t.Errorf("%s: the page says the key is %q, the code says %v", tpl.ID, row[3], tpl.KeyKind)
		}
		if newestLow, ok := newestLowWords[row[4]]; !ok || newestLow != tpl.NewestLow {
			t.Errorf("%s: the page says the newest part has the %q, the code says NewestLow %v", tpl.ID, row[4], tpl.NewestLow)
		}
		if needsActive, ok := yesNoWords[row[5]]; !ok || needsActive != tpl.NeedsActive {
			t.Errorf("%s: the page says %q to needing the active file, the code says %v", tpl.ID, row[5], tpl.NeedsActive)
		}
		groups := map[string]string{"{name}": "name", "{stem}.{ext}": "stem"}
		group, ok := groups[row[2]]
		if !ok || tpl.Pattern.SubexpIndex(group) < 0 {
			t.Errorf("%s: the page builds the chain name as %q, which the pattern's groups do not give", tpl.ID, row[2])
		}
	}
}

// Each example name on the page is matched as its row says: by that
// template, with that chain name, key and key kind; a name the page says
// no template matches is matched by none. Every template has an example.
func TestConceptPage_ExampleNamesMatchAsTheTableSays(t *testing.T) {
	rows := pageTable(t, conceptPage, exampleTableHeader)
	shown := map[string]bool{}
	for _, row := range rows {
		name, template, chain, key, kind := row[0], row[1], row[2], row[3], row[4]
		match, ok := matchName(name)
		if template == "none" {
			if ok {
				t.Errorf("%s: the page says no template matches it, %s does", name, match.template.ID)
			}
			continue
		}
		if !ok {
			t.Errorf("%s: no template matches it, the page says %s", name, template)
			continue
		}
		shown[template] = true
		if match.template.ID != template || match.chain != chain || match.key.Text != key {
			t.Errorf("%s: matched by %s as chain %q with key %q, the page says %s, %q, %q",
				name, match.template.ID, match.chain, match.key.Text, template, chain, key)
		}
		kindWord, _, _ := strings.Cut(kind, " ")
		if want, known := keyKindWords[kindWord]; !known || match.key.Kind != want {
			t.Errorf("%s: key kind %v, the page says %q", name, match.key.Kind, kind)
		}
	}
	for _, tpl := range Templates {
		if !shown[tpl.ID] {
			t.Errorf("the page shows no example of the template %s", tpl.ID)
		}
	}
}
