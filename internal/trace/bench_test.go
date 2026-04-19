package trace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkChunkerGetOffsets measures the cost of computing line-aligned
// chunk boundaries for a large file. Run with:
//
//	go test -bench=BenchmarkChunkerGetOffsets ./internal/trace/
//
// The result is compared across Go versions / chunking refactors in
// BENCHMARKS.md. Numbers from a clean run are the reference; ±20% drift
// should be investigated before shipping.
func BenchmarkChunkerGetOffsets(b *testing.B) {
	// Build a ~10 MB fixture. Not using testparity helpers to keep the
	// benchmark self-contained.
	dir := b.TempDir()
	p := filepath.Join(dir, "bench.log")
	f, err := os.Create(p)
	if err != nil {
		b.Fatalf("create: %v", err)
	}
	for f.Fd(); true; {
		// Write roughly 10 MB of log lines.
		line := []byte("2026-04-18T00:00:00.000 INFO request=1234 status=200 path=/api\n")
		if _, err := f.Write(bytes.Repeat(line, 200)); err != nil {
			b.Fatalf("write: %v", err)
		}
		stat, _ := f.Stat()
		if stat.Size() >= 10*1024*1024 {
			break
		}
	}
	_ = f.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := GetFileOffsets(p, 1024*1024) // 1 MB chunks
		if err != nil {
			b.Fatalf("GetFileOffsets: %v", err)
		}
	}
}

// BenchmarkEngine_RunSmall measures the end-to-end trace path on a small
// fixture. Exercises rg subprocess lifecycle + event parsing.
func BenchmarkEngine_RunSmall(b *testing.B) {
	if _, err := exec.LookPath("rg"); err != nil {
		b.Skip("rg not installed")
	}
	dir := b.TempDir()
	p := filepath.Join(dir, "small.log")
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		if i%100 == 0 {
			fmt.Fprintf(&sb, "error line %d\n", i)
		} else {
			fmt.Fprintf(&sb, "ordinary line %d\n", i)
		}
	}
	_ = os.WriteFile(p, []byte(sb.String()), 0o644)

	eng := New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.RunWithOptions(context.Background(), []string{p},
			[]string{"error"}, Options{}); err != nil {
			b.Fatalf("Run: %v", err)
		}
	}
}
