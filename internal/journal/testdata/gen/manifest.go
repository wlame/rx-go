package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// fixtureInfo is one object of fixtures.json: a journal fixture and the
// counts journalctl gives for it.
type fixtureInfo struct {
	Name           string `json:"name"`        // path under testdata without ".journal.zst"
	Set            string `json:"set"`         // basic, many, stamps or the chain's directory
	Compression    string `json:"compression"` // none, xz, lz4 or zstd, as the header says
	Compact        bool   `json:"compact"`
	Entries        int64  `json:"entries"`         // entries `journalctl -o export` gives
	Shown          int64  `json:"shown"`           // entries journalctl prints
	WithoutMessage int64  `json:"without_message"` // entries without a MESSAGE field
}

// entryFirstLine matches the first line of an entry in journalctl's
// text: a line that starts with the 32-byte UTC time. The continuation
// lines of a multi-line message start with spaces.
var entryFirstLine = regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}\+00:00`)

// runManifest prints fixtures.json for the journals under --work. make.sh
// decompresses every stored fixture there as NAME.journal, beside the
// `journalctl -o export` output (NAME.export) and the text journalctl
// prints (NAME.txt) for it.
func runManifest(args []string) error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	work := flags.String("work", "", "directory of the decompressed fixtures")
	if err := parseFlags(flags, args, "work"); err != nil {
		return err
	}
	var journals []string
	// WalkDir calls the function for every file and directory under work;
	// an error it passes in (an unreadable directory) ends the walk.
	err := filepath.WalkDir(*work, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".journal") {
			journals = append(journals, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Sort(journals)
	fixtures := make([]fixtureInfo, 0, len(journals))
	for _, p := range journals {
		info, describeErr := describeFixture(*work, p)
		if describeErr != nil {
			return describeErr
		}
		fixtures = append(fixtures, info)
	}
	out, err := json.MarshalIndent(fixtures, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(out, '\n'))
	return err
}

// describeFixture counts the entries of the journal at p.
func describeFixture(work, p string) (fixtureInfo, error) {
	rel, err := filepath.Rel(work, p)
	if err != nil {
		return fixtureInfo{}, err
	}
	name := strings.TrimSuffix(filepath.ToSlash(rel), ".journal")
	info := fixtureInfo{Name: name, Set: setOf(name)}
	if headerErr := info.readHeader(p); headerErr != nil {
		return fixtureInfo{}, fmt.Errorf("%s: %w", name, headerErr)
	}
	base := strings.TrimSuffix(p, ".journal")
	entries, err := readExport(base + ".export")
	if err != nil {
		return fixtureInfo{}, err
	}
	info.Entries = int64(len(entries))
	for _, e := range entries {
		if !e.hasField("MESSAGE") {
			info.WithoutMessage++
		}
	}
	text, err := os.ReadFile(base + ".txt")
	if err != nil {
		return fixtureInfo{}, err
	}
	info.Shown = int64(len(entryFirstLine.FindAllIndex(text, -1)))
	if info.Entries != info.Shown+info.WithoutMessage {
		return fixtureInfo{}, fmt.Errorf("%s: %d entries, but %d shown and %d without a message",
			name, info.Entries, info.Shown, info.WithoutMessage)
	}
	return info, nil
}

// setOf returns the set a fixture belongs to: the directory of a chain's
// part, else the part of the name before its first "-".
func setOf(name string) string {
	if dir := path.Dir(name); dir != "." {
		return dir
	}
	set, _, _ := strings.Cut(name, "-")
	return set
}

// readHeader fills in the compression and mode from the journal's
// header. For a fixture of a set it also checks that the header agrees
// with the variant the name gives (SET-COMPRESSION-MODE).
func (info *fixtureInfo) readHeader(p string) error {
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	h, err := readHeader(data)
	if err != nil {
		return err
	}
	compression, err := h.compression()
	if err != nil {
		return err
	}
	info.Compression = compression.name
	info.Compact = h.compact()
	if strings.Contains(info.Name, "/") {
		return nil
	}
	mode := "regular"
	if info.Compact {
		mode = "compact"
	}
	if want := info.Set + "-" + info.Compression + "-" + mode; info.Name != want {
		return fmt.Errorf("the header describes %s", want)
	}
	return nil
}
