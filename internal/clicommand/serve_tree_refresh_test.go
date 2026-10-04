package clicommand

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// /health of a running `rx serve` carries the interval by which the
// viewer reads the loaded folders of its files list again: --tree-refresh
// when the command line gives it, else RX_TREE_REFRESH_SECONDS, else 5
// seconds; 0 turns the refresh off. The banner names the same interval.
func TestServeCommand_HealthCarriesTheTreeRefreshOfTheFlagTheVariableOrTheDefault(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		args   []string
		want   int
		banner string
	}{
		{"default", "", nil, 5, "Tree refresh: every 5 s\n"},
		{"the variable", "12", nil, 12, "Tree refresh: every 12 s\n"},
		{"the variable turns it off", "0", nil, 0, "Tree refresh: off\n"},
		{"the flag wins over the variable", "12", []string{"--tree-refresh=7"}, 7, "Tree refresh: every 7 s\n"},
		{"the flag turns it off", "12", []string{"--tree-refresh=0"}, 0, "Tree refresh: off\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(resetServeGlobals)
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			t.Setenv("RX_TREE_REFRESH_SECONDS", tc.env)

			run := startServeCommand(t, append([]string{"--search-root=" + t.TempDir()}, tc.args...)...)
			resp, err := http.Get(run.base + "/health")
			if err != nil {
				run.stop(t)
				t.Fatal(err)
			}
			var health rxtypes.HealthResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&health)
			_ = resp.Body.Close()
			run.stop(t)
			if decodeErr != nil {
				t.Fatalf("decode /health: %v", decodeErr)
			}

			if health.TreeRefreshSeconds != tc.want {
				t.Errorf("tree_refresh_seconds %d, want %d", health.TreeRefreshSeconds, tc.want)
			}
			if banner := run.out.String(); !strings.Contains(banner, tc.banner) {
				t.Errorf("banner lacks %q:\n%s", tc.banner, banner)
			}
		})
	}
}

// A --tree-refresh outside 0 to 3600 seconds is a usage error (exit 2)
// that names the accepted range, and no server starts.
func TestServeCommand_ATreeRefreshOutOfRangeIsAUsageError(t *testing.T) {
	for _, value := range []string{"-1", "3601"} {
		t.Run(value, func(t *testing.T) {
			t.Cleanup(resetServeGlobals)
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			cmd := NewServeCommand(&bytes.Buffer{}, "test")
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs([]string{"--tree-refresh=" + value, "--search-root=" + t.TempDir(),
				"--host=127.0.0.1", fmt.Sprintf("--port=%d", freePort(t)), "--skip-frontend"})

			// Go note: a server that starts anyway blocks Execute until a
			// signal stops it, so the command runs in its own goroutine
			// and a timeout stops it with the signal runServe handles.
			done := make(chan error, 1)
			go func() { done <- cmd.Execute() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
				<-done
				t.Fatalf("rx serve started with --tree-refresh=%s", value)
			}

			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
				t.Fatalf("error %v, want exit code %d", err, ExitUsageError)
			}
			if !strings.Contains(exitErr.Error(), "from 0 to 3600") {
				t.Errorf("message %q does not name the range", exitErr.Error())
			}
		})
	}
}
