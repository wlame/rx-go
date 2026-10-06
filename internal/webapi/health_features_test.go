package webapi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// /health lists every row of the feature table, sorted, so a client
// can find a name without knowing the order rows were added in.
func TestHealth_FeaturesAreTheTableSorted(t *testing.T) {
	got := buildHealthResponse(NewServer(Config{AppVersion: "features-test"})).Features
	want := slices.Clone(featureTable)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("features %q; want %q", got, want)
	}
	if !slices.IsSorted(got) {
		t.Fatalf("features %q are not sorted", got)
	}
}

// Every feature the server lists is described in the /health page.
func TestHealth_EveryFeatureIsDocumented(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "api", "endpoints", "health.md"))
	if err != nil {
		t.Fatalf("read the health page: %v", err)
	}
	for _, name := range featureTable {
		if !strings.Contains(string(page), "| `"+name+"` |") {
			t.Errorf("docs/api/endpoints/health.md has no row for the feature %q", name)
		}
	}
}
