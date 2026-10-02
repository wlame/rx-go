package signatures

// The reference page docs/reference/signatures.md lists every built-in
// signature, group by group, as the embedded files define it. The page
// is generated: this test builds it from the catalogue and fails when
// the committed page differs. `just docs-signatures` rewrites it:
//
//	go test ./internal/signatures/ -run='^TestReferencePage$' -update-reference

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// updateReference rewrites the reference page instead of comparing with
// it.
var updateReference = flag.Bool("update-reference", false, "rewrite docs/reference/signatures.md from the built-in signatures")

// referencePage is the page, relative to this package's directory.
var referencePage = filepath.Join("..", "..", "docs", "reference", "signatures.md")

// The committed reference page is the one the built-in signatures give.
func TestReferencePage(t *testing.T) {
	got := renderReferencePage(Load(LoadOptions{Paths: []string{}}))
	if *updateReference {
		if err := os.MkdirAll(filepath.Dir(referencePage), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(referencePage, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(referencePage)
	if err != nil {
		t.Fatalf("%v (run `just docs-signatures` to write it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s is not the page the built-in signatures give: run `just docs-signatures` and commit the result", referencePage)
	}
}

// The page names every built-in signature once, as a heading, under
// its group's heading.
func TestReferencePage_EverySignatureUnderItsGroup(t *testing.T) {
	cat := Load(LoadOptions{Paths: []string{}})
	page := renderReferencePage(cat)
	group := ""
	seen := map[string]string{}
	for _, line := range strings.Split(page, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			group = strings.TrimPrefix(line, "## ")
		case strings.HasPrefix(line, "### "):
			id := strings.TrimPrefix(line, "### ")
			if _, dup := seen[id]; dup {
				t.Errorf("%s has two headings", id)
			}
			seen[id] = group
		}
	}
	for _, s := range cat.Signatures() {
		if seen[s.ID] != s.Group {
			t.Errorf("%s is under %q, want %q", s.ID, seen[s.ID], s.Group)
		}
	}
	if len(seen) != len(cat.Signatures()) {
		t.Errorf("the page has %d signature headings, the catalogue %d signatures", len(seen), len(cat.Signatures()))
	}
}

// renderReferencePage writes the reference page of a catalogue's
// built-in signatures.
func renderReferencePage(cat *Catalogue) string {
	var b strings.Builder
	sigs := cat.Signatures()
	byGroup := map[string][]*Signature{}
	defaults := 0
	for _, s := range sigs {
		byGroup[s.Group] = append(byGroup[s.Group], s)
		if s.Default {
			defaults++
		}
	}
	groups := cat.Groups()
	b.WriteString("# Built-in signatures\n\n")
	b.WriteString("<!-- Generated from internal/signatures/builtin/*.yml by `just docs-signatures`. Do not edit by hand. -->\n\n")
	fmt.Fprintf(&b, "rx has %d built-in signatures in %d groups. %d of them are in the default set, which\n", len(sigs), len(groups), defaults)
	b.WriteString("`rx --signatures` runs; the others are opt-in and run when they or their group are\n")
	b.WriteString("named, as in `--signatures=default,pii`. The model and the file format are in\n")
	b.WriteString("[Signatures](../concepts/signatures.md); `rx signatures show ID` prints one definition in\n")
	b.WriteString("full, with its regexes and examples.\n\n")
	b.WriteString("Keywords are the literals a line must hold before the signature's regex is tried on it,\n")
	b.WriteString("compared without case: a line needs one keyword of each set, and the sets are joined by\n")
	b.WriteString("\"and\" below. A signature without keywords is tried on every line.\n\n")
	b.WriteString("| Group | Signatures | In the default set | What it finds |\n|---|---|---|---|\n")
	for _, g := range groups {
		members := byGroup[g.Name]
		on := 0
		for _, s := range members {
			if s.Default {
				on++
			}
		}
		fmt.Fprintf(&b, "| [%s](#%s) | %d | %d | %s |\n", g.Name, g.Name, len(members), on, markdownText(g.Description))
	}
	for _, g := range groups {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", g.Name, markdownText(g.Description))
		for _, s := range byGroup[g.Name] {
			writeReferenceSignature(&b, s)
		}
	}
	return b.String()
}

// writeReferenceSignature writes one signature's section.
func writeReferenceSignature(b *strings.Builder, s *Signature) {
	fmt.Fprintf(b, "\n### %s\n\n", s.ID)
	marks := []string{"level `" + string(s.Level) + "`", referenceDefault(s), referenceKind(s)}
	if s.Redact {
		marks = append(marks, "values masked")
	}
	fmt.Fprintf(b, "**%s** · %s\n\n", markdownText(s.Name), strings.Join(marks, " · "))
	fmt.Fprintf(b, "%s\n\n", markdownText(s.Description))
	fmt.Fprintf(b, "- Keywords: %s\n", referenceKeywords(s))
	if len(s.Tags) > 0 {
		fmt.Fprintf(b, "- Tags: %s\n", codeList(s.Tags))
	}
	for i, link := range s.Links {
		prefix := "- Links: "
		if i > 0 {
			prefix = "  "
		}
		sep := ""
		if i < len(s.Links)-1 {
			sep = ","
		}
		fmt.Fprintf(b, "%s<%s>%s\n", prefix, link, sep)
	}
}

// referenceDefault says whether a signature is in the default set.
func referenceDefault(s *Signature) string {
	if s.Default {
		return "default"
	}
	return "opt-in"
}

// referenceKind names a signature's kind, with what makes it up: the
// form of a run, the name and parameters of a measure.
func referenceKind(s *Signature) string {
	switch {
	case s.Kind == KindRun && s.Run.Blank:
		return fmt.Sprintf("run of blank lines, at least %d", s.Run.Min)
	case s.Kind == KindRun && s.Run.Same:
		return fmt.Sprintf("run of equal lines, at least %d", s.Run.Min)
	case s.Kind == KindRun:
		return fmt.Sprintf("run, at least %d lines", s.Run.Min)
	case s.Kind == KindMeasure:
		var params []string
		for _, p := range s.Measure.Params.Fields() {
			params = append(params, p.Name+" "+referenceParam(p.Value))
		}
		if len(params) == 0 {
			return "measure `" + s.Measure.Name + "`"
		}
		return "measure `" + s.Measure.Name + "` (" + strings.Join(params, ", ") + ")"
	}
	return string(s.Kind)
}

// referenceParam writes one measure parameter's value.
func referenceParam(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// referenceKeywords writes a signature's keyword clauses: the members
// of a clause joined by "or", the clauses by "and".
func referenceKeywords(s *Signature) string {
	if len(s.Clauses) == 0 {
		return "none: tried on every line"
	}
	clauses := make([]string, len(s.Clauses))
	for i, c := range s.Clauses {
		words := make([]string, len(c))
		for j, w := range c {
			words[j] = "`" + strconv.Quote(w) + "`"
		}
		clauses[i] = strings.Join(words, " or ")
	}
	how := "derived from the regex"
	if s.KeywordsDeclared {
		how = "declared"
	}
	return strings.Join(clauses, "; and ") + " (" + how + ")"
}

// codeList writes words as code spans, comma-separated.
func codeList(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = "`" + w + "`"
	}
	return strings.Join(out, ", ")
}

// markdownText makes free text safe to place in Markdown: its lines
// joined into one, the characters Markdown would read as formatting
// escaped, and < > & written as entities so no text reads as HTML.
func markdownText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	var b strings.Builder
	for _, r := range text {
		switch r {
		case '\\', '`', '*', '_', '[', ']', '|', '#':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
