package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx/internal/cache"
	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/index"
	"github.com/wlame/rx/internal/rgjson"
)

// benchTestdataDir returns the absolute path to the testdata directory for benchmarks.
// Skips the benchmark if testdata is not found.
func benchTestdataDir(b *testing.B) string {
	b.Helper()
	dir, err := filepath.Abs("../../testdata")
	if err != nil {
		b.Skipf("cannot resolve testdata path: %v", err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		b.Skipf("testdata directory not found at %s", dir)
	}
	return dir
}

// BenchmarkTrace_SmallFile benchmarks trace on the sample.txt fixture.
func BenchmarkTrace_SmallFile(b *testing.B) {
	td := benchTestdataDir(b)
	path := filepath.Join(td, "sample.txt")

	// Disable caching and indexing to measure raw search performance.
	b.Setenv("RX_NO_CACHE", "1")
	b.Setenv("RX_NO_INDEX", "1")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Trace(context.Background(), TraceRequest{
			Paths:    []string{path},
			Patterns: []string{"ERROR"},
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTrace_WithIndex benchmarks trace with a pre-built index.
func BenchmarkTrace_WithIndex(b *testing.B) {
	td := benchTestdataDir(b)
	path := filepath.Join(td, "sample.txt")

	// Build the index once before benchmarking.
	cacheDir := b.TempDir()
	b.Setenv("RX_CACHE_DIR", cacheDir)
	b.Setenv("RX_NO_CACHE", "1")
	b.Setenv("RX_NO_INDEX", "")

	cfg := config.Load()
	idx, err := index.BuildIndex(path, &cfg)
	if err != nil {
		b.Fatalf("failed to build index: %v", err)
	}
	cachePath := index.IndexCachePath(cacheDir, path)
	if err := index.Save(cachePath, idx); err != nil {
		b.Fatalf("failed to save index: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Trace(context.Background(), TraceRequest{
			Paths:    []string{path},
			Patterns: []string{"ERROR"},
			UseIndex: true,
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTrace_CacheHit benchmarks trace with a warm cache.
func BenchmarkTrace_CacheHit(b *testing.B) {
	td := benchTestdataDir(b)
	path := filepath.Join(td, "sample.txt")

	cacheDir := b.TempDir()
	b.Setenv("RX_CACHE_DIR", cacheDir)
	b.Setenv("RX_NO_CACHE", "")
	b.Setenv("RX_LARGE_FILE_MB", "0") // treat test file as "large" so it gets cached

	patterns := []string{"ERROR"}

	// Warm the cache with a first search.
	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: patterns,
	})
	if err != nil {
		b.Fatal(err)
	}
	// Also manually store to guarantee cache is populated.
	if storeErr := cache.Store(cacheDir, patterns, nil, path, resp); storeErr != nil {
		b.Fatalf("failed to store cache: %v", storeErr)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Trace(context.Background(), TraceRequest{
			Paths:    []string{path},
			Patterns: patterns,
			UseCache: true,
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChunker_PlanChunks benchmarks chunk planning for files of various sizes.
func BenchmarkChunker_PlanChunks(b *testing.B) {
	// Create a temporary file to plan chunks on.
	dir := b.TempDir()
	content := strings.Repeat("line of text content here for chunking\n", 100000)
	path := filepath.Join(dir, "large.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		b.Fatal(err)
	}

	stat, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}

	cfg := config.Load()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := PlanChunks(path, stat.Size(), &cfg)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRgJSON_Parse benchmarks parsing of rg JSON output.
func BenchmarkRgJSON_Parse(b *testing.B) {
	// Build a realistic rg JSON stream with many match events.
	var buf bytes.Buffer
	// Begin event.
	buf.WriteString(`{"type":"begin","data":{"path":{"text":"test.log"}}}` + "\n")
	// Match events — 1000 simulated rg match lines.
	for i := 0; i < 1000; i++ {
		line := fmt.Sprintf(
			`{"type":"match","data":{"path":{"text":"test.log"},"lines":{"text":"ERROR: something failed at step %d\n"},"line_number":%d,"absolute_offset":%d,"submatches":[{"match":{"text":"ERROR"},"start":0,"end":5}]}}`,
			i, i+1, i*80,
		)
		buf.WriteString(line + "\n")
	}
	// End event.
	buf.WriteString(`{"type":"end","data":{"path":{"text":"test.log"},"binary_offset":null,"stats":{"elapsed":{"secs":0,"nanos":1000000,"human":"1ms"},"searches":1,"searches_with_match":1,"bytes_searched":80000,"bytes_printed":40000,"matched_lines":1000,"matches":1000}}}` + "\n")
	// Summary event.
	buf.WriteString(`{"type":"summary","data":{"elapsed_total":{"secs":0,"nanos":2000000,"human":"2ms"},"stats":{"elapsed":{"secs":0,"nanos":1000000,"human":"1ms"},"searches":1,"searches_with_match":1,"bytes_searched":80000,"bytes_printed":40000,"matched_lines":1000,"matches":1000}}}` + "\n")

	data := buf.Bytes()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		parser := rgjson.NewParser(bytes.NewReader(data), 8)
		msgs, err := parser.ParseAll()
		if err != nil {
			b.Fatal(err)
		}
		_ = msgs
	}
}
