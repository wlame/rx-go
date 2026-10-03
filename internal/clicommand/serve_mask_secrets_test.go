package clicommand

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// serveCommandRun is an `rx serve` started through its cobra command,
// as the binary starts it: the command line parsed by the command, the
// server built by runServe.
type serveCommandRun struct {
	base string
	out  *safeBuffer
	done chan error
}

// startServeCommand runs `rx serve ARGS --host=127.0.0.1 --port=P
// --skip-frontend` in a goroutine and waits until it answers /health.
func startServeCommand(t *testing.T, args ...string) *serveCommandRun {
	t.Helper()
	port := freePort(t)
	run := &serveCommandRun{base: fmt.Sprintf("http://127.0.0.1:%d", port), out: &safeBuffer{}, done: make(chan error, 1)}
	cmd := NewServeCommand(run.out, "test")
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(append(args, "--host=127.0.0.1", fmt.Sprintf("--port=%d", port), "--skip-frontend"))
	// Go note: the command blocks until a signal stops it, so it runs in
	// its own goroutine and hands its error back on the buffered channel.
	go func() { run.done <- cmd.Execute() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(run.base + "/health"); err == nil {
			_ = resp.Body.Close()
			return run
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("rx serve did not answer:\n%s", run.out.String())
	return nil
}

// stop sends SIGTERM, which runServe's signal handler takes as its stop,
// and waits for the command to return.
func (r *serveCommandRun) stop(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("rx serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rx serve did not stop within 10s")
	}
}

// The masking mode of `rx serve`'s own command line reaches the server
// it starts, over RX_MASK_SECRETS either way: --mask-secrets with
// RX_MASK_SECRETS=0 masks the private key's lines of a findings answer
// and names --mask-secrets in its cli_command, --mask-secrets=false
// with RX_MASK_SECRETS=1 shows them; the banner names the mode the
// answers have.
func TestServeCommand_TheMaskSecretsFlagReachesTheServer(t *testing.T) {
	cases := []struct {
		flag, env string
		masked    bool
	}{
		{"--mask-secrets", "0", true},
		{"--mask-secrets=false", "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			t.Cleanup(resetServeGlobals)
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			t.Setenv("RX_MASK_SECRETS", tc.env)
			signatureDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(signatureDir, "team.yml"), []byte("signatures: []\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RX_SIGNATURES_PATH", signatureDir)
			root := t.TempDir()
			log := writeText(t, root, "app.log", secretLog)

			run := startServeCommand(t, "--search-root="+root, tc.flag)
			resp, err := http.Get(run.base + "/v1/trace?" + url.Values{"path": {log}, "signatures": {"private-key"}}.Encode())
			if err != nil {
				run.stop(t)
				t.Fatal(err)
			}
			var got rxtypes.TraceResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&got)
			_ = resp.Body.Close()
			run.stop(t)
			if decodeErr != nil || resp.StatusCode != http.StatusOK || len(got.Findings) != 1 || got.CLICommand == nil {
				t.Fatalf("status %d, findings %+v, error %v", resp.StatusCode, got.Findings, decodeErr)
			}

			lines, cli, banner := got.Findings[0].Lines, *got.CLICommand, run.out.String()
			want, wantBanner := []string{"-----BEGIN PRIVATE KEY-----", secretBody, secretBody + "QQ", "-----END PRIVATE KEY-----"}, "Secrets: shown;"
			if tc.masked {
				want = []string{"----************", "[masked, 32 bytes]", "[masked, 34 bytes]", "[masked, 25 bytes]"}
				wantBanner = "Secrets: masked\n"
			}
			if !slices.Equal(lines, want) || slices.Contains(strings.Fields(cli), "--mask-secrets") != tc.masked {
				t.Errorf("private-key lines %q, cli_command %q; want lines %q", lines, cli, want)
			}
			if !strings.Contains(banner, wantBanner) {
				t.Errorf("banner lacks %q:\n%s", wantBanner, banner)
			}
		})
	}
}
