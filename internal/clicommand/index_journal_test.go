package clicommand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/journal"
	"github.com/wlame/rx-go/internal/testutil/journalfile"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// writeDamagedJournal writes, in a new directory, a 12-entry journal
// whose entry 3 is zeroed (a reader leaves it out) and whose entry list
// breaks at its last ENTRY_ARRAY object (the entries it lists are not
// reached). It returns the file's path and the reader's report of it.
func writeDamagedJournal(t *testing.T) (string, journal.Report) {
	t.Helper()
	messages := make([]string, 12)
	for i := range messages {
		messages[i] = fmt.Sprintf("LINE %d of a damaged journal", i+1)
	}
	j := journalfile.Write(t, journalfile.MessageEntries(messages...), journalfile.Options{State: 2})
	data := bytes.Clone(j.Bytes)
	j.ZeroEntry(data, 2)
	j.BreakList(data, len(j.Arrays)-1)
	path := filepath.Join(t.TempDir(), "system@damaged.journal")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := journal.NewTextReader(bytes.NewReader(data), int64(len(data)), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = text.Close() }()
	if _, err := io.Copy(io.Discard, text); err != nil {
		t.Fatal(err)
	}
	report := text.Report()
	if report.LeftOut == 0 || report.NotReached == 0 {
		t.Fatalf("the damaged journal is not damaged both ways: %+v", report)
	}
	return path, report
}

// `rx index --json` gives a journal's journal section, with the reader's
// counts and the header's state, and a plain file's as null.
func TestIndexJSON_GivesTheJournalSection(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, report := writeDamagedJournal(t)
	plain := tinyLogFile(t)
	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{path, plain}, threshold: &zero})
	indexed, _ := result["indexed"].([]any)
	if len(indexed) != 2 {
		t.Fatalf("indexed %d files, want 2: %v", len(indexed), result)
	}
	entries := map[string]map[string]any{}
	for _, item := range indexed {
		entry, _ := item.(map[string]any)
		entries[fmt.Sprint(entry["path"])] = entry
	}

	section, ok := entries[path]["journal"].(map[string]any)
	if !ok {
		t.Fatalf("the journal's entry has journal %v, want an object", entries[path]["journal"])
	}
	want := map[string]any{
		"entries":             float64(report.Entries),
		"shown":               float64(report.Shown),
		"without_message":     float64(report.WithoutMessage),
		"left_out":            float64(report.LeftOut),
		"not_reached":         float64(report.NotReached),
		"first_damaged_entry": float64(report.FirstDamaged),
		"damage_reason":       report.DamageReason,
		"state":               "archived",
		"compact":             false,
	}
	for key, value := range want {
		if section[key] != value {
			t.Errorf("journal.%s = %v, want %v", key, section[key], value)
		}
	}
	if value, present := entries[plain]["journal"]; !present || value != nil {
		t.Errorf("the plain file's entry has journal %v (present %v), want null", value, present)
	}
}

// `rx index --info` prints a journal's section: its state, its counts and
// its first damaged entry with the reason; a file that is not a journal
// has no such section.
func TestIndexInfo_ShowsTheJournalSection(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, report := writeDamagedJournal(t)
	got := runIndexInfoHuman(t, path)
	want := []string{
		"  journal:\n",
		"    state: archived\n",
		fmt.Sprintf("    entries: %d\n", report.Entries),
		fmt.Sprintf("    shown: %d\n", report.Shown),
		fmt.Sprintf("    without_message: %d\n", report.WithoutMessage),
		fmt.Sprintf("    left_out: %d\n", report.LeftOut),
		fmt.Sprintf("    not_reached: %d\n", report.NotReached),
		fmt.Sprintf("    first_damaged_entry: %d (%s)\n", report.FirstDamaged, report.DamageReason),
	}
	for _, line := range want {
		if !strings.Contains(got, line) {
			t.Errorf("rx index --info lacks %q:\n%s", line, got)
		}
	}

	whole := filepath.Join(t.TempDir(), "system.journal")
	data, err := os.ReadFile(journalfixture.Journal(t, "basic-zstd-compact"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(whole, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runIndexInfoHuman(t, whole); !strings.Contains(got, "    left_out: 0\n") ||
		!strings.Contains(got, "    first_damaged_entry: none\n") {
		t.Errorf("rx index --info of an undamaged journal:\n%s", got)
	}
	if got := runIndexInfoHuman(t, tinyLogFile(t)); strings.Contains(got, "  journal:\n") {
		t.Errorf("rx index --info of a plain file shows a journal section:\n%s", got)
	}
}

// An index of format 11 stored for a journal is not used: `rx index`
// builds a format 12 one, with its journal section, in its place.
func TestIndex_RebuildsAJournalIndexOfFormat11(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, report := writeDamagedJournal(t)
	zero := 0
	runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})

	cachePath := index.GetCachePath(path)
	body, err := os.ReadFile(cachePath) //nolint:gosec // the test's own cache
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	stored["version"] = 11
	delete(stored, "journal")
	if body, err = json.Marshal(stored); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := index.LoadForSource(path); err == nil && loaded != nil {
		t.Fatalf("the format 11 index is read as current (version %d)", loaded.Version)
	}

	runIndexJSON(t, indexParams{paths: []string{path}, threshold: &zero})
	rebuilt, err := index.LoadFromPath(cachePath)
	if err != nil {
		t.Fatalf("load the rebuilt index: %v", err)
	}
	if rebuilt.Version != 12 || rebuilt.Journal == nil || rebuilt.Journal.LeftOut != report.LeftOut {
		t.Errorf("the stored index has version %d and journal section %+v; want 12 with the damage",
			rebuilt.Version, rebuilt.Journal)
	}
}
