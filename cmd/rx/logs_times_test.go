package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Human times follow the file's layout: a chain whose parts write
// seconds only prints seconds in rx logs show and rx logs time-range, as
// rx time-range prints them for one of its files.
func TestLogsShow_TimesInTheFilesLayout(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 1, 7, 0, 30, 0, time.UTC)
	write := func(name string, at time.Time, n int) {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "%s LINE %d\n", at.Add(time.Duration(i)*time.Second).Format("2006-01-02 15:04:05"), i+1)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("db.log.1", start, 3)
	write("db.log", start.Add(time.Hour), 2)
	_, single, _ := runRxIn(t, dir, nil, "time-range", "db.log.1")
	if !strings.Contains(single, "2026-10-01 07:00:30 .. 2026-10-01 07:00:32") {
		t.Fatalf("rx time-range: %q", single)
	}
	_, show, _ := runRxIn(t, dir, nil, "logs", "show", "db.log")
	_, span, _ := runRxIn(t, dir, nil, "logs", "time-range", "db.log")
	if !strings.Contains(show, "2026-10-01 07:00:30  ") || strings.Contains(show, ".000") {
		t.Fatalf("rx logs show:\n%s", show)
	}
	if !strings.Contains(span, "2026-10-01 07:00:30 .. 2026-10-01 08:00:31") {
		t.Fatalf("rx logs time-range: %q", span)
	}
}
