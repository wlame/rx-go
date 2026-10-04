package main

import (
	"encoding/binary"
	"flag"
	"os"
)

// runTrim writes a copy of a journal cut to its used size.
//
// journald and journal-remote grow a journal in 8 MiB steps, so a journal
// with a few entries is 8 MiB of mostly zero bytes. The copy ends after
// the tail object, and its header's arena_size says so; journalctl reads
// it as it reads the original.
func runTrim(args []string) error {
	fs := flag.NewFlagSet("trim", flag.ContinueOnError)
	in := fs.String("in", "", "journal to read")
	out := fs.String("out", "", "file to write the trimmed copy to")
	if err := parseFlags(fs, args, "in", "out"); err != nil {
		return err
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	trimmed, err := trimJournal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, trimmed, 0o600) // #nosec G703 -- the path is this program's own argument, given by make.sh
}

// trimJournal returns data cut after its tail object, with arena_size set
// to the bytes between the end of the header and that cut.
func trimJournal(data []byte) ([]byte, error) {
	h, err := readHeader(data)
	if err != nil {
		return nil, err
	}
	end, err := usedEnd(data, h)
	if err != nil {
		return nil, err
	}
	trimmed := append([]byte(nil), data[:end]...)
	binary.LittleEndian.PutUint64(trimmed[arenaSizeOffset:], end-h.headerSize)
	return trimmed, nil
}
