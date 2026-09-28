package index

// The per-line cost of an index build's time section, measured on lines
// whose timestamps behave like those of a log that many threads write:
// a run of lines in one millisecond, a rise, now and then a small step
// back. A constant timestamp would hide the cost of any branch on the
// running maximum, which a branch predictor learns at once on such
// input and cannot learn on these lines.
//
// Compare a change with benchstat, as AGENTS.md ("Benchmarks") says:
//
//	go test ./internal/index/ -run='^$' -bench=Index -count=10

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/timestamps"
)

// The shares of the lines of a threaded log, in percent: a line sets a
// new highest millisecond (risePercent), or repeats the highest one
// (repeatPercent), or steps back below it (the rest, 1%). They are those
// of a real application log written by a pool of request threads: about
// 40% of its lines set a new high and 59% repeat it.
const (
	risePercent   = 40
	repeatPercent = 59
	// maxRiseMs is the largest rise, and maxStepBackMs the largest step
	// back. A step back of more than backwardStepMs counts as a backward
	// step, so about half of them do.
	maxRiseMs     = 5
	maxStepBackMs = 2 * backwardStepMs
)

// threadedLogStart is the first timestamp of a generated threaded log.
var threadedLogStart = time.Date(2025, 12, 10, 7, 0, 4, 574_000_000, time.UTC)

// threadedLogStamps returns n timestamps in ms, in line order, as a log
// that many threads write has them (see risePercent): each line rises
// above the highest so far, repeats it, or steps back below it, in an
// order a seeded generator chooses, so every run sees the same lines.
//
// Go note: math/rand/v2 with a PCG source of a fixed seed gives the same
// sequence on every run and every platform.
func threadedLogStamps(n int, seed uint64) []int64 {
	random := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	stamps := make([]int64, n)
	highest := threadedLogStart.UnixMilli()
	for i := range stamps {
		switch roll := random.IntN(100); {
		case roll < risePercent:
			highest += 1 + random.Int64N(maxRiseMs)
			stamps[i] = highest
		case roll < risePercent+repeatPercent:
			stamps[i] = highest
		default:
			stamps[i] = highest - 1 - random.Int64N(maxStepBackMs)
		}
	}
	return stamps
}

// threadedLogLines writes each timestamp as a line of an application
// log with ISO timestamps to the millisecond, its line break included:
// `2025-12-10 07:00:04.574 INFO [worker-3] c.e.Gateway - request served
// in 12 ms`.
func threadedLogLines(stamps []int64) [][]byte {
	lines := make([][]byte, len(stamps))
	for i, ms := range stamps {
		at := time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05.000")
		lines[i] = fmt.Appendf(nil, "%s INFO [worker-%d] c.e.Gateway - request served in %d ms\n", at, i%16, 3+i%40)
	}
	return lines
}

// threadedLogLineCount is how many lines one benchmark pass reads: 64Ki
// lines, about 5.5 MB of text.
const threadedLogLineCount = 1 << 16

// isoIndexer is a time indexer for ISO timestamps anchored at the start
// of the line, as an index build of such a file makes it.
func isoIndexer(tb testing.TB) *timeIndexer {
	tb.Helper()
	ti, err := newTimeIndexer(timestamps.Format{Family: timestamps.FamilyISO, Anchored: true}, 0)
	if err != nil {
		tb.Fatalf("newTimeIndexer: %v", err)
	}
	return ti
}

// reportNsPerLine reports the benchmark's time per line read, as
// ns/line, for a benchmark whose every iteration read linesPerOp lines.
// b.N holds the number of iterations once the b.Loop loop has ended.
func reportNsPerLine(b *testing.B, linesPerOp int) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*linesPerOp), "ns/line")
}

// The generator writes the shares it is named for, and observe keeps
// the running maximum and counts the backward steps over its lines as
// a direct count over the timestamps does.
func TestThreadedLogStamps_RiseRepeatAndStepBackLikeAThreadedLog(t *testing.T) {
	stamps := threadedLogStamps(threadedLogLineCount, 1)
	rises, repeats, backward := 0, 0, int64(0)
	highest := stamps[0]
	for _, ms := range stamps[1:] {
		switch {
		case ms > highest:
			rises++
			highest = ms
		case ms == highest:
			repeats++
		case highest-ms > backwardStepMs:
			backward++
		}
	}
	share := func(count int) float64 { return 100 * float64(count) / float64(len(stamps)) }
	if r, p := share(rises), share(repeats); r < risePercent-2 || r > risePercent+2 || p < repeatPercent-2 || p > repeatPercent+2 {
		t.Fatalf("%.1f%% rises and %.1f%% repeats, want about %d%% and %d%%", r, p, risePercent, repeatPercent)
	}

	ti := isoIndexer(t)
	var offset int64
	for i, line := range threadedLogLines(stamps) {
		ti.observe(line, int64(i+1), offset, offset+int64(len(line)))
		offset += int64(len(line))
	}
	if ti.maxAt.Ms != highest || ti.backwardSteps != backward || backward == 0 {
		t.Fatalf("observe: highest %d, %d backward steps; want %d and %d (more than 0)",
			ti.maxAt.Ms, ti.backwardSteps, highest, backward)
	}
}

// BenchmarkTimeIndexerObserve is the cost the time section adds to each
// line of an index build, on a threaded log's timestamps, in ns/line.
// Each iteration reads every line with a new indexer, as one build does,
// so the running maximum starts low and moves as it does in a file.
func BenchmarkTimeIndexerObserve(b *testing.B) {
	lines := threadedLogLines(threadedLogStamps(threadedLogLineCount, 1))
	b.ReportAllocs()
	for b.Loop() {
		ti := isoIndexer(b)
		var offset int64
		for i, line := range lines {
			ti.observe(line, int64(i+1), offset, offset+int64(len(line)))
			offset += int64(len(line))
		}
	}
	reportNsPerLine(b, len(lines))
}

// BenchmarkIndexBuild_ThreadedLog is the cost of a whole index build of a
// plain file of a threaded log's lines, in ns/line: the walk, the line
// statistics and the time section together.
func BenchmarkIndexBuild_ThreadedLog(b *testing.B) {
	lines := threadedLogLines(threadedLogStamps(threadedLogLineCount, 1))
	path := filepath.Join(b.TempDir(), "app.log")
	if err := os.WriteFile(path, bytes.Join(lines, nil), 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Build(path, BuildOptions{}); err != nil {
			b.Fatalf("Build: %v", err)
		}
	}
	reportNsPerLine(b, len(lines))
}
