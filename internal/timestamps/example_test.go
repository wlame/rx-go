package timestamps_test

import (
	"fmt"
	"time"

	"github.com/wlame/rx-go/internal/timestamps"
)

// A postgres log: few lines carry a timestamp, but those that do start
// with it, and its zone word MST is not one the package converts.
func ExampleDetect() {
	sample := []byte("2025-12-10 07:00:30 MST [4242]: LOG:  duration: 175.200 ms  plan:\n" +
		"\tQuery Text: select 1\n" +
		"2025-12-10 07:00:31 MST [4242]: LOG:  duration: 0.042 ms\n" +
		"\tQuery Text: select 2\n" +
		"2025-12-10 07:00:32 MST [4242]: LOG:  checkpoint starting\n")
	f, ok := timestamps.Detect(sample)
	fmt.Println(ok, f.Family, f.Anchored, f.HasZone)
	// Output: true iso true false
}

// A syslog line has no year: the Parser takes it from the file's mtime.
func ExampleParser_Own() {
	mtime := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	p, err := timestamps.NewParser(timestamps.Format{Family: timestamps.FamilySyslog, Anchored: true}, mtime.UnixNano())
	if err != nil {
		panic(err)
	}
	for _, line := range []string{
		"Dec 31 23:59:58.250 host app[1]: last line of the year",
		"Jan  1 00:00:01 host app[1]: first line of the next",
		"\tat com.example.Main.run(Main.java:42)",
	} {
		s, ok := p.Own([]byte(line))
		fmt.Println(ok, time.UnixMilli(s.Ms).UTC().Format("2006-01-02 15:04:05.000"), s.Zoned)
	}
	// Output:
	// true 2025-12-31 23:59:58.250 false
	// true 2026-01-01 00:00:01.000 false
	// false 1970-01-01 00:00:00.000 false
}

// A time-only range takes its date from the file, and a query without a
// zone is read in the file's own frame unless a query zone is given.
func ExampleResolve() {
	q, err := timestamps.ParseQuery("14:33:12..14:35", nil)
	if err != nil {
		panic(err)
	}
	ctx := timestamps.ResolveContext{
		FirstMs: time.Date(2025, 12, 10, 7, 0, 0, 0, time.UTC).UnixMilli(),
		LastMs:  time.Date(2025, 12, 10, 20, 0, 0, 0, time.UTC).UnixMilli(),
		HasSpan: true,
	}
	fmt.Println(q.NeedsSpan())
	r, err := timestamps.Resolve(q, ctx)
	if err != nil {
		panic(err)
	}
	show := func(b timestamps.Bound) string {
		return time.UnixMilli(b.Ms).UTC().Format("2006-01-02 15:04:05.000")
	}
	fmt.Println(r.Range, show(r.Start), show(r.End))

	ctx.QueryZone = time.FixedZone("+02:00", 2*3600)
	r, err = timestamps.Resolve(q, ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(r.Range, show(r.Start), show(r.End))
	// Output:
	// true
	// true 2025-12-10 14:33:12.000 2025-12-10 14:35:00.000
	// true 2025-12-10 12:33:12.000 2025-12-10 12:35:00.000
}
