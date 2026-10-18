package findings

import (
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
)

// The matching part carries the keyword clauses the author declared and
// none of the clauses rx derives from the anchor: derived clauses come
// from the regex, which the part carries already, and another regex
// engine would derive other ones for the same signature.
func TestMatchingPart_KeepsDeclaredKeywordsOnly(t *testing.T) {
	cat := loadCatalogue(t, probeRegex)
	sig, _ := cat.Lookup("probe")
	if sig.KeywordsDeclared || len(sig.Clauses) == 0 {
		t.Fatalf("the probe has declared keywords %v or no derived clauses %v; the case tests nothing", sig.KeywordsDeclared, sig.Clauses)
	}
	if part := matchingPartOf(sig); part.Keywords != nil {
		t.Errorf("derived clauses in the matching part: %v", part.Keywords)
	}

	declared := loadCatalogue(t, add(probeRegex, "keywords: [error, boom]"))
	with, _ := declared.Lookup("probe")
	if !with.KeywordsDeclared {
		t.Fatal("keywords: was not read as declared")
	}
	if part := matchingPartOf(with); len(part.Keywords) == 0 || part.Keywords[0][0] != "error" {
		t.Errorf("declared clauses missing from the matching part: %v", part.Keywords)
	}
}

// The derived clauses are varied by hand (derivation is deterministic
// from the anchor): two copies of one signature whose derived clauses
// differ have one key, and a declared empty list hashes as no gate.
func TestKey_IgnoresDerivedKeywordClauses(t *testing.T) {
	opts := testOptions(t)
	cat := loadCatalogue(t, probeRegex)
	sig, _ := cat.Lookup("probe")
	original := Key(sig, cat, opts)

	varied := *sig
	varied.Clauses = [][]string{{"foo"}, {"bar", "baz"}}
	if got := Key(&varied, cat, opts); got != original {
		t.Errorf("the key changed with the derived clauses: %s vs %s", got, original)
	}
	none := *sig
	none.Clauses = nil
	if got := Key(&none, cat, opts); got != original {
		t.Errorf("the key changed when the derived clauses were dropped: %s vs %s", got, original)
	}

	emptyDeclared := loadCatalogue(t, add(probeRegex, "keywords: []"))
	empty, _ := emptyDeclared.Lookup("probe")
	if !empty.KeywordsDeclared || empty.Clauses != nil {
		t.Fatalf("keywords: [] read as declared %v, clauses %v", empty.KeywordsDeclared, empty.Clauses)
	}
	if got := Key(empty, emptyDeclared, opts); got != original {
		t.Errorf("keywords: [] hashes differently from no gate: %s vs %s", got, original)
	}
	if !strings.Contains(string(canonicalJSON(matchingPartOf(empty))), `"keywords":null`) {
		t.Errorf("keywords: [] is not null in the part: %s", canonicalJSON(matchingPartOf(empty)))
	}
}

// With masking on, the key of the masked ranges does not depend on the
// derived clauses of the redacting signatures either.
func TestRedactorsHash_IgnoresDerivedKeywordClauses(t *testing.T) {
	cat := loadCatalogue(t, defKey)
	redactors := cat.Redactors()
	if len(redactors) == 0 {
		t.Fatal("no redacting signature")
	}
	original := redactorsHash(redactors)
	varied := make([]*signatures.Signature, 0, len(redactors))
	for _, r := range redactors {
		v := *r
		v.Clauses = [][]string{{"other"}}
		varied = append(varied, &v)
	}
	if got := redactorsHash(varied); got != original {
		t.Errorf("the ranges key changed with the derived clauses: %s vs %s", got, original)
	}
}
