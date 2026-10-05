package index

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/wlame/rx-go/internal/filetext"
	"github.com/wlame/rx-go/internal/journal"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// errNoJournalReport is journalSection's error for a text that is not a
// journal's: a caller that asks for the section of any other file has a
// bug.
var errNoJournalReport = errors.New("the text read is not a journal's, so it has no journal report")

// journalSection returns the journal section of the index of src, a
// systemd journal whose text the build has read to its end through text:
// the reader's report of that read, and the facts of the journal's
// header.
//
// The report is complete only once the text's Read returned io.EOF,
// which the walk of the text does before the build gets here. The header
// is read again, through the handle the pin opened: the reader read it
// when the text was opened, and keeps it to itself. It is one read of at
// most 272 bytes, outside the build's progress, which has ended.
func journalSection(src openedSource, text *filetext.Text) (*rxtypes.JournalInfo, error) {
	report, ok := text.JournalReport()
	if !ok {
		return nil, errNoJournalReport
	}
	header, err := journal.ReadHeader(src.file, src.info.Size())
	if err != nil {
		return nil, fmt.Errorf("read the journal's header: %w", err)
	}
	return journalInfoOf(report, header), nil
}

// journalInfoOf is the journal section that report, the reader's report
// of a full read of a journal's text, and header, the journal's header,
// give: the counts, the first damage (null when there is none), and the
// header's state, mode, counter id and first and last sequence numbers.
func journalInfoOf(report journal.Report, header journal.Header) *rxtypes.JournalInfo {
	info := &rxtypes.JournalInfo{
		Entries:        report.Entries,
		Shown:          report.Shown,
		WithoutMessage: report.WithoutMessage,
		LeftOut:        report.LeftOut,
		NotReached:     report.NotReached,
		State:          header.State.String(),
		Compact:        header.Compact,
		// The id is 16 bytes; hex.EncodeToString writes each byte as
		// two lowercase hex digits, 32 in all.
		SeqnumID:   hex.EncodeToString(header.SeqnumID[:]),
		HeadSeqnum: header.HeadSeqnum,
		TailSeqnum: header.TailSeqnum,
	}
	// The report says "no damage" with a 0 entry number and an empty
	// reason; the index says it with null.
	if report.FirstDamaged != 0 {
		info.FirstDamagedEntry = ptrInt64(report.FirstDamaged)
	}
	if report.DamageReason != "" {
		info.DamageReason = ptrString(report.DamageReason)
	}
	return info
}
