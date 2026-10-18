package assets

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
)

// Every embedded file is the release Files names: its bytes have the
// recorded SHA-256, and SHA256SUMS beside the files records the same
// digests, so an upgrade changes the table and the sums file together.
func TestEmbeddedFilesHaveTheRecordedDigests(t *testing.T) {
	sums := readSums(t)
	if len(sums) != len(Files) {
		t.Fatalf("SHA256SUMS lists %d files, Files %d", len(sums), len(Files))
	}
	for _, f := range Files {
		data, err := Read(f.Name)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if got := Digest(data); got != f.SHA256 {
			t.Errorf("%s: embedded bytes have sha256 %s, Files records %s", f.Name, got, f.SHA256)
		}
		if sums[f.Name] != f.SHA256 {
			t.Errorf("%s: SHA256SUMS records %q, Files %q", f.Name, sums[f.Name], f.SHA256)
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", f.Name)
		}
	}
	if _, err := Read("missing.js"); err == nil {
		t.Error("Read of a file not in Files should fail")
	}
}

// readSums parses SHA256SUMS (the `sha256sum` format: digest, two
// spaces, name) into name → digest.
func readSums(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile("SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	sums := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		digest, name, ok := strings.Cut(scanner.Text(), "  ")
		if !ok {
			t.Fatalf("SHA256SUMS line %q is not `digest  name`", scanner.Text())
		}
		sums[name] = digest
	}
	return sums
}
