package webapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/journal"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/testutil/journalfile"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// writeDamagedJournal writes into dir, as name, a 12-entry journal whose
// entry 3 is zeroed (a reader leaves it out) and whose entry list breaks
// at its last ENTRY_ARRAY object (the entries it lists are not reached),
// and returns its path and the reader's report of it.
func writeDamagedJournal(t *testing.T, dir, name string) (string, journal.Report) {
	t.Helper()
	messages := make([]string, 12)
	for i := range messages {
		messages[i] = fmt.Sprintf("LINE %d of a damaged journal", i+1)
	}
	j := journalfile.Write(t, journalfile.MessageEntries(messages...), journalfile.Options{Compact: true})
	data := bytes.Clone(j.Bytes)
	j.ZeroEntry(data, 2)
	j.BreakList(data, len(j.Arrays)-1)
	path := filepath.Join(dir, name)
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

// GET /v1/index of a journal, and the result of its index task, carry
// the journal section with the reader's counts; a plain file's carry
// journal null. Both answers validate against their schemas.
func TestIndex_AnswersCarryTheJournalSection(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	damaged, report := writeDamagedJournal(t, root, "system.journal")
	plain := filepath.Join(root, "app.log")
	if err := os.WriteFile(plain, []byte("LINE 1 plain\nLINE 2 plain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	srv := NewServer(Config{AppVersion: "unit-test", TaskManager: tasks.New(tasks.Config{})})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	registry := srv.API().OpenAPI().Components.Schemas

	want := map[string]any{
		"entries":             float64(report.Entries),
		"shown":               float64(report.Shown),
		"without_message":     float64(report.WithoutMessage),
		"left_out":            float64(report.LeftOut),
		"not_reached":         float64(report.NotReached),
		"first_damaged_entry": float64(report.FirstDamaged),
		"damage_reason":       report.DamageReason,
		"state":               "offline",
		"compact":             true,
	}
	for _, path := range []string{damaged, plain} {
		task := postJSON(t, ts.URL+"/v1/index", `{"path": "`+path+`", "threshold": 0}`)
		body := waitForTaskBody(t, ts.URL, task["task_id"].(string))
		validateBody(t, registry, "IndexTaskResult", body["result"])
		result, _ := body["result"].(map[string]any)
		index := getJSON(t, ts.URL+"/v1/index?path="+url.QueryEscape(path))
		validateBody(t, registry, "IndexResponse", index)

		for what, answer := range map[string]map[string]any{"the task's result": result, "GET /v1/index": index} {
			value, present := answer["journal"]
			if !present {
				t.Errorf("%s of %s has no journal member", what, filepath.Base(path))
				continue
			}
			if path == plain {
				if value != nil {
					t.Errorf("%s of a plain file has journal %v, want null", what, value)
				}
				continue
			}
			section, _ := value.(map[string]any)
			for key, w := range want {
				if section[key] != w {
					t.Errorf("%s: journal.%s = %v, want %v", what, key, section[key], w)
				}
			}
		}
	}
}

// GET /v1/tree says why each file that is not text is not text, with
// the reason every command refuses it by, and gives null for a text file
// (a journal included) and a directory.
func TestTree_GivesTheReasonAFileIsNotText(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	files := map[string][]byte{
		"app.log":   []byte("LINE 1 text\n"),
		"utf16.log": {0xFF, 0xFE, 'L', 0, '\n', 0},
		"bin.dat":   []byte("LINE 1\x00binary\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	copyJournal(t, "basic-zstd-compact", root, "app.journal")
	unreadable := copyJournal(t, "basic-none-regular", root, "system.journal")
	journalfixture.AddUnknownIncompatibleFlag(t, unreadable)
	if err := os.Mkdir(filepath.Join(root, "archive"), 0o700); err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t)

	tree := getJSON(t, ts.URL+"/v1/tree?path="+url.QueryEscape(root))
	entries, _ := tree["entries"].([]any)
	reasons := map[string]any{}
	for _, item := range entries {
		entry, _ := item.(map[string]any)
		value, present := entry["not_text_reason"]
		if !present {
			t.Errorf("the entry of %v has no not_text_reason member", entry["name"])
		}
		reasons[fmt.Sprint(entry["name"])] = value
	}
	want := map[string]any{
		"app.log":        nil,
		"app.journal":    nil,
		"archive":        nil,
		"utf16.log":      filekind.NotTextPrefix + ": UTF-16 text, which rx does not decode",
		"bin.dat":        filekind.NotTextPrefix + ": a NUL byte in its first 8 KiB",
		"system.journal": "not a text file: a systemd journal rx cannot read: incompatible flags",
	}
	for name, w := range want {
		got, listed := reasons[name]
		if !listed {
			t.Errorf("the tree does not list %s", name)
			continue
		}
		if name == "system.journal" {
			if text, _ := got.(string); !strings.HasPrefix(text, w.(string)) {
				t.Errorf("%s: not_text_reason %v, want one that starts with %q", name, got, w)
			}
			continue
		}
		if got != w {
			t.Errorf("%s: not_text_reason %v, want %v", name, got, w)
		}
	}
}
