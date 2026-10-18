package webapi

import (
	"strings"
	"testing"
)

// cli_command names the selection the answer's findings come from: the
// request's selection narrowed by signature= and group=, as ids when a
// filter narrows it, so the command lists exactly what the filtered
// answer holds. A filter the CLI has no flag for (q, lines, after_line,
// before_line) is named in cli_command_note, and so is a filter that
// leaves no signature of the selection; the note is null otherwise.
func TestFindingsRoute_CLICommandCarriesTheFilters(t *testing.T) {
	f := newFindingsFixture(t, nil)
	f.scanned(t, "default")
	base := "rx trace " + f.log + " --signatures="
	cases := []struct {
		name     string
		query    []string
		wantCmd  string
		wantNote string
	}{
		{"no filter", nil, base + "default", ""},
		{"group", []string{"group", "severity"}, base + f.builtin + ",err-line", ""},
		{"two groups", []string{"group", "traceback", "group", "secret"}, base + "py-trace,pem-key", ""},
		{"signature", []string{"signature", "err-line"}, base + "err-line", ""},
		{"signature and group that agree", []string{"signature", "err-line", "group", "severity"}, base + "err-line", ""},
		{"signature outside the selection", []string{"signature", "opt-sql"}, base + "default",
			"signature=opt-sql keeps no signature of the selection: the command lists the whole selection"},
		{"q", []string{"q", "timeout"}, base + "default",
			"q has no command-line flag: the command lists more findings than this answer"},
		{"q and a group", []string{"group", "severity", "q", "timeout"}, base + f.builtin + ",err-line",
			"q has no command-line flag: the command lists more findings than this answer"},
		{"lines and after_line", []string{"lines", "5-9", "after_line", "4"}, base + "default",
			"lines, after_line have no command-line flag: the command lists more findings than this answer"},
		{"text with before", []string{"text", "true", "before", "2"}, "rx trace " + f.log + " --before=2 --signatures=default", ""},
	}
	for _, tc := range cases {
		got := f.getFindings(t, tc.query...)
		if got.CLICommand != tc.wantCmd {
			t.Errorf("%s: cli_command %q, want %q", tc.name, got.CLICommand, tc.wantCmd)
		}
		note := ""
		if got.CLICommandNote != nil {
			note = *got.CLICommandNote
		}
		if note != tc.wantNote {
			t.Errorf("%s: cli_command_note %q, want %q", tc.name, note, tc.wantNote)
		}
	}
	// The narrowed command selects the signatures of the filtered
	// findings and no other: resolving it gives exactly those ids.
	got := f.getFindings(t, "group", "severity")
	selection := strings.TrimPrefix(got.CLICommand, base)
	resolved, err := f.cat.Resolve(selection)
	if err != nil {
		t.Fatalf("resolve %q: %v", selection, err)
	}
	ids := map[string]bool{}
	for _, s := range resolved {
		ids[s.ID] = true
	}
	for _, finding := range got.Findings {
		if !ids[finding.Signature] {
			t.Errorf("finding of %s is not in the command's selection %q", finding.Signature, selection)
		}
	}
	if len(ids) != 2 {
		t.Errorf("the command's selection %q resolves to %d signatures, want the 2 of group severity", selection, len(ids))
	}
}
