package index

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"

	// The real detector catalog, registered the way cmd/rx registers it,
	// so the analysis compared below finds real anomalies.
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/coredumpunix"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/jsonblob"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/longline"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/repeatidentical"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/secretsscan"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackgo"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackjava"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackjs"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackpython"
)

// buildAnomalousLog returns a log of about 20 KB whose ordinary lines
// read `LINE <n> ...`, with a Python traceback, a Java stack trace, a
// run of identical lines, a very long line and some empty lines in it.
// Cut into 512-byte frames, each of those spans a frame boundary.
func buildAnomalousLog() []byte {
	var lines []string
	filler := func(count int) {
		for i := 0; i < count; i++ {
			lines = append(lines, fmt.Sprintf("LINE %d request handled in %d ms", len(lines)+1, i%17))
		}
	}
	filler(60)
	lines = append(lines,
		"Traceback (most recent call last):",
		`  File "/srv/app/handler.py", line 42, in handle`,
		"    result = compute(payload)",
		`  File "/srv/app/compute.py", line 7, in compute`,
		"    return payload['value'] / 0",
		"ZeroDivisionError: division by zero",
	)
	filler(40)
	for i := 0; i < 8; i++ {
		lines = append(lines, "LINE heartbeat ok")
	}
	filler(30)
	lines = append(lines, "", "   ", "")
	lines = append(lines, fmt.Sprintf("LINE %d %s", len(lines)+1, strings.Repeat("x", 3000)))
	filler(50)
	lines = append(lines,
		`Exception in thread "main" java.lang.IllegalStateException: boom`,
		"\tat com.example.Service.run(Service.java:10)",
		"\tat com.example.Main.main(Main.java:5)",
	)
	filler(80)
	return []byte(strings.Join(lines, "\n") + "\n")
}

// analysisFields is every part of an index that an analysis computes
// from the file's text. Two indexes of the same text agree on all of
// it, however the text is stored; the line index and the container
// fields legitimately differ and are left out.
type analysisFields struct {
	AnalysisPerformed       bool
	LineCount               *int64
	EmptyLineCount          *int64
	LineEnding              *string
	LineLengthMax           *int64
	LineLengthAvg           *float64
	LineLengthMedian        *float64
	LineLengthP95           *float64
	LineLengthP99           *float64
	LineLengthStddev        *float64
	LineLengthMaxLineNumber *int64
	LineLengthMaxByteOffset *int64
	Anomalies               *[]rxtypes.AnomalyRangeResult
	AnomalySummary          map[string]int
	AnalysisWindowLines     *int
	AnalysisDetectorSet     *string
}

func analysisOf(idx *rxtypes.UnifiedFileIndex) analysisFields {
	return analysisFields{
		AnalysisPerformed:       idx.AnalysisPerformed,
		LineCount:               idx.LineCount,
		EmptyLineCount:          idx.EmptyLineCount,
		LineEnding:              idx.LineEnding,
		LineLengthMax:           idx.LineLengthMax,
		LineLengthAvg:           idx.LineLengthAvg,
		LineLengthMedian:        idx.LineLengthMedian,
		LineLengthP95:           idx.LineLengthP95,
		LineLengthP99:           idx.LineLengthP99,
		LineLengthStddev:        idx.LineLengthStddev,
		LineLengthMaxLineNumber: idx.LineLengthMaxLineNumber,
		LineLengthMaxByteOffset: idx.LineLengthMaxByteOffset,
		Anomalies:               idx.Anomalies,
		AnomalySummary:          idx.AnomalySummary,
		AnalysisWindowLines:     idx.AnalysisWindowLines,
		AnalysisDetectorSet:     idx.AnalysisDetectorSet,
	}
}

// writeSeekableCopy compresses text into a seekable .zst at path with
// the repository's own encoder, in frames of frameSize bytes.
func writeSeekableCopy(t *testing.T, text []byte, path string, frameSize int) {
	t.Helper()
	dst, err := os.Create(path) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = dst.Close() }()
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: frameSize, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), dst); err != nil {
		t.Fatalf("encode seekable: %v", err)
	}
}

// analyzeWithRealDetectors builds an analyzing index of path with the
// registered detector catalog, as `rx index --analyze` does.
func analyzeWithRealDetectors(t *testing.T, path string) *rxtypes.UnifiedFileIndex {
	t.Helper()
	idx, err := Build(path, BuildOptions{Analyze: true, Detectors: analyzer.LineDetectorSnapshot()})
	if err != nil {
		t.Fatalf("Build(%s): %v", path, err)
	}
	return idx
}

// requireSameAnalysis fails the test when got's analysis differs from
// the analysis of the plain text, naming the first field that differs.
func requireSameAnalysis(t *testing.T, label string, got, plain *rxtypes.UnifiedFileIndex) {
	t.Helper()
	gotFields, wantFields := analysisOf(got), analysisOf(plain)
	if reflect.DeepEqual(gotFields, wantFields) {
		return
	}
	gv, wv := reflect.ValueOf(gotFields), reflect.ValueOf(wantFields)
	for i := 0; i < gv.NumField(); i++ {
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			t.Errorf("%s: %s differs from the plain copy's", label, gv.Type().Field(i).Name)
		}
	}
}

func TestBuildAnalyzesASeekableZstdAsItsDecompressedText(t *testing.T) {
	text := buildAnomalousLog()
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plainPath, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	zstPath := filepath.Join(dir, "app.log.zst")
	writeSeekableCopy(t, text, zstPath, 512)

	plain := analyzeWithRealDetectors(t, plainPath)
	fromZst := analyzeWithRealDetectors(t, zstPath)

	if plain.Anomalies == nil || len(*plain.Anomalies) < 3 {
		t.Fatalf("the fixture should give several anomalies, got %v", plain.AnomalySummary)
	}
	if !fromZst.AnalysisPerformed {
		t.Fatal("analysis_performed is false for a seekable zstd file")
	}
	requireSameAnalysis(t, "seekable zstd", fromZst, plain)

	// The analysis must not cost the seekable index its own shape: the
	// frame table and the frame-numbered checkpoints stay.
	if fromZst.FileType != rxtypes.FileTypeSeekableZstd {
		t.Errorf("file_type = %q, want %q", fromZst.FileType, rxtypes.FileTypeSeekableZstd)
	}
	if fromZst.FrameCount == nil || *fromZst.FrameCount < 10 {
		t.Errorf("frame_count = %v, want the fixture's many frames", fromZst.FrameCount)
	}
	for _, entry := range fromZst.LineIndex {
		if entry.FrameIndex == nil {
			t.Fatalf("checkpoint %+v has no frame number", entry)
		}
	}

	// A cached seekable analysis answers the same request again.
	opts := BuildOptions{Analyze: true, Detectors: analyzer.LineDetectorSnapshot()}
	if !SatisfiesBuild(fromZst, opts) {
		t.Error("a seekable index built with analysis does not satisfy the same request")
	}
}

// compressedCopies writes text in every compressed format rx-go reads
// and returns the paths by format name. bzip2 needs the bzip2 binary,
// since Go's standard library only decodes it; without the binary that
// format is left out.
func compressedCopies(t *testing.T, text []byte, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	write := func(name string, body []byte) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		out[name] = path
	}

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(text)
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	write("app.log.gz", gz.Bytes())

	var xzBuf bytes.Buffer
	xw, err := xz.NewWriter(&xzBuf)
	if err != nil {
		t.Fatalf("xz writer: %v", err)
	}
	_, _ = xw.Write(text)
	if err := xw.Close(); err != nil {
		t.Fatalf("xz: %v", err)
	}
	write("app.log.xz", xzBuf.Bytes())

	zw, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	write("app.log.zst", zw.EncodeAll(text, nil))
	_ = zw.Close()

	seekablePath := filepath.Join(dir, "app.seekable.zst")
	writeSeekableCopy(t, text, seekablePath, 512)
	out["app.seekable.zst"] = seekablePath

	if _, err := exec.LookPath("bzip2"); err == nil {
		cmd := exec.Command("bzip2", "-c")
		cmd.Stdin = bytes.NewReader(text)
		body, err := cmd.Output()
		if err != nil {
			t.Fatalf("bzip2: %v", err)
		}
		write("app.log.bz2", body)
	} else {
		t.Log("bzip2 binary not on PATH; the bz2 copy is not checked")
	}
	return out
}

// No compressed format rx-go reads answers an analysis request with a
// silent analysis_performed: false; each one is analyzed as the text it
// holds.
func TestBuildAnalyzesEveryCompressedFormatAsItsText(t *testing.T) {
	text := buildAnomalousLog()
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plainPath, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	plain := analyzeWithRealDetectors(t, plainPath)

	for name, path := range compressedCopies(t, text, dir) {
		t.Run(name, func(t *testing.T) {
			got := analyzeWithRealDetectors(t, path)
			if !got.AnalysisPerformed {
				t.Fatalf("analysis_performed is false for %s", name)
			}
			requireSameAnalysis(t, name, got, plain)
		})
	}
}

// A frame that cannot be decoded fails an analyzing build with an
// error, and the decoder and the walk that share the pass both stop.
func TestBuildAnalyzeOfACorruptSeekableFrameFails(t *testing.T) {
	text := buildAnomalousLog()
	zstPath := filepath.Join(t.TempDir(), "app.log.zst")
	writeSeekableCopy(t, text, zstPath, 512)

	raw, err := os.ReadFile(zstPath) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Overwrite the middle of the compressed frames; the seek table at
	// the end stays intact, so the file is still recognized as seekable.
	middle := len(raw) / 2
	copy(raw[middle:], bytes.Repeat([]byte{0xff}, 64))
	if err := os.WriteFile(zstPath, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Build(zstPath, BuildOptions{Analyze: true, Detectors: analyzer.LineDetectorSnapshot()})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Build of a corrupt seekable file succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Build of a corrupt seekable file did not return")
	}
}
