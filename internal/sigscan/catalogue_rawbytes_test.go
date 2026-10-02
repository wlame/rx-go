package sigscan

// The files this repository ships with the catalogue must not hold a
// string shaped like a provider's key, or a secret scanner on the
// repository host (push protection) refuses them. The examples of the
// secret signatures write each token with a YAML escape inside it, so
// the example the loader reads still matches while the file's bytes do
// not.

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// publishedSecretFixtures are fixtures whose tokens the repository
// published before this catalogue: byte copies of the secret fixtures
// of the line detectors the signatures replace. They stay as they are.
var publishedSecretFixtures = map[string]bool{
	"secret-aws-key.log":     true,
	"secret-github-pat.log":  true,
	"secret-jwt.log":         true,
	"secret-pem-key.log":     true,
	"secret-slack-token.log": true,
}

// rawBytesFiles lists the files the scan below reads: the built-in
// signature files, the reference page, and the catalogue's testdata
// (the corpus golden included) but for publishedSecretFixtures.
func rawBytesFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "signatures", "builtin", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no built-in files: %v", err)
	}
	files = append(files, filepath.Join("..", "..", "docs", "reference", "signatures.md"))
	data, err := filepath.Glob(filepath.Join("testdata", "catalogue", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range data {
		if !publishedSecretFixtures[filepath.Base(f)] {
			files = append(files, f)
		}
	}
	return files
}

// tokenShape reports whether re finds a match in line that is not a
// masked value: an excerpt in the corpus golden shows a password as
// "Exa*****", which the generic password regexes still match, and
// which is no secret.
func tokenShape(re *regexp.Regexp, line []byte) bool {
	for _, m := range re.FindAllIndex(line, -1) {
		if !bytes.Contains(line[m[0]:m[1]], []byte("**")) {
			return true
		}
	}
	return false
}

// No line of those files, read as raw bytes, matches the anchor of a
// built-in secret signature, but for a masked value.
func TestBuiltinFiles_HoldNoTokenShapedString(t *testing.T) {
	var anchors []struct {
		id    string
		match func([]byte) bool
	}
	for _, s := range builtinCatalogue(t).Signatures() {
		if s.Group == "secret" && s.Anchor() != nil {
			re := s.Anchor()
			anchors = append(anchors, struct {
				id    string
				match func([]byte) bool
			}{s.ID, func(line []byte) bool { return tokenShape(re, line) }})
		}
	}
	if len(anchors) != 15 {
		t.Fatalf("%d secret anchors, want the 15 regex and block secrets", len(anchors))
	}
	for _, file := range rawBytesFiles(t) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for n := 1; scanner.Scan(); n++ {
			for _, a := range anchors {
				if a.match(scanner.Bytes()) {
					t.Errorf("%s:%d: a %s shape in the file's bytes", file, n, a.id)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
}
