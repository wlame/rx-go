package sigscan

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"testing"
)

// benchCatalogue is a selection like a default scan's: keyword-gated
// regexes (some noisy), blocks, a redacting key, and the runs that see
// every line.
var benchCatalogue = sigFile(
	regexSig("error-line", `\b(?:ERROR|FATAL|CRITICAL)\b`),
	regexSig("sql-statement", `(?i)\b(?:select|insert|update|delete)\b.+\b(?:from|into|set)\b`),
	regexSig("connection-failure", `(?i)connection (?:refused|reset|timed out)`),
	regexSig("api-key", `key=(AKIA[A-Z0-9]{16})`, "capture: 1", "redact: true"),
	regexSig("http-5xx", `" 5\d\d \d+`),
	kindSig("tb", "block", `start: '^Traceback \(most recent call last\):'`, `continue: '^\s'`, `end: '^[A-Za-z_][\w.]*(?::|$)'`),
	kindSig("java", "block", `start: '^\S*(?:Exception|Error)(?::|$)'`, `continue: '^\s+(?:at |\.\.\. \d+ more|Caused by:)'`),
	kindSig("blanks", "run", "blank: true", "min: 3"),
	kindSig("repeats", "run", "same: true", "min: 5"),
)

// benchIDs are the ids of benchCatalogue.
var benchIDs = []string{"error-line", "sql-statement", "connection-failure", "api-key", "http-5xx", "tb", "java", "blanks", "repeats"}

// benchMeasures are the nine measures with their defaults.
var benchMeasures = sigFile(
	measureSig("long", "long-line", ""), measureSig("bin", "binary", ""), measureSig("ent", "entropy", ""),
	measureSig("json", "json-block", ""), measureSig("bare", "unprefixed", ""), measureSig("b64", "base64-run", ""),
	measureSig("burst", "density", ""), measureSig("gap", "time-gap", ""), measureSig("back", "time-backwards", ""),
)

// benchMeasureIDs are the ids of benchMeasures.
var benchMeasureIDs = []string{"long", "bin", "ent", "json", "bare", "b64", "burst", "gap", "back"}

// writeBenchLog writes about size bytes of a synthetic log (timestamped
// lines at mixed levels, some SQL and HTTP lines, a traceback now and
// then, blank and repeated runs) and returns its path and line count.
func writeBenchLog(b *testing.B, size int) (string, int) {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.log")
	r := rand.New(rand.NewPCG(1, 2))
	lines := 0
	writePlainB(b, path, func(w io.Writer) {
		for written := 0; written < size; {
			var n int
			switch k := r.IntN(1000); {
			case k < 600:
				n, _ = fmt.Fprintf(w, "2026-01-02 10:%02d:%02d.%03d INFO worker-%d request handled in %d ms\n", r.IntN(60), r.IntN(60), r.IntN(1000), r.IntN(32), r.IntN(900))
			case k < 700:
				n, _ = fmt.Fprintf(w, "2026-01-02 10:%02d:%02d.%03d ERROR worker-%d upstream error code=%d\n", r.IntN(60), r.IntN(60), r.IntN(1000), r.IntN(32), r.IntN(600))
			case k < 800:
				n, _ = fmt.Fprintf(w, "2026-01-02 10:%02d:%02d.%03d DEBUG select id, name from users where id = %d\n", r.IntN(60), r.IntN(60), r.IntN(1000), r.IntN(1e6))
			case k < 900:
				n, _ = fmt.Fprintf(w, "10.0.%d.%d - - [02/Jan/2026:10:00:00 +0000] \"GET /api/v1/items/%d HTTP/1.1\" %d %d\n", r.IntN(256), r.IntN(256), r.IntN(1e5), 200+r.IntN(4)*100, r.IntN(1e4))
			case k < 990:
				n, _ = fmt.Fprintf(w, "2026-01-02 10:00:00.000 WARN update of item %d failed: connection reset by peer\n", r.IntN(1e5))
			case k < 995:
				n, _ = io.WriteString(w, "Traceback (most recent call last):\n  File \"app.py\", line 3, in <module>\n    main()\nValueError: bad input\n")
			default:
				n, _ = io.WriteString(w, "\n\n\n\nsame\nsame\nsame\nsame\nsame\n")
			}
			written += n
			lines++
		}
	})
	return path, lines
}

// writePlainB is writePlain for a benchmark.
func writePlainB(b *testing.B, path string, body func(io.Writer)) {
	b.Helper()
	t := &testing.T{}
	writePlain(t, path, body)
	if t.Failed() {
		b.Fatal("cannot write the benchmark log")
	}
}

// BenchmarkScanMeasures measures the pass with the nine measures beside
// the signatures of BenchmarkScan, on the same log, whose timestamps
// fall anywhere in one hour: about half of its stamped lines step back,
// so time-backwards stores its cap in every chunk.
func BenchmarkScanMeasures(b *testing.B) {
	sigs := append(loadSigs(b, benchCatalogue, benchIDs...), loadSigs(b, benchMeasures, benchMeasureIDs...)...)
	path, lines := writeBenchLog(b, 256<<20)
	file := classify(b, path)
	b.SetBytes(file.Pinned().Info().Size())
	b.ReportAllocs()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for b.Loop() {
		if _, err := scanFile(context.Background(), file, sigs, Options{}, defaultTuning()); err != nil {
			b.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(after.Mallocs-before.Mallocs)/float64(b.N)/float64(lines), "allocs/line")
}

// BenchmarkScan measures the one Go pass on a generated 256 MB log:
// MB/s, and allocations per line. The chunk sub-benchmarks compare
// about 8 chunks per worker with 4 (a 1 MiB minimum chunk, so the
// count differs on this size).
func BenchmarkScan(b *testing.B) {
	sigs := loadSigs(b, benchCatalogue, benchIDs...)
	path, lines := writeBenchLog(b, 256<<20)
	file := classify(b, path)
	size := file.Pinned().Info().Size()
	cases := []struct {
		name string
		tune func(*tuning)
	}{
		{"default", func(*tuning) {}},
		{"8-chunks-per-worker", func(t *tuning) { t.chunksPerWorker, t.minChunkBytes = 8, 1<<20 }},
		{"4-chunks-per-worker", func(t *tuning) { t.chunksPerWorker, t.minChunkBytes = 4, 1<<20 }},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			tune := defaultTuning()
			tc.tune(&tune)
			b.SetBytes(size)
			b.ReportAllocs()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for b.Loop() {
				if _, err := scanFile(context.Background(), file, sigs, Options{}, tune); err != nil {
					b.Fatal(err)
				}
			}
			runtime.ReadMemStats(&after)
			b.ReportMetric(float64(after.Mallocs-before.Mallocs)/float64(b.N)/float64(lines), "allocs/line")
		})
	}
}
