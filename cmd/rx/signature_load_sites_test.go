package main

// Only the commands that use signatures may load them: every other
// command must not pay for reading the signature files, and must not
// fail or slow down because of them. signatures.Load prints nothing and
// returns no error, so no run of a binary can tell whether a command
// called it. These tests read the source instead: every place that
// names signatures.Load, or a function that reaches it, must be in a
// file of allowedLoadFiles.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// allowedLoadFiles are the files, relative to the repository root, that
// may name signatures.Load or a function that reaches it: the commands
// that use signatures. A command that starts using them adds its file
// here.
var allowedLoadFiles = []string{
	"internal/clicommand/scan.go",
	"internal/clicommand/serve.go",
	"internal/clicommand/signatures.go",
	"internal/clicommand/trace.go",
	"internal/clicommand/trace_signatures.go",
}

// loadSite is one place a file names signatures.Load or a function
// that reaches it.
type loadSite struct {
	File string // relative to the scanned root
	Line int
	Name string // the name referenced
}

// The source names signatures.Load only in the allowed files: a command
// that does not use signatures never loads them.
func TestSignatureLoad_OnlyInCommandsThatUseSignatures(t *testing.T) {
	root := filepath.Join("..", "..")
	sites := signatureLoadSites(t, root, "github.com/wlame/rx-go")
	allowedSeen := false
	for _, s := range sites {
		if slices.Contains(allowedLoadFiles, s.File) {
			allowedSeen = true
			continue
		}
		t.Errorf("%s:%d names %s, which loads the signatures; only %v may", s.File, s.Line, s.Name, allowedLoadFiles)
	}
	if !allowedSeen {
		t.Errorf("no file names signatures.Load at all: the scan no longer sees the calls")
	}
}

// The scan finds a call through an import alias and a call through a
// helper of an allowed file, and does not count a constructor that
// loads only inside the closure it returns.
func TestSignatureLoadSites_FindsLoadsOutsideTheAllowList(t *testing.T) {
	sites := signatureLoadSites(t, filepath.Join("testdata", "loadsites"), "example.com/rx")
	var outside []loadSite
	for _, s := range sites {
		if !slices.Contains(allowedLoadFiles, s.File) {
			outside = append(outside, s)
		}
	}
	want := []loadSite{
		{File: "internal/clicommand/samples.go", Line: 5, Name: "example.com/rx/internal/clicommand.runCheckLater"},
		{File: "internal/other/other.go", Line: 6, Name: "example.com/rx/internal/signatures.Load"},
	}
	if !slices.Equal(outside, want) {
		t.Errorf("sites outside the allow-list %+v, want %+v", outside, want)
	}
}

// sourceFile is one parsed non-test Go file of the scanned tree.
type sourceFile struct {
	rel     string // path relative to the root, with forward slashes
	pkgPath string // import path of its package
	ast     *ast.File
	imports map[string]string // local name → import path
}

// signatureLoadSites returns every place under root's cmd, internal and
// pkg directories (test files, testdata and the signatures package
// itself left out) that names signatures.Load or a function that
// reaches it, sorted by file and line.
//
// A function reaches Load when its body names Load, or a function that
// reaches it, outside a function literal. A name inside a closure is a
// site of its own file but does not make the enclosing function reach
// Load: a command constructor whose RunE closure loads only builds the
// command, and building every command is what the root does.
func signatureLoadSites(t *testing.T, root, module string) []loadSite {
	t.Helper()
	signaturesPath := module + "/internal/signatures"
	files := parseSourceTree(t, root, module, signaturesPath)
	reaching := map[string]bool{signaturesPath + ".Load": true}
	// Grow the set of functions that reach Load until it stops growing:
	// each round adds the functions that name one already in it.
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			for _, decl := range f.ast.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				key := f.pkgPath + "." + fn.Name.Name
				if reaching[key] {
					continue
				}
				if namesAny(f, fn.Body, reaching, false) {
					reaching[key] = true
					changed = true
				}
			}
		}
	}
	var sites []loadSite
	for _, f := range files {
		for _, decl := range f.ast.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				if key, ok := referenceKey(f, n); ok && reaching[key] {
					if fn, isDecl := decl.(*ast.FuncDecl); isDecl && n == ast.Node(fn.Name) {
						return true
					}
					sites = append(sites, loadSite{File: f.rel, Line: sourceFileSet.Position(n.Pos()).Line, Name: key})
				}
				return true
			})
		}
	}
	slices.SortFunc(sites, func(a, b loadSite) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
	return sites
}

// namesAny reports whether node names a function of the set; with
// intoClosures false it does not look inside function literals.
func namesAny(f sourceFile, node ast.Node, set map[string]bool, intoClosures bool) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, isLit := n.(*ast.FuncLit); isLit && !intoClosures {
			return false
		}
		if key, ok := referenceKey(f, n); ok && set[key] {
			found = true
		}
		return true
	})
	return found
}

// referenceKey is the "importpath.Name" a node names: pkg.Name through
// an import of the file, or a bare identifier of the file's own
// package. Other nodes give false.
func referenceKey(f sourceFile, n ast.Node) (string, bool) {
	switch x := n.(type) {
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			if path, imported := f.imports[id.Name]; imported {
				return path + "." + x.Sel.Name, true
			}
		}
	case *ast.Ident:
		return f.pkgPath + "." + x.Name, true
	}
	return "", false
}

// sourceFileSet holds the positions of every file parsed by the scans.
var sourceFileSet = token.NewFileSet()

// parseSourceTree parses the non-test Go files under root's cmd,
// internal and pkg directories, without testdata directories and
// without the signatures package. A dot import of the signatures
// package fails the test: its names could not be told apart.
func parseSourceTree(t *testing.T, root, module, signaturesPath string) []sourceFile {
	t.Helper()
	var files []sourceFile
	for _, top := range []string{"cmd", "internal", "pkg"} {
		dir := filepath.Join(root, top)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if d.Name() == "testdata" || module+"/"+rel == signaturesPath {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(sourceFileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			f := sourceFile{rel: rel, pkgPath: module + "/" + filepath.ToSlash(filepath.Dir(rel)), ast: parsed, imports: map[string]string{}}
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				name := path[strings.LastIndex(path, "/")+1:]
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if name == "." && path == signaturesPath {
					t.Errorf("%s dot-imports the signatures package; the scan cannot follow its names", rel)
				}
				f.imports[name] = path
			}
			files = append(files, f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}
