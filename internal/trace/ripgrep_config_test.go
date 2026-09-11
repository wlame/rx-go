package trace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ripgrep reads RIPGREP_CONFIG_PATH unless told not to, so a personal
// ripgreprc used to reach every rx search. Each storage form runs rg from
// its own call site, so each one is checked: a config file that turns
// on --fixed-strings must not stop `err.r` from matching as a regex.
func TestRipgrepConfigFileDoesNotChangeTheAnswer(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	config := filepath.Join(t.TempDir(), "ripgreprc")
	if err := os.WriteFile(config, []byte("--fixed-strings\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("RIPGREP_CONFIG_PATH", config)

	text := []byte("ok\nerror one\nfine\n")
	plain := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(plain, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	files := map[string]string{
		"plain":         plain,
		"gzip":          writeGzipFile(t, text),
		"seekable zstd": writeSeekableZstdFile(t, text, 4096),
	}
	for name, path := range files {
		t.Run(name, func(t *testing.T) {
			resp, err := New().RunWithOptions(context.Background(),
				[]string{path}, []string{"err.r"}, Options{NoCache: true})
			if err != nil {
				t.Fatalf("RunWithOptions: %v", err)
			}

			if len(resp.Matches) != 1 {
				t.Errorf("matches = %d, want 1 (the config file applied)", len(resp.Matches))
			}
		})
	}
}
