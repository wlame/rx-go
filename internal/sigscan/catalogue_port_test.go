package sigscan

// The cases of the nine line detectors that rx ran before signatures,
// each against the built-in signature that took its place. Fixture
// files live in testdata/catalogue; the short cases are inline. Where a
// signature answers differently from the detector it replaced, the
// case says how and why.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
)

// portCase is one input and what one built-in signature finds in it.
type portCase struct {
	name string
	// fixture names a file of testdata/catalogue; text is used when it
	// is empty.
	fixture string
	text    string
	sig     string
	want    []lineSpan
}

// input returns the case's text.
func (c portCase) input(t *testing.T) string {
	t.Helper()
	if c.fixture == "" {
		return c.text
	}
	data, err := os.ReadFile(filepath.Join("testdata", "catalogue", c.fixture))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// runPortCases scans each case's text for its signature alone, with the
// built-in definition, and compares the findings' line spans.
func runPortCases(t *testing.T, cases []portCase) {
	t.Helper()
	cat := builtinCatalogue(t)
	for _, c := range cases {
		t.Run(c.name+"/"+c.sig, func(t *testing.T) {
			sig, ok := cat.Lookup(c.sig)
			if !ok {
				t.Fatalf("no built-in %s", c.sig)
			}
			res := scanOne(t, c.input(t), []*signatures.Signature{sig}, Options{})
			if got := spans(res, c.sig); !slices.Equal(got, c.want) {
				t.Errorf("findings %v, want %v", got, c.want)
			}
		})
	}
}

// joined joins lines with "\n" and ends the text with one, like lines.
func joined(ls ...string) string { return lines(ls...) }

// Python tracebacks: every traceback-python case. A chained exception
// ("During handling of the above exception...") is one finding now,
// where the detector reported each traceback of the chain apart.
func TestBuiltinPort_PythonTraceback(t *testing.T) {
	runPortCases(t, []portCase{
		{name: "single frame", fixture: "python-single-frame.log", sig: "python-traceback", want: []lineSpan{{2, 5}}},
		{name: "multi frame", fixture: "python-multi-frame.log", sig: "python-traceback", want: []lineSpan{{2, 9}}},
		{name: "chained", fixture: "python-chained.log", sig: "python-traceback", want: []lineSpan{{2, 16}}},
		{name: "at the end of the file", fixture: "python-eof.log", sig: "python-traceback", want: []lineSpan{{2, 7}}},
		{name: "no exact header", sig: "python-traceback", text: joined(
			"Traceback (something else):", `  File "/app/x.py", line 1, in <module>`, "ValueError: nope")},
		{name: "frames without the exception line", sig: "python-traceback", want: []lineSpan{{1, 3}}, text: joined(
			"Traceback (most recent call last):", `  File "/app/x.py", line 5, in foo`, "    do_thing()",
			"some random prose that terminates the traceback")},
		{name: "a blank line closes it", sig: "python-traceback", want: []lineSpan{{1, 3}}, text: joined(
			"Traceback (most recent call last):", `  File "/app/x.py", line 5, in foo`, "    do_thing()", "", "next log event")},
		{name: "exception without a message", sig: "python-traceback", want: []lineSpan{{1, 4}}, text: joined(
			"Traceback (most recent call last):", `  File "/app/x.py", line 5, in foo`, "    do_thing()", "KeyboardInterrupt", "next log event")},
		{name: "dotted exception name", sig: "python-traceback", want: []lineSpan{{1, 4}}, text: joined(
			"Traceback (most recent call last):", `  File "/app/x.py", line 5, in foo`, "    do_thing()",
			"json.decoder.JSONDecodeError: Expecting value", "next log event")},
	})
}

// JVM exceptions: every traceback-java case.
func TestBuiltinPort_JavaException(t *testing.T) {
	runPortCases(t, []portCase{
		{name: "single stack", fixture: "java-single-stack.log", sig: "java-exception", want: []lineSpan{{2, 4}}},
		{name: "caused by", fixture: "java-caused-by.log", sig: "java-exception", want: []lineSpan{{2, 12}}},
		{name: "suppressed", fixture: "java-suppressed.log", sig: "java-exception", want: []lineSpan{{2, 14}}},
		{name: "back to back", fixture: "java-multiple-stacks.log", sig: "java-exception", want: []lineSpan{{2, 4}, {5, 7}}},
		{name: "bare exception line", fixture: "java-bare-exception.log", sig: "java-exception", want: []lineSpan{{2, 4}}},
		{name: "at the end of the file", sig: "java-exception", want: []lineSpan{{1, 3}}, text: joined(
			`Exception in thread "main" java.lang.NullPointerException: x`,
			"\tat com.example.Main.m(Main.java:1)", "\tat com.example.Main.main(Main.java:2)")},
		{name: "a blank line closes it", sig: "java-exception", want: []lineSpan{{1, 2}}, text: joined(
			`Exception in thread "main" java.lang.RuntimeException: boom`, "\tat com.example.App.run(App.java:10)", "", "app shutting down")},
		{name: "no exception line", sig: "java-exception", text: joined(
			"just a log line", "\tat something that looks like a frame", "Caused by: something that looks cause-ish", "another log line")},
		{name: "not at the start of the line", sig: "java-exception", text: joined(
			`2026-04-21 12:00:00 INFO Exception in thread "main" reported by user`, "  java.lang.NullPointerException: indented")},
	})
}

// JavaScript errors: every traceback-js case.
func TestBuiltinPort_JSError(t *testing.T) {
	runPortCases(t, []portCase{
		{name: "node stack", fixture: "js-node-stack.log", sig: "js-error", want: []lineSpan{{2, 6}}},
		{name: "browser stack", fixture: "js-browser-stack.log", sig: "js-error", want: []lineSpan{{2, 4}}},
		{name: "unhandled rejection", fixture: "js-unhandled-rejection.log", sig: "js-error", want: []lineSpan{{3, 6}}},
		{name: "error line without frames", sig: "js-error", text: joined(
			"TypeError: bad input received from client", "client request rejected", "moving on")},
		{name: "error line at the end of the file", sig: "js-error", text: joined("request handled", "TypeError: very suspicious")},
		{name: "at the end of the file", sig: "js-error", want: []lineSpan{{1, 3}}, text: joined(
			"Error: boom", "    at /app/x.js:1:1", "    at /app/y.js:2:2")},
		{name: "back to back", sig: "js-error", want: []lineSpan{{2, 3}, {4, 5}}, text: joined(
			"request a", "TypeError: first failure", "    at fn (/app/a.js:1:1)", "RangeError: second failure",
			"    at fn (/app/b.js:2:2)", "request done")},
		{name: "prose", sig: "js-error", text: joined(
			"server started on port 3000", "    at first glance this looks like a frame but no colon-line-col",
			"no Error: here because this line starts with lowercase", "handled 42 requests in 1s")},
		{name: "not at the start of the line", sig: "js-error", text: joined(
			"2026-04-21 12:00:00 WARN TypeError: bad user input reported", "    at fn (/app/x.js:1:1)")},
		{name: "bare Error", sig: "js-error", want: []lineSpan{{1, 2}}, text: joined("Error: something went wrong", "    at fn (/app/x.js:1:1)")},
	})
}

// Go panics: every traceback-go case.
func TestBuiltinPort_GoPanic(t *testing.T) {
	runPortCases(t, []portCase{
		{name: "simple panic", fixture: "go-simple-panic.log", sig: "go-panic", want: []lineSpan{{2, 9}}},
		{name: "several goroutines", fixture: "go-multiple-goroutines.log", sig: "go-panic", want: []lineSpan{{2, 19}}},
		{name: "fatal error", fixture: "go-race-detector.log", sig: "go-panic", want: []lineSpan{{2, 21}}},
		{name: "signal", fixture: "go-sigsegv.log", sig: "go-panic", want: []lineSpan{{2, 14}}},
		{name: "at the end of the file", sig: "go-panic", want: []lineSpan{{1, 5}}, text: joined(
			"panic: boom", "", "goroutine 1 [running]:", "main.main()", "\t/app/main.go:10 +0x20")},
		{name: "a blank line then prose closes it", sig: "go-panic", want: []lineSpan{{1, 5}}, text: joined(
			"panic: boom", "", "goroutine 1 [running]:", "main.main()", "\t/app/main.go:10", "",
			"ordinary log line after crash", "another ordinary line")},
		{name: "two blank lines close it", sig: "go-panic", want: []lineSpan{{1, 5}}, text: joined(
			"panic: boom", "", "goroutine 1 [running]:", "main.main()", "\t/app/main.go:10", "", "", "\t/bogus/continuation.go:1")},
		{name: "back to back", sig: "go-panic", want: []lineSpan{{1, 4}, {5, 8}}, text: joined(
			"panic: first", "goroutine 1 [running]:", "main.a()", "\t/app/main.go:1",
			"panic: second", "goroutine 2 [running]:", "main.b()", "\t/app/main.go:2")},
		{name: "no panic", sig: "go-panic", text: joined(
			"just a log line", "goroutine info reported", "\tat something that looks like a frame", "another log line")},
		{name: "not at the start of the line", sig: "go-panic", text: joined(
			"2026-04-21 12:00:00 INFO panic: reported by user", "2026-04-21 12:00:01 INFO fatal error: reported by user", "  panic: indented")},
		{name: "exit status ends it", sig: "go-panic", want: []lineSpan{{1, 6}}, text: joined(
			"panic: boom", "", "goroutine 1 [running]:", "main.main()", "\t/app/main.go:10", "exit status 2", "shell prompt")},
	})
}

// Crashes: every coredump-unix case, now spread over the four crash
// signatures that replaced it. The kernel oops is found from the
// "kernel BUG at" line through the "end trace" line; the detector
// started at "Call Trace:" and ran on through every later line with a
// kernel time. A stack-smashing abort is found by glibc-abort, and the
// shell's "Aborted (core dumped)" after it by killed-by-signal too.
func TestBuiltinPort_Crashes(t *testing.T) {
	segfaultThenASan := joined("first process starting", "Segmentation fault (core dumped)",
		"==55555==ERROR: AddressSanitizer: heap-use-after-free", "    #0 0x400abc in func /app/x.c:1:1", "==55555==ABORTING", "done")
	plain := joined("application starting", "handled request /api/users in 42ms",
		"log line mentioning Segmentation fault in the middle of prose", "shutting down gracefully")
	cases := []portCase{
		{name: "segfault", fixture: "crash-segfault.log", sig: "killed-by-signal", want: []lineSpan{{3, 3}}},
		{name: "core dumped on the next line", sig: "killed-by-signal", want: []lineSpan{{2, 3}}, text: joined(
			"running binary", "Segmentation fault", "(core dumped)", "shell returned")},
		{name: "address sanitizer", fixture: "crash-asan.log", sig: "sanitizer-report", want: []lineSpan{{2, 9}}},
		{name: "kernel oops", fixture: "crash-kernel-oops.log", sig: "kernel-oops", want: []lineSpan{{2, 8}}},
		{name: "kernel lines end", sig: "kernel-oops", want: []lineSpan{{2, 4}}, text: joined(
			"normal application log line", "[   42.000000] Call Trace:", "[   42.000001]  [<ffffffff810a1234>] do_sync_write+0x56/0x78",
			"[   42.000002]  [<ffffffff810a1300>] vfs_write+0x90/0xa0", "application continues without kernel prefix", "another line")},
		{name: "stack smashing", fixture: "crash-stack-smashing.log", sig: "glibc-abort", want: []lineSpan{{3, 11}}},
		{name: "stack smashing", fixture: "crash-stack-smashing.log", sig: "killed-by-signal", want: []lineSpan{{11, 11}}},
		{name: "a blank line closes the abort", sig: "glibc-abort", want: []lineSpan{{2, 4}}, text: joined(
			"running binary", "*** stack smashing detected ***: terminated", "======= Backtrace: =========",
			"/lib/x86_64-linux-gnu/libc.so.6(+0x1210e2)[0x7f2e9c8bf0e2]", "", "next log line after blank")},
		{name: "sanitizer at the end of the file", sig: "sanitizer-report", want: []lineSpan{{2, 3}}, text: joined(
			"running binary", "==99999==ERROR: AddressSanitizer: stack-buffer-overflow", "    #0 0x55a7d8f2a1bf in main /app/src/main.c:12:11")},
		{name: "back to back", text: segfaultThenASan, sig: "killed-by-signal", want: []lineSpan{{2, 2}}},
		{name: "back to back", text: segfaultThenASan, sig: "sanitizer-report", want: []lineSpan{{3, 5}}},
	}
	for _, sig := range []string{"killed-by-signal", "glibc-abort", "sanitizer-report", "kernel-oops"} {
		cases = append(cases, portCase{name: "no crash", text: plain, sig: sig})
	}
	openers := []struct {
		line string
		sig  string
	}{
		{"Segmentation fault", "killed-by-signal"},
		{"Segmentation fault (core dumped)", "killed-by-signal"},
		{"*** stack smashing detected ***: terminated", "glibc-abort"},
		{"==42==ERROR: AddressSanitizer: heap-buffer-overflow", "sanitizer-report"},
		{"[   12.345] Call Trace:", "kernel-oops"},
		{"[12.345] Call Trace:", "kernel-oops"},
	}
	for _, o := range openers {
		cases = append(cases, portCase{name: "opener " + o.line, text: joined(o.line), sig: o.sig, want: []lineSpan{{1, 1}}})
	}
	for _, line := range []string{"", "random log line", "got Segmentation fault during run", "prefix *** stack smashing detected ***", "[   12.345] regular kernel log line"} {
		for _, sig := range []string{"killed-by-signal", "glibc-abort", "sanitizer-report", "kernel-oops"} {
			cases = append(cases, portCase{name: fmt.Sprintf("not an opener %q", line), text: joined(line), sig: sig})
		}
	}
	runPortCases(t, cases)
}

// Secrets: every secrets-scan case, against the five signatures that
// replaced it. A secret is a finding of its line, with the secret as
// the value span; a private key is found through its END line, where
// the detector found the BEGIN line only.
func TestBuiltinPort_Secrets(t *testing.T) {
	cases := []portCase{
		{name: "aws key", fixture: "secret-aws-key.log", sig: "aws-access-key", want: []lineSpan{{3, 3}}},
		{name: "github token", fixture: "secret-github-pat.log", sig: "github-token", want: []lineSpan{{2, 2}}},
		{name: "slack token", fixture: "secret-slack-token.log", sig: "slack-token", want: []lineSpan{{2, 2}}},
		{name: "jwt", fixture: "secret-jwt.log", sig: "jwt", want: []lineSpan{{2, 2}}},
		{name: "private key", fixture: "secret-pem-key.log", sig: "private-key", want: []lineSpan{{2, 4}}},
	}
	for _, sig := range []string{"aws-access-key", "github-token", "slack-token", "jwt", "private-key"} {
		cases = append(cases, portCase{name: "look-alikes", fixture: "secret-negatives.log", sig: sig})
	}
	twoOnALine := joined("ctx AKIAIOSFODNN7EXAMPLE and ghp_0123456789abcdefABCDEF0123456789wxyz together")
	cases = append(cases,
		portCase{name: "two on a line", text: twoOnALine, sig: "aws-access-key", want: []lineSpan{{1, 1}}},
		portCase{name: "two on a line", text: twoOnALine, sig: "github-token", want: []lineSpan{{1, 1}}})
	for _, token := range []string{"xoxb-12345-67890-AbCdEfGh", "xoxa-2-12345-67890-AbCdEfGh", "xoxp-12345-67890-AbCdEfGh",
		"xoxr-12345-67890-AbCdEfGh", "xoxs-12345-67890-AbCdEfGh"} {
		cases = append(cases, portCase{name: "slack " + token[:4], text: joined("TOKEN=" + token), sig: "slack-token", want: []lineSpan{{1, 1}}})
	}
	for _, header := range []string{"-----BEGIN PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----", "-----BEGIN EC PRIVATE KEY-----",
		"-----BEGIN DSA PRIVATE KEY-----", "-----BEGIN OPENSSH PRIVATE KEY-----"} {
		cases = append(cases, portCase{name: header, text: joined(header), sig: "private-key", want: []lineSpan{{1, 1}}})
	}
	runPortCases(t, cases)
	t.Run("the value span is the key", func(t *testing.T) {
		sig, _ := builtinCatalogue(t).Lookup("aws-access-key")
		res := scanOne(t, joined("prefix-padding-here AKIAIOSFODNN7EXAMPLE trailing-padding"), []*signatures.Signature{sig}, Options{})
		f := res.BySignature["aws-access-key"].Findings
		if len(f) != 1 || deref(f[0].ValueStart) != 20 || deref(f[0].ValueEnd) != 40 {
			t.Fatalf("findings %s, want one with the value at bytes 20 to 40", describe(f))
		}
	})
}

// Long lines: the detector compared a line with the whole file's
// percentiles; long-line compares it with the lines around it. A long
// line among short ones is still found; lines that are long like their
// neighbors (2,048, 1,000 and 1,500 bytes) no longer are.
func TestBuiltinPort_LongLine(t *testing.T) {
	many := make([]string, 0, 101)
	for i := 0; i < 50; i++ {
		many = append(many, strings.Repeat("s", 10))
	}
	many = append(many, strings.Repeat("L", 5000))
	for i := 0; i < 50; i++ {
		many = append(many, strings.Repeat("s", 10))
	}
	runPortCases(t, []portCase{
		{name: "one long line", sig: "long-line", want: []lineSpan{{3, 3}}, text: joined(
			strings.Repeat("a", 10), strings.Repeat("a", 10), strings.Repeat("b", 4000), strings.Repeat("a", 10))},
		{name: "one long line among a hundred", sig: "long-line", want: []lineSpan{{51, 51}}, text: joined(many...)},
		{name: "no line of a kilobyte", sig: "long-line", text: joined(strings.Repeat("a", 200), strings.Repeat("a", 100), strings.Repeat("a", 500))},
		{name: "long like the neighbors", sig: "long-line", text: joined(strings.Repeat("a", 2048), strings.Repeat("a", 1000), strings.Repeat("a", 1500))},
		{name: "growing lengths", sig: "long-line", text: joined(
			strings.Repeat("a", 2000), strings.Repeat("a", 3000), strings.Repeat("a", 5000), strings.Repeat("a", 9000))},
	})
}

// Repeated lines: every repeat-identical case (five lines or more).
func TestBuiltinPort_RepeatedLines(t *testing.T) {
	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = "spam"
	}
	runPortCases(t, []portCase{
		{name: "five lines", sig: "repeated-lines", want: []lineSpan{{1, 5}}, text: joined("same", "same", "same", "same", "same")},
		{name: "four lines", sig: "repeated-lines", text: joined("x", "x", "x", "x", "y")},
		{name: "two runs", sig: "repeated-lines", want: []lineSpan{{1, 6}, {8, 12}}, text: joined(
			"aa", "aa", "aa", "aa", "aa", "aa", "break", "bb", "bb", "bb", "bb", "bb")},
		{name: "at the end of the file", sig: "repeated-lines", want: []lineSpan{{1, 5}}, text: joined("eof", "eof", "eof", "eof", "eof")},
		{name: "short runs only", sig: "repeated-lines", text: joined("a", "b", "c", "d", "d", "e", "f", "f", "f", "f")},
		{name: "one line", sig: "repeated-lines", text: joined("only")},
		{name: "a hundred lines", sig: "repeated-lines", want: []lineSpan{{1, 100}}, text: joined(hundred...)},
		{name: "between other lines", sig: "repeated-lines", want: []lineSpan{{2, 7}}, text: joined(
			"intro", "repeat", "repeat", "repeat", "repeat", "repeat", "repeat", "outro")},
	})
	t.Run("offsets", func(t *testing.T) {
		sig, _ := builtinCatalogue(t).Lookup("repeated-lines")
		f := scanOne(t, joined("same", "same", "same", "same", "same"), []*signatures.Signature{sig}, Options{}).BySignature["repeated-lines"].Findings
		if len(f) != 1 || f[0].StartOffset != 0 || f[0].EndOffset != 25 {
			t.Fatalf("findings %s, want one from byte 0 to 25", describe(f))
		}
	})
}

// JSON blocks: every json-blob-multiline case. An object cut off by the
// end of the file is a finding through its last line, as the detector
// reported it.
func TestBuiltinPort_JSONBlock(t *testing.T) {
	runPortCases(t, []portCase{
		{name: "simple object", fixture: "json-simple-object.log", sig: "json-block", want: []lineSpan{{3, 6}}},
		{name: "nested object", fixture: "json-nested-object.log", sig: "json-block", want: []lineSpan{{2, 9}}},
		{name: "array", fixture: "json-simple-array.log", sig: "json-block", want: []lineSpan{{2, 6}}},
		{name: "braces in strings", fixture: "json-strings-with-braces.log", sig: "json-block", want: []lineSpan{{2, 7}}},
		{name: "cut off", fixture: "json-unterminated.log", sig: "json-block", want: []lineSpan{{2, 7}}},
		{name: "no opener line", sig: "json-block", text: joined(
			"plain log message", "another line with { somewhere in the middle", "or with } closer-ish chars too", "done")},
		{name: "opener with more on its line", sig: "json-block", text: joined(`request: { "action": "foo" }`, `{ inline stuff here }`)},
		{name: "closer with more on its line", sig: "json-block", text: joined("{", "} extra stuff here", "done")},
		{name: "wrong closer", sig: "json-block", text: joined("{", `  "key": "value"`, "]", "done")},
		{name: "closer at another indent", sig: "json-block", text: joined(
			"{", `  "nested": {`, `    "inner": "value"`, `  }`, `    }`, "done")},
		{name: "unclosed at the end of the file", sig: "json-block", want: []lineSpan{{1, 2}}, text: joined("{", `  "key": "value"`)},
		{name: "two blocks", sig: "json-block", want: []lineSpan{{2, 4}, {6, 9}}, text: joined(
			"first event", "{", `  "id": 1`, "}", "second event", "[", `  "a",`, `  "b"`, "]", "done")},
	})
}
