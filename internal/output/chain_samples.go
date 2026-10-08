package output

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// FormatChainSamples renders a chain samples answer for
// `rx logs samples`: a head with the chain, its state, part count and
// fingerprint, its time span (ready chains) in the parts' layout
// (times), and the context; then one block per key, in the order a
// person reads them (by line, a time query by the line it found), each
// line with its global number (`?` before the chain is ready), its part
// and its own number in the part, and a `-- NAME --` line where a piece
// starts.
//
//	Chain: /var/log/syslog  ready  8 parts  fingerprint e8ed018e0ff41f34
//	Times: 2026-09-29 00:00:01 .. 2026-10-06 22:11:00  UTC
//	Context: 3 before, 3 after
//
//	=== /var/log/syslog:43418 ===
//	-- syslog-20261002-1790899200.gz --
//	43415  syslog-20261002-1790899200.gz:10711  …
//	43418  syslog-20261002-1790899200.gz:10714  …
//	-- syslog-20261003-1790985600.gz --
//	43419  syslog-20261003-1790985600.gz:1  …
//
// part is the request's part, "" for global lines: a key then numbers
// that part's lines, and its head says so (`=== HANDLE PART:500 ===`).
func FormatChainSamples(resp *rxtypes.ChainSamplesResponse, part string, times ChainTimes, zone string) string {
	var b strings.Builder
	handle := Printable(resp.Path)
	fmt.Fprintf(&b, "Chain: %s  %s  %s  fingerprint %s\n", handle, resp.State, plural(len(resp.Parts), "part"), resp.Fingerprint)
	if first, last, ok := chainSpan(resp.Parts); ok && resp.State == rxtypes.ChainStateReady {
		fmt.Fprintf(&b, "Times: %s .. %s  %s\n", times.Format(first), times.Format(last), Printable(zone))
	}
	fmt.Fprintf(&b, "Context: %d before, %d after\n", resp.BeforeContext, resp.AfterContext)
	keys := sortedSampleKeys(resp.Samples)
	if len(resp.Timestamps) > 0 {
		keys = SortedTimeQueries(resp.Timestamps)
	}
	for _, key := range keys {
		b.WriteByte('\n')
		b.WriteString(chainBlockHead(resp, handle, part, key))
		b.WriteByte('\n')
		writeChainPieces(&b, resp.Samples[key])
	}
	return b.String()
}

// chainBlockHead is the line that heads a key's block.
func chainBlockHead(resp *rxtypes.ChainSamplesResponse, handle, part, key string) string {
	switch {
	case len(resp.Timestamps) > 0:
		line := resp.Timestamps[key]
		position := strconv.FormatInt(line, 10)
		if lines := countLines(resp.Samples[key]); strings.Contains(key, "..") && line > 0 && lines > 1 {
			position = fmt.Sprintf("%d-%d", line, line+int64(lines)-1)
		}
		return fmt.Sprintf("=== %s:%s @ %s ===", handle, position, Printable(key))
	case part != "":
		return fmt.Sprintf("=== %s %s:%s ===", handle, Printable(part), key)
	}
	return fmt.Sprintf("=== %s:%s ===", handle, key)
}

// writeChainPieces writes the lines of a key's pieces, each with its
// global number, right-aligned in the block, and its part's own.
func writeChainPieces(b *strings.Builder, pieces []rxtypes.ChainPiece) {
	width := 1
	for _, p := range pieces {
		if p.FirstGlobalLine > 0 {
			width = max(width, len(strconv.FormatInt(p.FirstGlobalLine+int64(len(p.Lines))-1, 10)))
		}
	}
	for _, p := range pieces {
		name := Printable(p.Part)
		fmt.Fprintf(b, "-- %s --\n", name)
		for i, line := range p.Lines {
			global := "?"
			if p.FirstGlobalLine > 0 {
				global = strconv.FormatInt(p.FirstGlobalLine+int64(i), 10)
			}
			fmt.Fprintf(b, "%*s  %s:%d  %s\n", width, global, name, p.FirstLocalLine+int64(i), line)
		}
	}
}

// chainSpan is the chain's first and last timestamp: the first of its
// first part with lines and the last of its last, when both are known.
func chainSpan(parts []rxtypes.ChainPart) (first, last int64, ok bool) {
	var firstMs, lastMs *int64
	for _, p := range parts {
		if p.LineCount != nil && *p.LineCount == 0 {
			continue
		}
		if firstMs == nil {
			firstMs = p.FirstMs
		}
		lastMs = p.LastMs
	}
	if firstMs == nil || lastMs == nil {
		return 0, 0, false
	}
	return *firstMs, *lastMs, true
}

// countLines is the number of lines a key's pieces hold.
func countLines(pieces []rxtypes.ChainPiece) int {
	n := 0
	for _, p := range pieces {
		n += len(p.Lines)
	}
	return n
}

// plural writes a count with its noun, plural when it is not one.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
