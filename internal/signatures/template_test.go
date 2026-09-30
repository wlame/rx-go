package signatures

import (
	"testing"
)

// The template is a signature file that loads with no error and defines
// one signature of each kind, none of them a built-in's id.
func TestTemplate_Loads(t *testing.T) {
	path := writeSignatureFile(t, t.TempDir(), "template.yml", string(Template()))
	cat := loadFiles(path)
	requireNoErrors(t, cat)
	kinds := map[Kind]int{}
	for _, s := range cat.Signatures() {
		if s.Source.Builtin {
			continue
		}
		if s.Overrides {
			t.Errorf("%s replaces a built-in", s.ID)
		}
		if s.Anchor() != nil && s.LinePass() {
			t.Errorf("%s has an anchor and no keywords", s.ID)
		}
		kinds[s.Kind]++
	}
	for _, k := range []Kind{KindRegex, KindBlock, KindRun, KindMeasure} {
		if kinds[k] == 0 {
			t.Errorf("no %s signature in the template", k)
		}
	}
}
