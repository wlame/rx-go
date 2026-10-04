package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// compressThreshold is the size from which systemd compresses a DATA
// object's payload ("NAME=value") in a journal that compresses.
const compressThreshold = 512

// runCheck checks a journal journal-remote wrote from an export stream:
// every field sent reached it unchanged, it holds no field that was not
// sent, its header names the compression and mode asked for, and its
// DATA objects are compressed as that compression says.
func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	sentPath := fs.String("sent", "", "export stream given to journal-remote")
	storedPath := fs.String("stored", "", "`journalctl -o export` of the journal")
	journalPath := fs.String("journal", "", "the journal")
	compressionName := fs.String("compression", "", "none, xz, lz4 or zstd")
	compactFlag := fs.String("compact", "", "1 for compact mode, 0 for regular")
	if err := parseFlags(fs, args, "sent", "stored", "journal", "compression", "compact"); err != nil {
		return err
	}
	want, err := compressionByName(*compressionName)
	if err != nil {
		return err
	}
	wantCompact, err := strconv.ParseBool(*compactFlag)
	if err != nil {
		return fmt.Errorf("--compact: %w", err)
	}
	sent, err := readExport(*sentPath)
	if err != nil {
		return err
	}
	stored, err := readExport(*storedPath)
	if err != nil {
		return err
	}
	if missing := checkFieldsReached(sent, stored); missing != nil {
		return fmt.Errorf("%s: %w", *journalPath, missing)
	}
	data, err := os.ReadFile(*journalPath)
	if err != nil {
		return err
	}
	if wrong := checkVariant(data, want, wantCompact, hasCompressibleValue(sent)); wrong != nil {
		return fmt.Errorf("%s: %w", *journalPath, wrong)
	}
	return nil
}

// readExport parses the export stream in the file at path.
func readExport(path string) ([]entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entries, err := parseExport(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, nil
}

// checkFieldsReached compares the entries sent with the entries the
// journal gives back: the same number, each with the same times and boot
// and the same set of FIELD=value items. A set, because the journal
// stores an item an entry repeats exactly once, and in its own order.
func checkFieldsReached(sent, stored []entry) error {
	if len(sent) != len(stored) {
		return fmt.Errorf("%d entries sent, %d stored", len(sent), len(stored))
	}
	var problems []error
	for k := range sent {
		s, j := sent[k], stored[k]
		if s.realtime != j.realtime || s.monotonic != j.monotonic || s.bootID != j.bootID {
			problems = append(problems, fmt.Errorf("entry %d: times or boot changed", k+1))
		}
		sentItems, storedItems := itemSet(s), itemSet(j)
		for item := range sentItems {
			if !storedItems[item] {
				problems = append(problems, fmt.Errorf("entry %d: the %s field sent is not stored", k+1, itemName(item)))
			}
		}
		for item := range storedItems {
			if !sentItems[item] {
				problems = append(problems, fmt.Errorf("entry %d: the %s field stored was not sent", k+1, itemName(item)))
			}
		}
	}
	// errors.Join returns nil for no errors, and otherwise one error
	// whose text has one line per problem.
	return errors.Join(problems...)
}

// itemSet returns an entry's fields as a set of "NAME=value" strings.
func itemSet(e entry) map[string]bool {
	set := make(map[string]bool, len(e.fields))
	for _, f := range e.fields {
		set[f.name+"="+string(f.value)] = true
	}
	return set
}

// itemName returns the NAME part of a "NAME=value" string.
func itemName(item string) string {
	name, _, _ := strings.Cut(item, "=")
	return name
}

// hasCompressibleValue reports whether any field sent is large enough
// for a journal that compresses to compress it.
func hasCompressibleValue(entries []entry) bool {
	for _, e := range entries {
		for _, f := range e.fields {
			if len(f.name)+1+len(f.value) >= compressThreshold {
				return true
			}
		}
	}
	return false
}

// checkVariant checks that the journal's header names the compression and
// mode asked for, that no DATA object is compressed in another way, and,
// when a value was large enough, that at least one is compressed.
func checkVariant(data []byte, want compressionFlag, wantCompact, expectCompressed bool) error {
	h, err := readHeader(data)
	if err != nil {
		return err
	}
	got, err := h.compression()
	if err != nil {
		return err
	}
	if got.name != want.name {
		return fmt.Errorf("the header names compression %s, want %s", got.name, want.name)
	}
	if h.compact() != wantCompact {
		return fmt.Errorf("compact mode is %t, want %t", h.compact(), wantCompact)
	}
	flags, err := dataObjectFlags(data, h)
	if err != nil {
		return err
	}
	compressed := 0
	for _, f := range flags {
		if f == 0 {
			continue
		}
		if f != want.objectFlag {
			return fmt.Errorf("a DATA object has flags %#x in a journal that compresses with %s", f, want.name)
		}
		compressed++
	}
	if expectCompressed && want.objectFlag != 0 && compressed == 0 {
		return fmt.Errorf("no DATA object is compressed with %s", want.name)
	}
	return nil
}
