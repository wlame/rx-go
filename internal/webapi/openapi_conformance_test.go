package webapi

// The golden OpenAPI document is the contract rx-viewer generates its
// types from. openapi_test.go checks that the router describes itself
// as the document says; the tests here check that the router *answers*
// as the document says. They start the real router over small fixtures
// (plain, gzip, seekable zstd, empty, 3- and 7-line files), call every
// operation for its success and its main error statuses, and validate
// each body against the schema the document declares for that status
// and content type. A status the operation does not declare fails the
// test, and so does an operation no call reached.
//
// The validator is a full JSON Schema 2020-12 implementation, not
// huma's own: huma's matches property names without regard to case and
// accepts null for any optional property, which would let through the
// kind of mismatch these tests exist to catch.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
)

// goldenResourceURL is the name the golden document is registered under
// in the schema compiler; schema locations are fragments of it.
const goldenResourceURL = "file:///openapi.golden.json"

// unknownTaskID is a well-formed task id no task has.
const unknownTaskID = "00000000-0000-0000-0000-000000000000"

// contract is the golden document, ready to hand out the compiled
// schema of any response it declares.
type contract struct {
	doc      map[string]any
	compiler *jsonschema.Compiler
}

func loadContract(t *testing.T) *contract {
	t.Helper()
	f, err := os.Open(goldenPath)
	if err != nil {
		t.Fatalf("open golden spec: %v", err)
	}
	defer func() { _ = f.Close() }()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("parse golden spec: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	// OpenAPI 3.1 schemas are JSON Schema 2020-12. Formats such as
	// date-time are checked, not only annotated.
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	if err := compiler.AddResource(goldenResourceURL, doc); err != nil {
		t.Fatalf("register golden spec: %v", err)
	}
	return &contract{doc: doc.(map[string]any), compiler: compiler}
}

// operations lists every "METHOD /template" the document declares.
func (c *contract) operations() []string {
	var ops []string
	for template, item := range c.doc["paths"].(map[string]any) {
		for method := range item.(map[string]any) {
			ops = append(ops, strings.ToUpper(method)+" "+template)
		}
	}
	sort.Strings(ops)
	return ops
}

// response returns the response object the operation declares for
// status, or nil when it declares none. The "default" response does not
// count: a status the operation can answer must be listed by number.
func (c *contract) response(method, template string, status int) map[string]any {
	item, _ := c.doc["paths"].(map[string]any)[template].(map[string]any)
	op, _ := item[strings.ToLower(method)].(map[string]any)
	responses, _ := op["responses"].(map[string]any)
	declared, _ := responses[strconv.Itoa(status)].(map[string]any)
	return declared
}

// schemaFor compiles the schema declared for one status and media type
// of one operation.
func (c *contract) schemaFor(method, template string, status int, mediaType string) (*jsonschema.Schema, error) {
	location := goldenResourceURL + "#" + jsonPointer(
		"paths", template, strings.ToLower(method), "responses", strconv.Itoa(status), "content", mediaType, "schema")
	return c.compiler.Compile(location)
}

// jsonPointer joins tokens into a JSON pointer (RFC 6901), escaped for
// use as a URL fragment.
func jsonPointer(tokens ...string) string {
	var b strings.Builder
	for _, token := range tokens {
		token = strings.ReplaceAll(token, "~", "~0")
		token = strings.ReplaceAll(token, "/", "~1")
		b.WriteString("/" + url.PathEscape(token))
	}
	return b.String()
}

// apiCall is one request and the status it should get.
type apiCall struct {
	label    string
	method   string
	template string      // the operation's path as the document lists it
	path     string      // the concrete path requested
	query    url.Values  // nil for none
	body     any         // nil for none; a string is sent as it is
	header   http.Header // extra request headers; nil for none
	want     int         // 0 accepts any status the operation declares
}

// conformanceRun sends calls to one server and checks each answer
// against the contract.
type conformanceRun struct {
	t        *testing.T
	base     string
	contract *contract
	answered map[string][]int // "METHOD /template" → statuses seen
}

// check sends call, checks its status and body against the contract in
// a subtest named after the call, and returns the decoded body (nil
// when it is not a JSON object).
func (r *conformanceRun) check(call apiCall) map[string]any {
	r.t.Helper()
	status, mediaType, raw := r.send(call)
	op := call.method + " " + call.template
	r.answered[op] = append(r.answered[op], status)

	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	r.t.Run(call.label, func(t *testing.T) {
		if call.want != 0 && status != call.want {
			t.Errorf("status %d, want %d; body %s", status, call.want, bodyExcerpt(raw))
		}
		if problem := r.contract.mismatch(call.method, call.template, status, mediaType, raw); problem != "" {
			t.Error(problem)
		}
	})
	return decoded
}

// mismatch reports how an answer departs from the contract, or "" when
// it conforms: a status the operation does not declare, a media type
// the status does not declare, a body that is not JSON, or a body the
// declared schema rejects.
func (c *contract) mismatch(method, template string, status int, mediaType string, raw []byte) string {
	op := method + " " + template
	declared := c.response(method, template, status)
	if declared == nil {
		return fmt.Sprintf("%s answered %d, which the operation does not declare; body %s", op, status, bodyExcerpt(raw))
	}
	content, _ := declared["content"].(map[string]any)
	if _, ok := content[mediaType]; !ok {
		return fmt.Sprintf("%s %d answered %q, the document declares %v", op, status, mediaType, keys(content))
	}
	schema, err := c.schemaFor(method, template, status, mediaType)
	if err != nil {
		return fmt.Sprintf("compile the schema of %s %d: %v", op, status, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Sprintf("%s %d: body is not JSON: %v; body %s", op, status, err, bodyExcerpt(raw))
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Sprintf("body does not match the schema of %s %d:\n%v\nbody %s", op, status, err, bodyExcerpt(raw))
	}
	return ""
}

// send performs the request and returns the status, the media type of
// the answer and its body.
func (r *conformanceRun) send(call apiCall) (int, string, []byte) {
	r.t.Helper()
	target := r.base + call.path
	if call.query != nil {
		target += "?" + call.query.Encode()
	}
	var body io.Reader
	switch b := call.body.(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			r.t.Fatalf("%s: encode body: %v", call.label, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(call.method, target, body)
	if err != nil {
		r.t.Fatalf("%s: new request: %v", call.label, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range call.header {
		req.Header[name] = values
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("%s: %v", call.label, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("%s: read body: %v", call.label, err)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return resp.StatusCode, mediaType, raw
}

// finishTask waits for a task to leave queued and running, then checks
// its answer as a call of GET /v1/tasks/{task_id}.
func (r *conformanceRun) finishTask(label string, created map[string]any) map[string]any {
	r.t.Helper()
	taskID, _ := created["task_id"].(string)
	if taskID == "" {
		r.t.Fatalf("%s: no task_id in %v", label, created)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, _, raw := r.send(apiCall{label: label, method: http.MethodGet, path: "/v1/tasks/" + taskID})
		var task struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &task)
		if task.Status == "completed" || task.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r.check(apiCall{label: label, method: http.MethodGet, template: "/v1/tasks/{task_id}",
		path: "/v1/tasks/" + taskID, want: http.StatusOK})
}

// requireEveryOperationSucceeded fails the test for each operation of
// the document that no call reached with a 2xx answer.
func (r *conformanceRun) requireEveryOperationSucceeded() {
	r.t.Helper()
	for _, op := range r.contract.operations() {
		succeeded := false
		for _, status := range r.answered[op] {
			succeeded = succeeded || (status >= 200 && status < 300)
		}
		if !succeeded {
			r.t.Errorf("%s: no call got a success answer (statuses seen: %v)", op, r.answered[op])
		}
	}
}

// conformanceFixtures writes the files the calls use under a fresh
// search root and returns the root.
func conformanceFixtures(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name string, body []byte) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	var log bytes.Buffer
	for n := 1; n <= 3000; n++ {
		level := [...]string{"INFO", "INFO", "WARN", "INFO", "ERROR"}[n%5]
		fmt.Fprintf(&log, "LINE %d %s request served in %d ms\n", n, level, n%311)
		if n%700 == 0 {
			fmt.Fprintf(&log, "Traceback (most recent call last):\n  File \"app.py\", line %d, in handle\nValueError: bad input\n", n)
		}
	}
	write("app.log", log.Bytes())
	write("app.log.gz", gzipped(t, log.Bytes()))
	// Truncated gzip: a compress task over it fails.
	broken := gzipped(t, log.Bytes())
	write("broken.log.gz", broken[:len(broken)/2])
	write("three.log", []byte("LINE 1 alpha\nLINE 2 beta\nLINE 3 gamma\n"))
	write("seven.log", []byte("LINE 1\nLINE 2\nLINE 3\nLINE 4\nLINE 5\nLINE 6\nLINE 7\n"))
	write("empty.log", nil)
	write("sub/other.log", []byte("LINE 1 nested ERROR\n"))
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root
}

func gzipped(t *testing.T, text []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(text)
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return buf.Bytes()
}

func TestOpenAPIConformance_EveryAnswerMatchesTheGoldenDocument(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	root := conformanceFixtures(t)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	manager := tasks.New(tasks.Config{})
	ts := httptest.NewServer(NewServer(Config{AppVersion: "conformance-test", RipgrepPath: rgPath, TaskManager: manager}))
	t.Cleanup(ts.Close)

	run := &conformanceRun{t: t, base: ts.URL, contract: loadContract(t), answered: map[string][]int{}}
	at := func(name string) string { return filepath.Join(root, name) }
	q := func(pairs ...string) url.Values {
		v := url.Values{}
		for i := 0; i+1 < len(pairs); i += 2 {
			v.Add(pairs[i], pairs[i+1])
		}
		return v
	}
	get := func(label, template string, query url.Values, want int) map[string]any {
		return run.check(apiCall{label: label, method: http.MethodGet, template: template, path: template, query: query, want: want})
	}
	post := func(label, template string, body any, want int) map[string]any {
		return run.check(apiCall{label: label, method: http.MethodPost, template: template, path: template, body: body, want: want})
	}

	get("health", "/health", nil, http.StatusOK)
	get("detectors", "/v1/detectors", nil, http.StatusOK)

	get("tree of the roots", "/v1/tree", nil, http.StatusOK)
	get("tree of a directory", "/v1/tree", q("path", root), http.StatusOK)
	get("tree of a file", "/v1/tree", q("path", at("app.log")), http.StatusBadRequest)
	get("tree outside the root", "/v1/tree", q("path", "/etc"), http.StatusForbidden)
	get("tree of a missing path", "/v1/tree", q("path", at("nope")), http.StatusNotFound)

	get("trace plain", "/v1/trace", q("path", at("app.log"), "regexp", "ERROR"), http.StatusOK)
	get("trace capped, two files, two patterns", "/v1/trace",
		q("path", at("app.log"), "path", at("app.log.gz"), "regexp", "ERROR", "regexp", "WARN", "max_results", "5"), http.StatusOK)
	get("trace a directory, ignoring case", "/v1/trace", q("path", root, "regexp", "traceback", "ignore_case", "true"), http.StatusOK)
	get("trace with context lines", "/v1/trace",
		q("path", at("app.log"), "regexp", "ValueError", "before", "2", "after", "1"), http.StatusOK)
	get("trace without a match", "/v1/trace", q("path", at("app.log"), "regexp", "zzqqxx"), http.StatusOK)
	get("trace an empty directory", "/v1/trace", q("path", at("emptydir"), "regexp", "a"), http.StatusOK)
	get("trace an empty file", "/v1/trace", q("path", at("empty.log"), "regexp", "a"), http.StatusOK)
	get("trace an invalid regex", "/v1/trace", q("path", at("app.log"), "regexp", "a("), http.StatusBadRequest)
	get("trace an invalid PCRE2 pattern", "/v1/trace", q("path", at("app.log"), "regexp", "(", "pcre2", "true"), http.StatusBadRequest)
	get("trace a missing file", "/v1/trace", q("path", at("nope.log"), "regexp", "a"), http.StatusNotFound)
	get("trace outside the root", "/v1/trace", q("path", "/etc/hosts", "regexp", "a"), http.StatusForbidden)
	get("trace without a pattern", "/v1/trace", q("path", at("app.log")), http.StatusUnprocessableEntity)
	get("trace with a bad boolean", "/v1/trace", q("path", at("app.log"), "regexp", "a", "ignore_case", "maybe"),
		http.StatusUnprocessableEntity)

	get("samples by line with context", "/v1/samples", q("path", at("app.log"), "lines", "100,200-205", "context", "2"), http.StatusOK)
	get("samples by offset", "/v1/samples", q("path", at("app.log"), "offsets", "0,5000"), http.StatusOK)
	get("samples of a gzip file", "/v1/samples", q("path", at("app.log.gz"), "lines", "2500"), http.StatusOK)
	get("samples of the 3-line file", "/v1/samples", q("path", at("three.log"), "lines", "1-3"), http.StatusOK)
	get("samples past the end", "/v1/samples", q("path", at("seven.log"), "lines", "99"), 0)
	get("samples without lines or offsets", "/v1/samples", q("path", at("app.log")), http.StatusBadRequest)
	get("samples with both lines and offsets", "/v1/samples", q("path", at("app.log"), "lines", "1", "offsets", "0"),
		http.StatusBadRequest)
	get("samples outside the root", "/v1/samples", q("path", "/etc/hosts", "lines", "1"), http.StatusForbidden)
	get("samples of a missing file", "/v1/samples", q("path", at("nope.log"), "lines", "1"), http.StatusNotFound)
	get("samples without a path", "/v1/samples", q("lines", "1"), http.StatusUnprocessableEntity)

	get("index before it is built", "/v1/index", q("path", at("app.log")), http.StatusNotFound)
	get("index outside the root", "/v1/index", q("path", "/etc/hosts"), http.StatusForbidden)
	get("index without a path", "/v1/index", nil, http.StatusUnprocessableEntity)
	indexed := post("index with analysis", "/v1/index", map[string]any{"path": at("app.log"), "analyze": true}, http.StatusOK)
	run.finishTask("finished index task", indexed)
	get("index after it is built", "/v1/index", q("path", at("app.log")), http.StatusOK)
	post("index a file below the threshold", "/v1/index", map[string]any{"path": at("sub/other.log")}, http.StatusBadRequest)
	post("index a missing file", "/v1/index", map[string]any{"path": at("nope.log")}, http.StatusNotFound)
	post("index a file outside the root", "/v1/index", map[string]any{"path": "/etc/hosts", "analyze": true}, http.StatusForbidden)
	post("index with an unknown field", "/v1/index", map[string]any{"path": at("app.log"), "no_such_field": true},
		http.StatusUnprocessableEntity)
	post("index with a body that is not JSON", "/v1/index", "{not json", http.StatusBadRequest)

	compressed := post("compress", "/v1/compress", map[string]any{
		"input_path": at("app.log"), "output_path": at("app.log.zst"), "frame_size": "16K",
		"compression_level": 3, "build_index": true, "force": true,
	}, http.StatusOK)
	run.finishTask("finished compress task", compressed)
	get("trace a seekable zstd file", "/v1/trace", q("path", at("app.log.zst"), "regexp", "ERROR", "max_results", "3"), http.StatusOK)
	get("samples of a seekable zstd file", "/v1/samples", q("path", at("app.log.zst"), "lines", "2000"), http.StatusOK)
	get("index of a seekable zstd file", "/v1/index", q("path", at("app.log.zst")), http.StatusOK)
	minimal := post("compress with only input_path", "/v1/compress", map[string]any{"input_path": at("seven.log")}, http.StatusOK)
	run.finishTask("finished minimal compress task", minimal)
	failing := post("compress a truncated gzip file", "/v1/compress",
		map[string]any{"input_path": at("broken.log.gz"), "build_index": false}, http.StatusOK)
	run.finishTask("failed compress task", failing)
	post("compress over an existing output", "/v1/compress", map[string]any{"input_path": at("seven.log")}, http.StatusBadRequest)
	post("compress a seekable zstd file without force", "/v1/compress",
		map[string]any{"input_path": at("app.log.zst"), "output_path": at("again.zst")}, http.StatusBadRequest)
	post("compress a missing file", "/v1/compress", map[string]any{"input_path": at("nope.log")}, http.StatusNotFound)
	post("compress outside the root", "/v1/compress", map[string]any{"input_path": "/etc/hosts"}, http.StatusForbidden)
	post("compress at level 99", "/v1/compress", map[string]any{"input_path": at("three.log"), "compression_level": 99, "force": true},
		http.StatusUnprocessableEntity)

	run.check(apiCall{label: "task that does not exist", method: http.MethodGet, template: "/v1/tasks/{task_id}",
		path: "/v1/tasks/" + unknownTaskID, want: http.StatusNotFound})

	// A task the manager holds as queued makes the next request for the
	// same file a conflict, without racing a real task to its end.
	busy, err := paths.ValidatePathWithinRoots(at("three.log"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	manager.Create(busy, "index")
	post("index while a task runs for the file", "/v1/index", map[string]any{"path": at("three.log"), "analyze": true},
		http.StatusConflict)
	post("compress while a task runs for the file", "/v1/compress",
		map[string]any{"input_path": at("three.log"), "force": true}, http.StatusConflict)

	run.requireEveryOperationSucceeded()
}

func TestOpenAPIConformance_TraceWithoutRipgrepAnswersAsDeclared(t *testing.T) {
	ts := httptest.NewServer(NewServer(Config{AppVersion: "conformance-test"}))
	t.Cleanup(ts.Close)
	run := &conformanceRun{t: t, base: ts.URL, contract: loadContract(t), answered: map[string][]int{}}

	run.check(apiCall{label: "trace without ripgrep", method: http.MethodGet, template: "/v1/trace", path: "/v1/trace",
		query: url.Values{"path": {"/tmp/any.log"}, "regexp": {"x"}}, want: http.StatusServiceUnavailable})
}

// A lookup in a file whose index is still being built answers 202 with
// the build's task once the server's wait runs out. A queued index task
// the manager holds for the file stands in for a long build, so the 202
// does not depend on how fast a real one is.
func TestOpenAPIConformance_SamplesWhileTheIndexBuildsAnswersAsDeclared(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := conformanceFixtures(t)
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	manager := tasks.New(tasks.Config{})
	ts := httptest.NewServer(NewServer(Config{
		AppVersion: "conformance-test", TaskManager: manager, SamplesIndexWait: 10 * time.Millisecond,
	}))
	t.Cleanup(ts.Close)
	run := &conformanceRun{t: t, base: ts.URL, contract: loadContract(t), answered: map[string][]int{}}

	held, err := paths.ValidatePathWithinRoots(filepath.Join(root, "app.log.gz"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	task, _ := manager.Create(held, "index")
	pending := run.check(apiCall{label: "samples while the index builds", method: http.MethodGet,
		template: "/v1/samples", path: "/v1/samples",
		query: url.Values{"path": {held}, "lines": {"10"}}, header: http.Header{"Prefer": {"respond-async"}},
		want: http.StatusAccepted})
	if pending["task_id"] != task.TaskID {
		t.Fatalf("202 names task %v, want the build's %s", pending["task_id"], task.TaskID)
	}
	run.check(apiCall{label: "the build's task while it runs", method: http.MethodGet, template: "/v1/tasks/{task_id}",
		path: "/v1/tasks/" + task.TaskID, want: http.StatusOK})
}

func TestContractMismatch_FindsEachKindOfDeparture(t *testing.T) {
	c := loadContract(t)
	const method, template = http.MethodPost, "/v1/compress"
	cases := []struct {
		name      string
		status    int
		mediaType string
		body      string
		conforms  bool
	}{
		{"a conflict body as declared", 409, "application/json", `{"detail":"busy","task_id":"t1"}`, true},
		{"a required property missing", 409, "application/json", `{"detail":"busy"}`, false},
		{"a property of the wrong type", 409, "application/json", `{"detail":"busy","task_id":7}`, false},
		{"null for a property that is not nullable", 409, "application/json", `{"detail":"busy","task_id":null}`, false},
		{"a property the schema does not list", 409, "application/json", `{"detail":"busy","task_id":"t1","extra":1}`, false},
		{"a property name in another case", 409, "application/json", `{"Detail":"busy","task_id":"t1"}`, false},
		{"a status the operation does not declare", 418, "application/json", `{"detail":"teapot"}`, false},
		{"a media type the status does not declare", 409, "text/plain", `{"detail":"busy","task_id":"t1"}`, false},
		{"a body that is not JSON", 409, "application/json", `busy`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problem := c.mismatch(method, template, tc.status, tc.mediaType, []byte(tc.body))
			if tc.conforms && problem != "" {
				t.Errorf("reported a conforming answer: %s", problem)
			}
			if !tc.conforms && problem == "" {
				t.Error("did not report the departure")
			}
		})
	}
}

func TestOpenAPIConformance_EveryV1OperationAnswersAMissingTokenAsDeclared(t *testing.T) {
	ts := newTokenServer(t, testToken)
	run := &conformanceRun{t: t, base: ts.URL, contract: loadContract(t), answered: map[string][]int{}}

	for _, op := range run.contract.operations() {
		method, template, _ := strings.Cut(op, " ")
		if !strings.HasPrefix(template, "/v1/") {
			continue
		}
		call := apiCall{label: op, method: method, template: template,
			path: strings.ReplaceAll(template, "{task_id}", unknownTaskID), want: http.StatusUnauthorized}
		if method == http.MethodPost {
			call.body = map[string]any{}
		}
		run.check(call)
	}
}

// bodyExcerpt shortens a body for a failure message.
func bodyExcerpt(raw []byte) string {
	const limit = 600
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "…"
}

// keys lists a map's keys in order, for a failure message.
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
