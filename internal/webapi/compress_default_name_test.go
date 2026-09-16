package webapi

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A request without output_path writes a compressed input's text to its
// name without the compression suffix, as rx compress does, and the
// task's cli_command, which names no --output, writes the same file.
func TestCompressPost_DefaultOutputDropsTheCompressionSuffix(t *testing.T) {
	url, root, text := compressInputServer(t)
	input := filepath.Join(root, "app.log.gz")
	noIndex := false

	result := compressAndWait(t, url, rxtypes.CompressRequest{
		InputPath: input, FrameSize: "16K", CompressionLevel: 3, BuildIndex: &noIndex,
	})

	want := filepath.Join(root, "app.log.zst")
	if result["output_path"] != want {
		t.Errorf("output_path = %v, want %s", result["output_path"], want)
	}
	if got := seekableText(t, want); !bytes.Equal(got, text) {
		t.Errorf("%s holds %d bytes, want the input's %d bytes of text", want, len(got), len(text))
	}
	if got, wantCommand := result["cli_command"], "rx compress "+input+" --frame-size=16K --build-index=false"; got != wantCommand {
		t.Errorf("cli_command = %v, want %s", got, wantCommand)
	}
}

// The default output may already exist: without force the request is
// refused with 400 and the file is left alone; with force it is replaced.
func TestCompressPost_DefaultOutputThatExistsNeedsForce(t *testing.T) {
	url, root, text := compressInputServer(t)
	input := filepath.Join(root, "app.log.gz")
	existing := filepath.Join(root, "app.log.zst")
	if err := os.WriteFile(existing, []byte("an older file\n"), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	noIndex := false
	req := rxtypes.CompressRequest{InputPath: input, FrameSize: "16K", CompressionLevel: 3, BuildIndex: &noIndex}

	status, body := postCompress(t, url, req)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %v", status, body)
	}
	if want := "Output file already exists: " + existing; body["detail"] != want {
		t.Errorf("detail = %v, want %q", body["detail"], want)
	}
	if got, _ := os.ReadFile(existing); string(got) != "an older file\n" { //nolint:gosec // test path
		t.Errorf("the refused request changed %s", existing)
	}

	req.Force = true
	compressAndWait(t, url, req)
	if got := seekableText(t, existing); !bytes.Equal(got, text) {
		t.Errorf("force left %d bytes of text in %s, want %d", len(got), existing, len(text))
	}
}

// A plain zstd input named app.log.zst would get its own name as the
// default output: refused with 400, force or not.
func TestCompressPost_PlainZstdInputIsNeverItsOwnDefaultOutput(t *testing.T) {
	url, root, text := compressInputServer(t)
	input := filepath.Join(root, "app.log.zst")
	body := compressedcopy.Encode(t, compressedcopy.Zstd, text)
	if err := os.WriteFile(input, body, 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	for _, force := range []bool{false, true} {
		status, answer := postCompress(t, url, rxtypes.CompressRequest{
			InputPath: input, FrameSize: "16K", CompressionLevel: 3, Force: force,
		})
		if status != http.StatusBadRequest {
			t.Fatalf("force=%v: status = %d, want 400; body %v", force, status, answer)
		}
		want := input + `: the output path is the input file (set "output_path" to another file)`
		if answer["detail"] != want {
			t.Errorf("force=%v: detail = %v, want %q", force, answer["detail"], want)
		}
	}
	if got, _ := os.ReadFile(input); !bytes.Equal(got, body) { //nolint:gosec // test path
		t.Error("the input was changed")
	}
}
