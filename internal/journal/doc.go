// Package journal reads systemd journal files: the binary files
// (signature "LPKSHHRH") that systemd-journald writes, such as
// system.journal and its system@….journal archives, regular or compact,
// with values stored plain or compressed with xz, lz4 or zstd.
//
// rx works on a journal's text, as it works on the decompressed text of
// a gzip file. The text of a journal is, entry by entry, exactly what
//
//	journalctl --file=F --no-pager --utc -q -o short-iso-precise
//
// of systemd 257 prints for it with stdout not a terminal and no
// SYSTEMD_COLORS or SYSTEMD_URLIFY in the environment: one line per
// entry, or more for a multi-line message, such as
//
//	2026-01-15T08:00:01.000000+00:00 fixture-host demo-app[4101]: basic 1: service started
//
// An entry without a MESSAGE field is not shown. Line numbers and byte
// offsets of every rx answer about a journal refer to this text.
//
// NewTextReader gives the text as an io.Reader, entry by entry in the
// order of the file's entry list, holding one entry's text at a time.
// A journal may be damaged (a truncated copy, a corrupt object): an entry
// that cannot be read whole is left out, a broken entry list ends the
// text there, and the reader's Report counts both, so the text is still a
// fixed function of the file's bytes. ReadHeader reads the header alone;
// a header rx cannot read is refused with ErrUnreadable.
//
// testdata/ holds invented journals and the text journalctl prints for
// them; testdata/gen writes them (`just journal-fixtures`, which needs
// Docker).
package journal
