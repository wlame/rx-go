package timestamps

import (
	"fmt"
	"strings"
	"testing"
)

// sample joins lines into a sample text, each line ending with a newline.
func sample(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// repeat returns n lines made by fn(i).
func repeat(n int, fn func(i int) string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fn(i)
	}
	return out
}

func concat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// isoWindowedZoned is the format of JSON lines whose times end in `Z`.
var isoWindowedZoned = Format{Family: FamilyISO, HasZone: true}

// detectCase is one sample and the format Detect must choose for it.
type detectCase struct {
	name   string
	sample []byte
	want   Format
	ok     bool
}

// slashLines returns n slash-dated lines whose first two numbers are
// first and second+i, so a caller can make lines that read in one order
// only (a number above 12 on one side) or in both.
func slashLines(n, first, second int, text string) []string {
	return repeat(n, func(i int) string {
		return fmt.Sprintf("%02d/%02d/2026 12:00:%02d %s", first, second+i, i%60, text)
	})
}

// jsonWithSyslog is n JSON lines that carry an ISO time after a key, then
// three lines that start with a syslog time.
func jsonWithSyslog(n int) []string {
	return concat(
		repeat(n, func(i int) string {
			return fmt.Sprintf(`{"level":"info","ts":"2026-10-06T12:34:56.%03dZ","msg":"request %d"}`, i%1000, i)
		}),
		repeat(3, func(i int) string { return fmt.Sprintf("Oct  6 12:34:5%d injected", i) }),
	)
}

// detectCases are the samples TestDetect checks; FuzzDetect starts from
// them too.
func detectCases() []detectCase {
	// A postgres log: 4 timestamped lines among 81, the rest are
	// tab-led query-plan lines, one of which quotes a date.
	postgres := concat(
		repeat(4, func(i int) string {
			return fmt.Sprintf("2025-12-10 07:00:3%d MST [216895]: [155-1] user=sa,db=mw LOG:  duration: 182.491 ms  plan:", i)
		}),
		repeat(77, func(i int) string {
			if i == 5 {
				return "\t  Filter: (created_at > '2025-12-01 00:00:00'::timestamp)"
			}
			return fmt.Sprintf("\t  ->  Seq Scan on t%d  (cost=0.00..35.50 rows=2550 width=4)", i)
		}),
	)
	// The middleware shape: an ISO timestamp at column 0 and the same
	// moment as epoch milliseconds in brackets on every line.
	middleware := repeat(50, func(i int) string {
		return fmt.Sprintf("2025-12-10 07:00:04.%03d [1765375204%03d] [PatchStream] INFO Conn  Waiting", i, i)
	})
	jsonLines := repeat(10, func(i int) string {
		if i == 3 {
			return `{"level":"info","msg":"renewed","expires":"2026-10-06T12:34:56Z"}`
		}
		return fmt.Sprintf(`{"level":"info","msg":"request %d served"}`, i)
	})
	accessLog := repeat(20, func(i int) string {
		return fmt.Sprintf(`203.0.113.%d - - [06/Oct/2026:12:34:%02d +0000] "GET / HTTP/1.1" 200 512`, i, i)
	})
	isoAndEpochTie := concat(
		repeat(3, func(i int) string { return fmt.Sprintf("2026-10-06 12:34:5%d x", i) }),
		repeat(3, func(i int) string { return fmt.Sprintf("169660000%d x", i) }),
	)
	slashMonthFirstLines := repeat(5, func(i int) string { return fmt.Sprintf("10/0%d/2026 12:34:56 PM x", i+1) })
	oneDayFirstLine := append(append([]string{}, slashMonthFirstLines...), "13/10/2026 12:34:56 x")
	// 200 lines that read either way, and one written to read day first
	// only: one line must not turn every other date around.
	injectedDayFirst := concat(
		repeat(200, func(int) string { return "10/06/2026 12:00:00 GET /index" }),
		[]string{"13/01/2026 12:00:00 injected"},
	)
	twoDayFirstOnly := concat(slashLines(5, 1, 2, "x"), slashLines(2, 13, 1, "x"))
	threeDayFirstOnly := concat(slashLines(5, 1, 2, "x"), slashLines(3, 13, 1, "x"))
	dayFirstReadsMore := concat(slashLines(3, 1, 13, "x"), slashLines(4, 13, 1, "x"))
	orderTie := concat(slashLines(4, 1, 13, "x"), slashLines(4, 13, 1, "x"))
	monthFirstReadsMore := concat(slashLines(4, 1, 13, "x"), slashLines(3, 13, 1, "x"))
	// A few column-0 lines among JSON lines that each carry a time further
	// in: below 1% of the lines, the column-0 lines do not decide the file.
	jsonPlusSyslog := jsonWithSyslog(3000)
	syslogAtOnePercent := jsonWithSyslog(297)
	syslogBelowOnePercent := jsonWithSyslog(298)
	// With no windowed reading to prefer, three column-0 lines still make
	// a log, whatever their share.
	fewSyslogAmongText := concat(
		repeat(3, func(i int) string { return fmt.Sprintf("Oct  6 12:34:5%d host x", i) }),
		repeat(400, func(i int) string { return fmt.Sprintf("plain text line %d", i) }),
	)
	pureSyslog := repeat(50, func(i int) string { return fmt.Sprintf("Dec 10 07:49:%02d.123 host app[42]: x", i) })
	// Three syslog lines at column 0 beat ten lines with a CLF time
	// further in.
	anchoredBeatsWindowed := concat(
		repeat(3, func(i int) string { return fmt.Sprintf("Oct  6 12:34:5%d host x", i) }),
		repeat(10, func(i int) string { return fmt.Sprintf(`h - - [06/Oct/2026:12:34:%02d +0000] "GET /"`, i) }),
	)
	windowedHalf := concat(
		repeat(5, func(i int) string { return fmt.Sprintf("id=%d at 2026-10-06 12:34:5%d", i, i) }),
		repeat(5, func(i int) string { return fmt.Sprintf("id=%d nothing", i) }),
	)
	windowedBelowHalf := concat(
		repeat(4, func(i int) string { return fmt.Sprintf("id=%d at 2026-10-06 12:34:5%d", i, i) }),
		repeat(6, func(i int) string { return fmt.Sprintf("id=%d nothing", i) }),
	)
	// Blank lines do not count towards the half.
	windowedWithBlanks := concat(windowedBelowHalf[:4], []string{"", "\r", "  \t", "id=x nothing", "id=y nothing", "id=z nothing"})
	zonedMajority := concat(
		repeat(3, func(i int) string { return fmt.Sprintf("2026-10-06T12:34:5%dZ x", i) }),
		repeat(2, func(i int) string { return fmt.Sprintf("2026-10-06 12:34:5%d x", i) }),
	)
	zonedHalf := concat(
		repeat(2, func(i int) string { return fmt.Sprintf("2026-10-06T12:34:5%dZ x", i) }),
		repeat(2, func(i int) string { return fmt.Sprintf("2026-10-06 12:34:5%d x", i) }),
	)
	crlf := repeat(5, func(i int) string { return fmt.Sprintf("Dec 10 07:49:5%d.123 x\r", i) })

	return []detectCase{
		{"postgres: few anchored lines among tab-led ones", sample(postgres...), isoAnchored, true},
		{"middleware: anchored iso beats windowed epoch", sample(middleware...), isoAnchored, true},
		{"JSON lines with one date: none", sample(jsonLines...), Format{}, false},
		{"access log: clf windowed", sample(accessLog...), clfWindowed, true},
		{"tie goes to table order", sample(isoAndEpochTie...), isoAnchored, true},
		{"slash month first", sample(slashMonthFirstLines...), slashMonthFirst, true},
		{"slash: one day-first line keeps month first", sample(oneDayFirstLine...), slashMonthFirst, true},
		{"slash: one injected line among 200 keeps month first", sample(injectedDayFirst...), slashMonthFirst, true},
		{"slash: two day-first-only lines keep month first", sample(twoDayFirstOnly...), slashMonthFirst, true},
		{"slash: three day-first-only lines and no month-first-only line", sample(threeDayFirstOnly...), slashDayFirst, true},
		{"slash: day first reads more lines", sample(dayFirstReadsMore...), slashDayFirst, true},
		{"slash: a tie reads month first", sample(orderTie...), slashMonthFirst, true},
		{"slash: month first reads more lines", sample(monthFirstReadsMore...), slashMonthFirst, true},
		{"anchored beats windowed", sample(anchoredBeatsWindowed...), syslogAnchored, true},
		{"three column-0 lines among 3,000 JSON lines: windowed iso", sample(jsonPlusSyslog...), isoWindowedZoned, true},
		{"column-0 lines at 1% beat windowed", sample(syslogAtOnePercent...), syslogAnchored, true},
		{"column-0 lines below 1% lose to windowed", sample(syslogBelowOnePercent...), isoWindowedZoned, true},
		{"column-0 lines below 1% with no windowed reading", sample(fewSyslogAmongText...), syslogAnchored, true},
		{"pure syslog", sample(pureSyslog...), syslogAnchored, true},
		{"windowed at half qualifies", sample(windowedHalf...), isoWindowed, true},
		{"windowed below half: none", sample(windowedBelowHalf...), Format{}, false},
		{"blank lines do not count", sample(windowedWithBlanks...), isoWindowed, true},
		{"anchored needs three lines", sample("2026-10-06 12:34:56 a", "2026-10-06 12:34:57 b", "x", "y", "z"), Format{}, false},
		{"has_zone when most lines carry a zone", sample(zonedMajority...), Format{Family: FamilyISO, Anchored: true, HasZone: true}, true},
		{"no has_zone at exactly half", sample(zonedHalf...), isoAnchored, true},
		{"CRLF lines", sample(crlf...), syslogAnchored, true},
		{"no final newline", []byte("Dec 10 07:49:50 a\nDec 10 07:49:51 b\nDec 10 07:49:52 c"), syslogAnchored, true},
		{"empty", nil, Format{}, false},
	}
}

func TestDetect(t *testing.T) {
	for _, tc := range detectCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Detect(tc.sample)
			if ok != tc.ok || !sameFormat(got, tc.want) {
				t.Fatalf("Detect = %s, %t; want %s, %t", formatString(got), ok, formatString(tc.want), tc.ok)
			}
		})
	}
}

// TestDetect_SampleLimits: lines past SampleLines, and bytes past
// SampleBytes, are not looked at.
func TestDetect_SampleLimits(t *testing.T) {
	stamped := repeat(10, func(i int) string { return fmt.Sprintf("2026-10-06 12:34:5%d x", i) })

	afterLineCap := concat(repeat(SampleLines, func(int) string { return "x" }), stamped)
	if f, ok := Detect(sample(afterLineCap...)); ok {
		t.Errorf("lines after line %d were used: %s", SampleLines, formatString(f))
	}
	beforeLineCap := concat(repeat(SampleLines-3, func(int) string { return "x" }), stamped)
	if _, ok := Detect(sample(beforeLineCap...)); !ok {
		t.Errorf("the last 3 lines within the line cap were not used")
	}

	long := strings.Repeat("y", 1000)
	fillsBytes := repeat(SampleBytes/len(long+"\n")+1, func(int) string { return long })
	if f, ok := Detect(sample(concat(fillsBytes, stamped)...)); ok {
		t.Errorf("lines after byte %d were used: %s", SampleBytes, formatString(f))
	}
}

// TestDetect_LockedSlashOrder: a later line that does not fit the order
// detection locked has no timestamp.
func TestDetect_LockedSlashOrder(t *testing.T) {
	f, ok := Detect(sample("13/10/2026 12:34:56 a", "14/10/2026 12:34:56 b", "15/10/2026 12:34:56 c"))
	if !ok || f.DayFirst == nil || !*f.DayFirst {
		t.Fatalf("Detect = %s, %t; want day-first slash", formatString(f), ok)
	}
	p := mustParser(t, f, mtime2025)
	if s, ok := p.Own([]byte("10/13/2026 12:34:56 d")); ok {
		t.Fatalf("a month-first line in a day-first file read as %+v", s)
	}
}

func sameFormat(a, b Format) bool {
	return formatString(a) == formatString(b)
}

func formatString(f Format) string {
	order := "nil"
	if f.DayFirst != nil {
		order = fmt.Sprint(*f.DayFirst)
	}
	return fmt.Sprintf("{%s anchored=%t day_first=%s has_zone=%t}", f.Family, f.Anchored, order, f.HasZone)
}

func BenchmarkDetect(b *testing.B) {
	// About 1 MiB of postgres-shaped text, mostly tab-led lines that make
	// every windowed candidate scan its whole window.
	var lines []string
	for size := 0; size < SampleBytes; {
		line := fmt.Sprintf("\t  ->  Seq Scan on t%d  (cost=0.00..35.50 rows=2550 width=4) Filter: (x > 42) and (y < 17) and z is not null", size)
		if len(lines)%20 == 0 {
			line = "2025-12-10 07:00:30 MST [216895]: [155-1] user=sa,db=mw LOG:  duration: 182.491 ms  plan:"
		}
		lines = append(lines, line)
		size += len(line) + 1
	}
	s := sample(lines...)
	b.SetBytes(int64(min(len(s), SampleBytes)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Detect(s)
	}
}
