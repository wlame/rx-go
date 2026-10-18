package clicommand

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/compressfile"
)

// A usage error leaves no --output-dir behind: the flags are checked
// before the directory is created, and with valid flags the directory
// is created and the output lands in it.
func TestCompress_UsageErrorCreatesNoOutputDir(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "a.log")
	if err := os.WriteFile(input, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, params := range map[string]compressParams{
		"bad level":      {paths: []string{input}, outputDir: filepath.Join(dir, "newdir"), level: 99, frameSize: "4M", workers: 1},
		"bad frame size": {paths: []string{input}, outputDir: filepath.Join(dir, "newdir"), level: 3, frameSize: "lots", workers: 1},
		"no workers":     {paths: []string{input}, outputDir: filepath.Join(dir, "newdir"), level: 3, frameSize: "4M", workers: 0},
	} {
		var out bytes.Buffer
		err := runCompress(context.Background(), &out, params)
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
			t.Fatalf("%s: got %v, want exit code %d", name, err, ExitUsageError)
		}
		if _, statErr := os.Stat(params.outputDir); !os.IsNotExist(statErr) {
			t.Errorf("%s: --output-dir exists after a usage error (stat: %v)", name, statErr)
		}
	}
	var out bytes.Buffer
	good := compressParams{paths: []string{input}, outputDir: filepath.Join(dir, "newdir"), level: 3, frameSize: "4M", workers: 1}
	if err := runCompress(context.Background(), &out, good); err != nil {
		t.Fatalf("runCompress with valid flags: %v", err)
	}
	if _, err := os.Stat(filepath.Join(good.outputDir, "a.log.zst")); err != nil {
		t.Errorf("the output is not in --output-dir: %v", err)
	}
}

// A frame size below one byte is refused by the parser, so the encoder
// never sees a size it would silently replace with its default. 0.1B
// parses to 0 bytes and is refused the same way.
func TestParseFrameSize_BelowOneByteIsRefused(t *testing.T) {
	for _, in := range []string{"0", "-1", "0B", "0.1B", "-4K"} {
		_, err := ParseFrameSize(in)
		if !errors.Is(err, compressfile.ErrFrameSizeBelowMinimum) {
			t.Errorf("%s: err %v, want ErrFrameSizeBelowMinimum", in, err)
			continue
		}
		if !strings.Contains(err.Error(), "--frame-size must be at least 1 byte") {
			t.Errorf("%s: message %q should name the flag and the bound", in, err)
		}
	}
	for in, want := range map[string]int64{"1": 1, "1B": 1, "0.5K": 512} {
		got, err := ParseFrameSize(in)
		if err != nil || got != want {
			t.Errorf("%s: got %d, %v; want %d", in, got, err, want)
		}
	}
}
