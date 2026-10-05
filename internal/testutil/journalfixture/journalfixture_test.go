package journalfixture

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// From a package other than internal/journal, the helpers find the
// fixtures: the manifest lists every set in every variant, a journal
// decompresses to a file that starts with the journal signature, and a
// set's text is the file its Info names.
func TestHelpers_FindTheFixturesFromAnotherPackage(t *testing.T) {
	listed := map[string]bool{}
	for _, f := range Manifest(t) {
		listed[f.Name] = true
	}
	for _, set := range Sets {
		for _, variant := range Variants {
			if !listed[set+"-"+variant] {
				t.Errorf("the manifest lacks %s-%s", set, variant)
			}
		}
	}

	journal, err := os.ReadFile(Journal(t, "basic-none-regular"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(journal, []byte("LPKSHHRH")) {
		t.Errorf("the decompressed journal starts with %q", journal[:min(len(journal), 8)])
	}

	text := SetText(t, "basic")
	fromInfo, err := os.ReadFile(Info{Name: "basic-none-regular", Set: "basic"}.TextPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(text) == 0 || !bytes.Equal(text, fromInfo) {
		t.Errorf("SetText gives %d bytes, the Info's text path %d", len(text), len(fromInfo))
	}
}

// A chain directory holds every part of the chain, decompressed, and
// nothing else.
func TestChainDir_HoldsTheJournalsOfTheChain(t *testing.T) {
	dir := ChainDir(t, "chain-one-counter")
	parts, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := filepath.Glob(filepath.Join(Root(t), "chain-one-counter", "*.journal.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != len(stored) || len(parts) == 0 {
		t.Fatalf("%d files in the chain directory, %d stored parts", len(parts), len(stored))
	}
	for _, part := range parts {
		if filepath.Ext(part) != ".journal" {
			t.Errorf("%s is not a journal part", filepath.Base(part))
		}
	}
}
