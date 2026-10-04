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
// testdata/ holds invented journals with journalctl's text beside each;
// testdata/gen writes them (`just journal-fixtures`, which needs Docker).
package journal
