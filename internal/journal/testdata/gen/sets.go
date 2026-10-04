package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// fixtureSet is one set of fixture journals: make.sh writes every set in
// every compression and mode.
type fixtureSet struct {
	name    string
	entries func() []entry
}

// fixtureSets are the sets `gen streams` writes, one export stream each.
var fixtureSets = []fixtureSet{
	{"basic", basicEntries},
	{"many", manyEntries},
	{"stamps", stampsEntries},
}

// baseTime is the entry time the sets count from: an invented morning.
var baseTime = uint64(time.Date(2026, time.January, 15, 8, 0, 0, 0, time.UTC).UnixMicro())

// The boots the entries belong to (invented ids).
const (
	bootOne = "11111111111111111111111111111111"
	bootTwo = "22222222222222222222222222222222"
)

const (
	second = uint64(time.Second / time.Microsecond)
	minute = 60 * second
	hour   = 60 * minute
)

// runStreams writes DIR/SET.export for every set.
func runStreams(args []string) error {
	fs := flag.NewFlagSet("streams", flag.ContinueOnError)
	out := fs.String("out", "", "directory to write the export streams into")
	if err := parseFlags(fs, args, "out"); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o750); err != nil {
		return err
	}
	for _, set := range fixtureSets {
		if err := writeStream(filepath.Join(*out, set.name+".export"), set.entries()); err != nil {
			return fmt.Errorf("set %s: %w", set.name, err)
		}
	}
	return nil
}

// writeStream writes entries to a new export file at path.
func writeStream(path string, entries []entry) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	// The deferred function runs when writeStream returns. It closes
	// the file and, through the named result err, reports a failed close
	// when the write itself went well: a close can be the first place a
	// write error shows.
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	return writeExport(f, entries)
}

// clock gives entries their times: each entry it stamps is step
// microseconds after the one before, in realtime and in monotonic time.
type clock struct {
	realtime  uint64
	monotonic uint64
	step      uint64
	boot      string
}

// stamp makes an entry of fields at the clock's time and moves the clock
// on by one step.
func (c *clock) stamp(fields ...field) entry {
	e := entry{realtime: c.realtime, monotonic: c.monotonic, bootID: c.boot, fields: fields}
	c.realtime += c.step
	c.monotonic += c.step
	return e
}

// now is the realtime the next entry stamped gets. It is a method, not a
// read of c.realtime, so that inside one composite literal it runs after
// the stamp calls written before it: Go evaluates function calls left to
// right, but leaves the moment of a plain variable read unspecified.
func (c *clock) now() uint64 { return c.realtime }

// newBoot starts the clock's monotonic time again, for a new boot.
func (c *clock) newBoot(boot string, monotonic uint64) {
	c.boot = boot
	c.monotonic = monotonic
}

// micros formats a time in microseconds as _SOURCE_REALTIME_TIMESTAMP
// holds it.
func micros(usec uint64) string { return strconv.FormatUint(usec, 10) }

// demoApp are the fields of an entry written by the basic set's usual
// program, followed by extra.
func demoApp(extra ...field) []field {
	return append([]field{
		text("_HOSTNAME", "fixture-host"),
		text("SYSLOG_IDENTIFIER", "demo-app"),
		text("_PID", "4101"),
		text("_SYSTEMD_UNIT", "demo-app.service"),
		text("PRIORITY", "6"),
	}, extra...)
}

// basicEntries are the entries of the basic set: one entry for each case
// a journal's text has to get right, over two boots.
func basicEntries() []entry {
	c := clock{realtime: baseTime + second, monotonic: 3 * second, step: second + 1237, boot: bootOne}
	entries := []entry{
		c.stamp(demoApp(text("MESSAGE", "basic 1: service started"))...),
		c.stamp(demoApp(text("MESSAGE", "basic 2: a message of three lines\nbasic 2, second line\nbasic 2, third line"))...),
		c.stamp(demoApp(text("MESSAGE", "basic 3: binary payload\x00after a NUL byte"))...),
		c.stamp(demoApp(text("MESSAGE", "basic 4: column\tvalue \x1b[1;31mred text\x1b[0m plain again"))...),
		// Entry 5 has no MESSAGE field: journalctl does not show it.
		c.stamp(demoApp(
			text("CODE_FILE", "demo.c"),
			text("CODE_LINE", "42"),
			text("MESSAGE_ID", "0f1e2d3c4b5a69788796a5b4c3d2e1f0"),
		)...),
		// Entry 6 has an empty MESSAGE: journalctl shows the prefix alone.
		c.stamp(demoApp(text("MESSAGE", ""))...),
		// The value of the last item in stored order wins. Two new names
		// are stored in the order sent, so demo-second wins; the second
		// MESSAGE repeats basic 1's value, which the journal stored
		// earlier, so it sorts first and the new value wins.
		c.stamp(
			text("_HOSTNAME", "fixture-host"),
			text("SYSLOG_IDENTIFIER", "demo-first"),
			text("SYSLOG_IDENTIFIER", "demo-second"),
			text("_PID", "4101"),
			text("MESSAGE", "basic 7: duplicate fields, the value stored last is shown"),
			text("MESSAGE", "basic 1: service started"),
		),
		c.stamp(demoApp(
			text("_SOURCE_REALTIME_TIMESTAMP", micros(c.now()-350_000)),
			text("MESSAGE", "basic 8: a valid source time, 0.35 seconds before the entry time"),
		)...),
		c.stamp(demoApp(
			text("_SOURCE_REALTIME_TIMESTAMP", "1768464000x"),
			text("MESSAGE", "basic 9: an invalid source time, so the entry time is shown"),
		)...),
		c.stamp(demoApp(
			text("_SOURCE_REALTIME_TIMESTAMP", micros(c.now()-3*minute)),
			text("MESSAGE", "basic 10: a source time three minutes before the entry time"),
		)...),
		c.stamp(
			text("_HOSTNAME", ""),
			text("SYSLOG_IDENTIFIER", ""),
			text("_PID", ""),
			text("MESSAGE", "basic 11: empty host, name and pid"),
		),
		c.stamp(
			text("_HOSTNAME", "fixture-host"),
			text("_COMM", "demo-worker"),
			text("_PID", "4102"),
			text("MESSAGE", "basic 12: no SYSLOG_IDENTIFIER, so the name comes from _COMM"),
		),
		c.stamp(demoApp(text("MESSAGE", longValue("basic 13: a value of 2 KiB, compressed in a journal that compresses;", 2048)))...),
	}
	c.newBoot(bootTwo, 2*second)
	return append(entries,
		c.stamp(demoApp(text("MESSAGE", "basic 14: the first entry of the second boot"))...),
		c.stamp(demoApp(text("MESSAGE", "basic 15: a message that ends with a newline\n"))...),
	)
}

// longValue returns head followed by numbered segments, exactly size
// bytes long: a printable value that compresses well.
func longValue(head string, size int) string {
	var b strings.Builder
	b.WriteString(head)
	for i := 1; b.Len() < size; i++ {
		fmt.Fprintf(&b, " segment %04d;", i)
	}
	return b.String()[:size]
}

// manyEntries are the entries of the many set: 2,000 entries that share
// a few hosts, units and names, as journald's entries share the DATA
// objects of those values. The values cycle with periods 3, 4 and 16,
// so the same combination comes back every 48 entries.
func manyEntries() []entry {
	const entries = 2000
	hosts := []string{"web-1", "web-2", "db-1"}
	services := []struct{ unit, name, pid string }{
		{"nginx.service", "nginx", "1201"},
		{"app.service", "app", "1310"},
		{"backup.service", "backup", "1422"},
		{"cron.service", "cron", "1533"},
	}
	// Message k%16 is written by service k%4.
	messages := []string{
		"GET /status 200 in 3 ms", "worker pool at 12 of 32", "chunk 7 of 40 written", "job hourly-report started",
		"GET /metrics 200 in 5 ms", "queue depth 4", "chunk 8 of 40 written", "job hourly-report finished",
		"POST /login 302 in 41 ms", "cache hit ratio 0.93", "chunk 9 of 40 written", "job cleanup started",
		"GET /missing 404 in 2 ms", "slow request took 1.8 s", "backup verified", "job cleanup finished",
	}
	c := clock{realtime: baseTime, monotonic: 5 * second, step: 250_000, boot: bootOne}
	out := make([]entry, 0, entries)
	for k := range entries {
		service := services[k%len(services)]
		priority := "6"
		if k%12 == 0 {
			priority = "4"
		}
		out = append(out, c.stamp(
			text("_HOSTNAME", hosts[k%len(hosts)]),
			text("_SYSTEMD_UNIT", service.unit),
			text("SYSLOG_IDENTIFIER", service.name),
			text("_PID", service.pid),
			text("PRIORITY", priority),
			text("MESSAGE", messages[k%len(messages)]),
		))
	}
	return out
}

// stampsEntries are the entries of the stamps set: the time shown goes
// back by more than a second several ways — the entry's own time steps
// back (a clock set back), a _SOURCE_REALTIME_TIMESTAMP before the time
// shown above it, a time on the day before — and two entries share one
// time.
func stampsEntries() []entry {
	steps := []struct {
		realtime int64 // microseconds after baseTime; negative is before it
		source   int64 // the same for _SOURCE_REALTIME_TIMESTAMP; 0 is none
		message  string
	}{
		{0, 0, "stamps 1: the first entry"},
		{1_000_101, 0, "stamps 2: one second later"},
		{2_000_202, 0, "stamps 3: two seconds later"},
		{3_000_303, 0, "stamps 4: three seconds later"},
		{800_404, 0, "stamps 5: the clock went back 2.2 seconds"},
		{1_800_505, -int64(hour), "stamps 6: a source time an hour before the entry time"},
		{2_800_606, 0, "stamps 7: back at the entry time"},
		{3_800_707, 1_550_000, "stamps 8: a source time 1.25 seconds before the time shown above"},
		{4_800_808, 0, "stamps 9: after the source time"},
		{5_800_909, -int64(8*hour + 30*second), "stamps 10: a source time on the day before"},
		{6_801_010, 0, "stamps 11: two entries with one time"},
		{6_801_010, 0, "stamps 12: two entries with one time"},
		{-int64(10 * minute), 0, "stamps 13: the clock went back ten minutes"},
		{-int64(10*minute) + 1_000_000, 0, "stamps 14: one second after that"},
	}
	out := make([]entry, 0, len(steps))
	for k, s := range steps {
		fields := []field{
			text("_HOSTNAME", "stamp-host"),
			text("SYSLOG_IDENTIFIER", "clockd"),
			text("_PID", "77"),
		}
		if s.source != 0 {
			fields = append(fields, text("_SOURCE_REALTIME_TIMESTAMP", micros(addMicros(baseTime, s.source))))
		}
		out = append(out, entry{
			realtime:  addMicros(baseTime, s.realtime),
			monotonic: 9*second + uint64(k)*second,
			bootID:    bootOne,
			fields:    append(fields, text("MESSAGE", s.message)),
		})
	}
	return out
}

// addMicros adds a signed number of microseconds to a time.
func addMicros(usec uint64, delta int64) uint64 {
	if delta < 0 {
		return usec - uint64(-delta)
	}
	return usec + uint64(delta)
}
