package clicommand

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/internal/webapi"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// GET /v1/trace with signatures answers what `rx trace --signatures
// --json` prints for the same file: the same findings with their lines,
// summary and selection, whether the scan ran in the CLI or in the
// server, and with no_cache too. Only the fields that say how an answer
// was produced (request_id, time, cli_command, file_chunks) may differ;
// the server's cli_command is the command that gives the answer.
func TestTraceSignatures_HTTPAnswerIsTheCLIAnswer(t *testing.T) {
	withTraceSignatures(t)
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	path := writeText(t, t.TempDir(), "app.log", tracebackLog)
	// The catalogue as `rx serve` loads it, from the same search path.
	cat := signatures.Load(signatures.LoadOptions{CheckExamples: sigscan.CheckExamples})
	opts := webapi.FindingsOptionsFromEnv()
	ts := httptest.NewServer(webapi.NewServer(webapi.Config{RipgrepPath: rg, Signatures: cat, Findings: &opts}))
	t.Cleanup(ts.Close)
	httpTrace := func(extra url.Values) rxtypes.TraceResponse {
		t.Helper()
		q := url.Values{"path": {path}, "signatures": {"traceback,error-line"}}
		for k, v := range extra {
			q[k] = v
		}
		resp, err := http.Get(ts.URL + "/v1/trace?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/trace: %d %s", resp.StatusCode, raw)
		}
		var answer rxtypes.TraceResponse
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatal(err)
		}
		return answer
	}

	fromServer := httpTrace(nil) // the server scans and caches
	fromCLI := traceSignaturesJSON(t, "--signatures=traceback,error-line", path)
	if len(fromCLI.Findings) != 3 || len(fromCLI.Findings[0].Lines) == 0 {
		t.Fatalf("CLI findings %+v", fromCLI.Findings)
	}
	traceanswer.RequireSame(t, "server, then CLI from the cache", fromServer, fromCLI)
	traceanswer.RequireSame(t, "server without the cache", httpTrace(url.Values{"no_cache": {"true"}}), fromCLI)
	if want := "rx trace " + path + " --signatures=traceback,error-line"; fromServer.CLICommand == nil || *fromServer.CLICommand != want {
		t.Errorf("cli_command %v, want %q", fromServer.CLICommand, want)
	}

	withPattern := httpTrace(url.Values{"regexp": {"timeout"}, "context": {"1"}})
	cliPattern := traceSignaturesJSON(t, "--signatures=traceback,error-line", "-e", "timeout", "--context=1", path)
	traceanswer.RequireSame(t, "a pattern and signatures, with context", withPattern, cliPattern)
}
