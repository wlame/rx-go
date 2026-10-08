package trace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// classifiedFiles pins and classifies each path as a search does, for
// a plan a test hands the engine.
func classifiedFiles(t *testing.T, paths ...string) []SearchFile {
	t.Helper()
	files := make([]SearchFile, 0, len(paths))
	for _, p := range paths {
		src, err := sandbox.Pin(p)
		if err != nil {
			t.Fatalf("pin %s: %v", p, err)
		}
		file, err := ClassifyForSearch(src)
		if err != nil {
			t.Fatalf("classify %s: %v", p, err)
		}
		files = append(files, file)
	}
	return files
}

// A plan is searched in its own order: its files get their ids in that
// order (whatever their names), its skips and walked directories come
// back in skipped_files, skip_reasons and scanned_files, and its paths
// are the answer's path.
func TestEngine_RunResolved_SearchesThePlanInItsOrder(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	names := []string{"c.log", "a.log", "b.log"}
	for i, name := range names {
		text := "LINE 1 error " + name + "\nLINE 2 quiet\n"
		if i == 1 {
			text = "LINE 1 quiet\nLINE 2 error " + name + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ordered := []string{filepath.Join(dir, "c.log"), filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")}
	plan := &SearchPlan{
		Paths:       []string{dir},
		Files:       classifiedFiles(t, ordered...),
		ScannedDirs: []string{dir},
		Skipped:     []rxtypes.SkippedFile{{Path: filepath.Join(dir, "d.log"), Reason: "a reason the resolver gave"}},
	}

	resp, err := New().RunResolved(context.Background(), func(context.Context) (*SearchPlan, error) {
		return plan, nil
	}, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("RunResolved: %v", err)
	}

	for i, p := range ordered {
		if id := "f" + string(rune('1'+i)); resp.Files[id] != p {
			t.Errorf("files[%s] = %q, want %q (the plan's order)", id, resp.Files[id], p)
		}
	}
	var got []string
	for _, m := range resp.Matches {
		got = append(got, resp.Files[m.File])
	}
	if !slices.Equal(got, ordered) {
		t.Errorf("matches come from %v, want the plan's order %v", got, ordered)
	}
	if resp.Matches[1].AbsoluteLineNumber != 2 {
		t.Errorf("a.log's match is on line %d, want 2", resp.Matches[1].AbsoluteLineNumber)
	}
	if !slices.Equal(resp.Path, []string{dir}) {
		t.Errorf("path %v, want the plan's %v", resp.Path, []string{dir})
	}
	if !slices.Equal(resp.ScannedFiles, ordered) {
		t.Errorf("scanned_files %v, want every file of the plan, %v", resp.ScannedFiles, ordered)
	}
	if !slices.Equal(resp.SkipReasons, plan.Skipped) || !slices.Equal(resp.SkippedFiles, []string{plan.Skipped[0].Path}) {
		t.Errorf("skips %v / %v, want the plan's %v", resp.SkippedFiles, resp.SkipReasons, plan.Skipped)
	}
}

// RunWithOptions is RunResolved over the plan its walk makes: the same
// answer for the same paths.
func TestEngine_RunWithOptions_IsRunResolvedOverItsWalk(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	for _, name := range []string{"x.log", "x.log.1", "y.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("error in "+name+"\nfine\nerror again\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "z.bin"), []byte("error\x00binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := []string{dir}
	opts := Options{ContextBefore: 1, ContextAfter: 1, NoCache: true}

	walked, err := New().RunWithOptions(context.Background(), paths, []string{"error"}, opts)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	resolved, err := New().RunResolved(context.Background(), func(context.Context) (*SearchPlan, error) {
		return expandPaths(paths, true), nil
	}, []string{"error"}, opts)
	if err != nil {
		t.Fatalf("RunResolved: %v", err)
	}
	if diff := traceanswer.Difference(resolved, walked, nil); diff != "" {
		t.Fatalf("the two answers differ: %s", diff)
	}
	if len(walked.SkippedFiles) != 1 || filepath.Base(walked.SkippedFiles[0]) != "z.bin" {
		t.Errorf("skipped %v, want the binary file", walked.SkippedFiles)
	}
}

// The patterns are checked before the resolver runs, so a search whose
// pattern cannot compile walks nothing; and a resolver's error ends the
// search as it is.
func TestEngine_RunResolved_ChecksPatternsBeforeResolving(t *testing.T) {
	requireRipgrep(t)
	called := false
	_, err := New().RunResolved(context.Background(), func(context.Context) (*SearchPlan, error) {
		called = true
		return &SearchPlan{}, nil
	}, []string{"(unclosed"}, Options{})
	if !errors.Is(err, ErrInvalidPattern) {
		t.Fatalf("error %v, want ErrInvalidPattern", err)
	}
	if called {
		t.Error("the resolver ran for a pattern that does not compile")
	}

	failure := errors.New("the resolver failed")
	_, err = New().RunResolved(context.Background(), func(context.Context) (*SearchPlan, error) {
		return nil, failure
	}, []string{"error"}, Options{})
	if !errors.Is(err, failure) {
		t.Fatalf("error %v, want the resolver's", err)
	}
}
