package timestamps

import "testing"

// ownBenchLines holds one realistic line per family, each with its
// format, for the allocation guard and the benchmark.
var ownBenchLines = []struct {
	format Format
	line   string
}{
	{isoAnchored, "2025-12-10 07:00:04.574 [1765375204574] [Worker-3] INFO JobRunner  Waiting for the next batch"},
	{clfWindowed, `203.0.113.7 - - [06/Oct/2026:12:34:56 +0000] "GET /index.html HTTP/1.1" 200 5120 "-" "curl/8.0"`},
	{ctimeAnchored, "[Tue Oct 06 12:34:56.123456 2026] [core:error] [pid 4242] AH00037: Symbolic link not allowed"},
	{syslogAnchored, "Dec 10 07:00:12.156 7A3F09C21B5E I      job.scheduler.retry Retrying task (4210, 17, 30)"},
	{slashMonthFirst, "10/06/2026 12:34:56 PM INFO [worker-7] job 42 finished in 1.2 s"},
	{dottedAnchored, "06.10.2026 12:34:56,789 INFO  [main] de.example.App - started"},
	{epochAnchored, "1696600000.123 level=info msg=\"request served\" status=200"},
	// A line with no timestamp makes a windowed scan try every position.
	{isoWindowed, "\tQuery Text: select \"derived\".\"item_id\", \"derived\".\"order_total\", \"derived\".\"item_name\" from x"},
}

func BenchmarkOwn(b *testing.B) {
	for _, bl := range ownBenchLines {
		name := string(bl.format.Family)
		if !bl.format.Anchored {
			name += "-windowed"
		}
		b.Run(name, func(b *testing.B) {
			p := mustParser(b, bl.format, mtime2025)
			line := []byte(bl.line)
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.Own(line)
			}
		})
	}
}
