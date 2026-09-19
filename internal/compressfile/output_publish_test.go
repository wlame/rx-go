package compressfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// otherText is a second text, told apart from logText by every line.
func otherText(lines int) []byte {
	var b bytes.Buffer
	for n := 1; n <= lines; n++ {
		fmt.Fprintf(&b, "OTHER %d level=warn message=retrying after %d ms\n", n, n%89)
	}
	return b.Bytes()
}

// twoInputsForOneOutput writes app.log and a gzip app.log.gz holding
// another text, the pair whose default outputs are both app.log.zst,
// and returns their paths, their texts and that output path.
func twoInputsForOneOutput(t *testing.T) (inputs [2]string, texts [2][]byte, output string) {
	t.Helper()
	dir := t.TempDir()
	texts = [2][]byte{logText(20000), otherText(15000)}
	inputs = [2]string{filepath.Join(dir, "app.log"), filepath.Join(dir, "app.log.gz")}
	if err := os.WriteFile(inputs[0], texts[0], 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(inputs[1], compressedcopy.Encode(t, compressedcopy.Gzip, texts[1]), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return inputs, texts, filepath.Join(dir, "app.log.zst")
}

// compressBoth runs one Compress per input at the same time, all to
// output, and returns each one's result and error.
func compressBoth(inputs [2]string, output string, overwrite bool) ([2]Result, [2]error) {
	var (
		results [2]Result
		errs    [2]error
		wg      sync.WaitGroup
	)
	// Each goroutine writes only its own slot of results and errs, so
	// they need no lock; wg.Wait makes their writes visible here.
	for i := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = Compress(context.Background(), Options{
				InputPath: inputs[i], OutputPath: output, FrameSize: 16 << 10, Level: 1, Overwrite: overwrite,
			})
		}()
	}
	wg.Wait()
	return results, errs
}

// requireOnlyFiles fails unless dir holds exactly the names given: no
// output left behind, no temporary file.
func requireOnlyFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if fmt.Sprint(got) != fmt.Sprint(names) {
		t.Errorf("%s holds %v, want %v", dir, got, names)
	}
}

// fileSize is the size of the file at path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.Size()
}

// Two compressions to one output that may not overwrite: exactly one
// of them puts its file there, the other is refused, and the file is
// the winner's whole, the size the winner reports.
func TestCompress_ConcurrentWritersOfOneOutputLeaveOneWholeFile(t *testing.T) {
	t.Parallel()
	for round := range 5 {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			t.Parallel()
			inputs, texts, output := twoInputsForOneOutput(t)

			results, errs := compressBoth(inputs, output, false)

			winner := -1
			for i, err := range errs {
				switch {
				case err == nil && winner == -1:
					winner = i
				case err == nil:
					t.Fatalf("both compressions report success over one file")
				case !errors.Is(err, ErrOutputExists):
					t.Fatalf("compression %d: %v, want success or ErrOutputExists", i, err)
				}
			}
			if winner == -1 {
				t.Fatalf("both compressions were refused: %v", errs)
			}
			if got := decompressedText(t, output); !bytes.Equal(got, texts[winner]) {
				t.Errorf("the output holds %d bytes of text, want the winner's %d", len(got), len(texts[winner]))
			}
			if size := fileSize(t, output); results[winner].CompressedSize != size {
				t.Errorf("compressed_size %d, want the output's %d", results[winner].CompressedSize, size)
			}
		})
	}
}

// Two compressions to one output that may overwrite: both succeed, and
// the file left is one of them whole, with the size that one reports.
func TestCompress_ConcurrentOverwritesLeaveOneWholeFile(t *testing.T) {
	t.Parallel()
	for round := range 5 {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			t.Parallel()
			inputs, texts, output := twoInputsForOneOutput(t)

			results, errs := compressBoth(inputs, output, true)

			for i, err := range errs {
				if err != nil {
					t.Fatalf("compression %d: %v", i, err)
				}
			}
			got := decompressedText(t, output)
			holder := -1
			for i, text := range texts {
				if bytes.Equal(got, text) {
					holder = i
				}
			}
			if holder < 0 {
				t.Fatalf("the output holds %d bytes of text that are neither input's", len(got))
			}
			if size := fileSize(t, output); results[holder].CompressedSize != size {
				t.Errorf("compressed_size %d, want the output's %d", results[holder].CompressedSize, size)
			}
		})
	}
}

// A compression that fails or is canceled leaves nothing under the
// output name, and no temporary file either.
func TestCompress_FailureLeavesNoFileBehind(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	body := compressedcopy.Encode(t, compressedcopy.Gzip, logText(20000))
	cases := []struct {
		name  string
		ctx   context.Context
		input []byte
	}{
		{"corrupt input", context.Background(), body[:len(body)/2]},
		{"canceled", canceled, body},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			input := filepath.Join(dir, "app.log.gz")
			if err := os.WriteFile(input, tc.input, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			_, err := Compress(tc.ctx, Options{
				InputPath: input, OutputPath: filepath.Join(dir, "app.log.zst"), FrameSize: 4096,
			})

			if err == nil {
				t.Fatal("Compress succeeded, want an error")
			}
			requireOnlyFiles(t, dir, "app.log.gz")
		})
	}
}

// A failed compression that was allowed to overwrite leaves the file
// that held the output name as it was.
func TestCompress_FailedOverwriteKeepsTheExistingOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	input := filepath.Join(dir, "app.log.gz")
	body := compressedcopy.Encode(t, compressedcopy.Gzip, logText(20000))
	if err := os.WriteFile(input, body[:len(body)/2], 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	output := filepath.Join(dir, "app.log.zst")
	if err := os.WriteFile(output, []byte("an older file\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Compress(context.Background(), Options{InputPath: input, OutputPath: output, Overwrite: true})

	if err == nil {
		t.Fatal("Compress succeeded, want the decoder's error")
	}
	if got, _ := os.ReadFile(output); string(got) != "an older file\n" { //nolint:gosec // test path
		t.Errorf("the output holds %q, want the older file", got)
	}
	requireOnlyFiles(t, dir, "app.log.gz", "app.log.zst")
}

// Without Overwrite, anything that holds the output name is left as it
// is: a file, and a symbolic link, even one that leads nowhere.
func TestCompress_RefusesAnOutputNameThatIsTaken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		taken func(t *testing.T, output string)
	}{
		{"file", func(t *testing.T, output string) {
			if err := os.WriteFile(output, []byte("an older file\n"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"dangling symlink", func(t *testing.T, output string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(output), "nowhere"), output); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			input := filepath.Join(dir, "app.log")
			if err := os.WriteFile(input, logText(100), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			output := filepath.Join(dir, "app.log.zst")
			tc.taken(t, output)
			before, err := os.Lstat(output)
			if err != nil {
				t.Fatalf("lstat: %v", err)
			}

			_, err = Compress(context.Background(), Options{InputPath: input, OutputPath: output})

			if !errors.Is(err, ErrOutputExists) {
				t.Errorf("Compress = %v, want ErrOutputExists", err)
			}
			after, err := os.Lstat(output)
			if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() {
				t.Errorf("the output name changed: %v", err)
			}
			requireOnlyFiles(t, dir, "app.log", "app.log.zst")
		})
	}
}

// With Overwrite, a symbolic link at the output name is replaced by the
// output; the file it leads to is not written.
func TestCompress_OverwriteReplacesASymlinkWithoutWritingThroughIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	text := logText(100)
	input := filepath.Join(dir, "app.log")
	if err := os.WriteFile(input, text, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	target := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	output := filepath.Join(dir, "app.log.zst")
	if err := os.Symlink(target, output); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := Compress(context.Background(), Options{InputPath: input, OutputPath: output, Overwrite: true}); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	info, err := os.Lstat(output)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the output is not a regular file: %v %v", info, err)
	}
	if got := decompressedText(t, output); !bytes.Equal(got, text) {
		t.Error("the output does not hold the input's text")
	}
	if got, _ := os.ReadFile(target); string(got) != "keep me\n" { //nolint:gosec // test path
		t.Errorf("the link's target holds %q, want it unchanged", got)
	}
}

// On a file system without hard links the output is still put in place
// only when nothing holds its name.
//
// Not parallel: it replaces linkOutput for the package. Go runs every
// parallel test of a package only after its sequential ones return.
func TestCompress_WithoutHardLinksStillRefusesATakenOutputName(t *testing.T) {
	saved := linkOutput
	linkOutput = func(*os.Root, string, string) error { return &os.LinkError{Op: "link", Err: syscall.ENOTSUP} }
	t.Cleanup(func() { linkOutput = saved })
	dir := t.TempDir()
	text := logText(100)
	input := filepath.Join(dir, "app.log")
	if err := os.WriteFile(input, text, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	output := filepath.Join(dir, "app.log.zst")

	if _, err := Compress(context.Background(), Options{InputPath: input, OutputPath: output}); err != nil {
		t.Fatalf("Compress to a free name: %v", err)
	}
	if got := decompressedText(t, output); !bytes.Equal(got, text) {
		t.Error("the output does not hold the input's text")
	}
	_, err := Compress(context.Background(), Options{InputPath: input, OutputPath: output})
	if !errors.Is(err, ErrOutputExists) {
		t.Errorf("Compress to a taken name = %v, want ErrOutputExists", err)
	}
	requireOnlyFiles(t, dir, "app.log", "app.log.zst")
}

// The output's directory is looked up again from the search root when
// the output is written. A directory swapped for a link to somewhere
// outside the root after the caller's check gets nothing written.
func TestCompress_OutputDirectorySwappedForALinkGetsNothingWritten(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(root, "out"), outside} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	input := filepath.Join(root, "app.log")
	if err := os.WriteFile(input, logText(100), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(paths.Reset)
	output, err := paths.ValidatePathWithinRoots(filepath.Join(root, "out", "app.log.zst"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// After the check: out/ becomes a link to the outside directory.
	if err := os.Remove(filepath.Join(root, "out")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err = Compress(context.Background(), Options{InputPath: input, OutputPath: output, Overwrite: true})

	if err == nil {
		t.Error("Compress wrote through a directory swapped for a link")
	}
	requireOnlyFiles(t, outside)
}

// Check reads the input through its pin: an input that leads outside
// the search roots is refused, not looked at.
func TestCheck_RefusesAnInputOutsideTheSearchRoots(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(base, "secret.log.zst")
	if err := os.WriteFile(secret, compressedcopy.Encode(t, compressedcopy.SeekableZstd, logText(100)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(root, "app.log.zst")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(paths.Reset)

	err = Check(link, filepath.Join(root, "out.zst"), false)

	var outside *paths.ErrPathOutsideRoots
	if !errors.As(err, &outside) {
		t.Errorf("Check = %v, want ErrPathOutsideRoots", err)
	}
}
