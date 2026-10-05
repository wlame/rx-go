package clicommand

import (
	"fmt"
	"io"

	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// writeJournalHuman prints the journal section of an index for
// `rx index --info`: the header's state, the entries the header counts,
// how many the text shows, how many have no message, how many were left
// out as damaged and not reached, and the first damaged entry with the
// reason. Nothing for a file that is not a journal (j is nil).
func writeJournalHuman(out io.Writer, j *rxtypes.JournalInfo) {
	if j == nil {
		return
	}
	_, _ = fmt.Fprintln(out, "  journal:")
	_, _ = fmt.Fprintf(out, "    state: %s\n", j.State)
	counts := []struct {
		label string
		value int64
	}{
		{"entries", j.Entries},
		{"shown", j.Shown},
		{"without_message", j.WithoutMessage},
		{"left_out", j.LeftOut},
		{"not_reached", j.NotReached},
	}
	for _, c := range counts {
		_, _ = fmt.Fprintf(out, "    %s: %d\n", c.label, c.value)
	}
	_, _ = fmt.Fprintf(out, "    first_damaged_entry: %s\n", firstDamageText(j))
}

// firstDamageText words the first damaged entry of a journal section:
// its number and the reason, or "none".
func firstDamageText(j *rxtypes.JournalInfo) string {
	if j.FirstDamagedEntry == nil {
		return "none"
	}
	reason := ""
	if j.DamageReason != nil {
		reason = *j.DamageReason
	}
	// The reason comes from the file's bytes only through numbers the
	// reader formats; Printable keeps a control byte off the terminal
	// all the same.
	return fmt.Sprintf("%d (%s)", *j.FirstDamagedEntry, output.Printable(reason))
}
