package main

// The rx logs examples of the docs run here, against a generated host
// whose /var/log and /srv/app/logs hold rotated logs of the kinds the
// pages show. Every `rx logs` command in a shell fence of the README or
// a page under docs/ must exit 0 there, and may name no path outside
// those two directories, absolute or relative (toHost). The commands of
// one fence run in order on one cache of their own, so a fence can
// index a chain and then read it. Where a fence ends with an rx logs
// command and a ```text fence follows it, that text is what the last
// command prints: the test compares it with the real output, line by
// line, after normalize (spacing, fingerprints, sizes, times taken and
// request ids vary). A doc line `...` stands for any number of lines,
// and `…` inside a line for any text. A `--json` command's output must
// decode into its wire type with no unknown field.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// docFence is one fenced block of a Markdown page.
type docFence struct {
	language string   // the info string after the opening backticks
	line     int      // the line number of the opening backticks
	lines    []string // the lines inside, continuation lines joined
	// adjacent says that only blank lines stand between the previous
	// fence's closing backticks and this one's opening.
	adjacent bool
}

// docFences returns the fenced blocks of page in order.
func docFences(t *testing.T, page string) []docFence {
	t.Helper()
	body, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	var fences []docFence
	var current *docFence
	onlyBlankSinceFence := false
	pending := ""
	for number, text := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "```") {
			if current == nil {
				current = &docFence{
					language: strings.TrimPrefix(trimmed, "```"),
					line:     number + 1,
					adjacent: onlyBlankSinceFence,
				}
				continue
			}
			fences = append(fences, *current)
			current, onlyBlankSinceFence = nil, true
			continue
		}
		if current == nil {
			if trimmed != "" {
				onlyBlankSinceFence = false
			}
			continue
		}
		if continued, ok := strings.CutSuffix(trimmed, `\`); ok {
			pending += continued + " "
			continue
		}
		current.lines = append(current.lines, pending+text)
		pending = ""
	}
	return fences
}

// logsDocDirs are the directories the examples name, each mapped to the
// directory of the generated host that stands for it.
type logsDocDirs map[string]string

// docRunDir is the documented directory the commands of the pages run
// in: rx runs in the directory that stands for it, so a relative word
// names a path from there.
const docRunDir = "/var/log"

// toHost rewrites one word of a documented command onto the generated
// host: a path under one of the documented directories, written as the
// word or as the value of a long flag (`--search-root=/var/log`), moves
// to the directory that stands for it, and any other word stays as it
// is. Every word is then read as the path it would name, cleaned, an
// absolute one as it is and a relative one from the run directory
// (docRunDir), and refused with an error unless that path lies in a
// generated directory: no doc example runs on the real filesystem,
// neither through another absolute path nor through `..`. A pattern
// that would name a path outside is refused too, which a page avoids
// by writing it another way. The words come with their quotes removed
// (rxCommandWords), so a quoted path is checked as the shell passes it.
func (d logsDocDirs) toHost(word string) (string, error) {
	flag, value := "", word
	if name, flagValue, isFlag := strings.Cut(word, "="); isFlag && strings.HasPrefix(name, "-") {
		flag, value = name+"=", flagValue
	}
	moved, named := value, filepath.Join(d[docRunDir], value)
	if doc, real, found := d.documentedDirOf(value); found {
		moved = real + strings.TrimPrefix(value, doc)
		named = filepath.Clean(moved)
	} else if filepath.IsAbs(value) {
		return "", fmt.Errorf("the absolute path %s in %q is not on the generated host: write it under one of %v",
			value, word, slices.Sorted(maps.Keys(d)))
	}
	if !d.holds(named) {
		return "", fmt.Errorf("%q names %s, which is not on the generated host: write a path under one of %v",
			word, named, slices.Sorted(maps.Keys(d)))
	}
	return flag + moved, nil
}

// documentedDirOf returns the documented directory value is in, or is,
// and the generated directory that stands for it.
func (d logsDocDirs) documentedDirOf(value string) (doc, real string, found bool) {
	for doc, real := range d {
		if value == doc || strings.HasPrefix(value, doc+"/") {
			return doc, real, true
		}
	}
	return "", "", false
}

// holds reports whether path, a clean absolute path, is one of the
// generated directories or lies inside one.
func (d logsDocDirs) holds(path string) bool {
	for _, real := range d {
		if path == real || strings.HasPrefix(path, real+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// fromHost writes the generated host's directories in text back as the
// directories the docs name.
func (d logsDocDirs) fromHost(text string) string {
	for doc, real := range d {
		text = strings.ReplaceAll(text, real, doc)
		if resolved, err := filepath.EvalSymlinks(real); err == nil {
			text = strings.ReplaceAll(text, resolved, doc)
		}
	}
	return text
}

// docLogLine is a line of a generated log: its time and its text after
// the timestamp.
type docLogLine struct {
	at   time.Time
	text string
}

// syslogMessages are the messages a generated syslog repeats, by line
// number. %[1]d is a number that varies (a process id, a session), %[2]d
// a request number and %[3]d an address byte.
var syslogMessages = []string{
	"web01 systemd[1]: Started session-%[1]d.scope - Session %[1]d of User deploy.",
	"web01 CRON[%[1]d]: (root) CMD (command -v debian-sa1 > /dev/null && debian-sa1 1 1)",
	"web01 kernel: [UFW BLOCK] IN=eth0 OUT= SRC=203.0.113.%[3]d DST=10.0.4.2 PROTO=TCP DPT=23",
	"web01 nginx[%[1]d]: upstream timed out (110: Connection timed out) while reading response header from upstream, request %[2]d",
	"web01 systemd-logind[612]: New session %[1]d of user deploy.",
	"web01 app[%[1]d]: request %[2]d failed: timeout after 30s",
	"web01 nginx[%[1]d]: connect() failed (111: Connection refused) while connecting to upstream, request %[2]d",
	"web01 systemd[1]: logrotate.service: Deactivated successfully.",
}

// docSyslogPart is one day of the generated syslog: n lines from start,
// a fixed step apart, and the out-of-memory lines at the local lines oom
// names, each with its process id.
func docSyslogPart(start time.Time, n int, step time.Duration, oom map[int]int) []docLogLine {
	lines := make([]docLogLine, n)
	for i := range lines {
		local := i + 1
		at := start.Add(time.Duration(i) * step)
		if pid, ok := oom[local]; ok {
			lines[i] = docLogLine{at, fmt.Sprintf("web01 kernel: Out of memory: Killed process %d (java) total-vm:8123456kB", pid)}
			continue
		}
		number := 1000 + (local*7919)%9000
		message := syslogMessages[(local*7)%len(syslogMessages)]
		if strings.Contains(message, "%") {
			message = fmt.Sprintf(message, number, number+local, 1+number%250)
		}
		lines[i] = docLogLine{at, message}
	}
	return lines
}

// docAuthPart is one day of the generated auth.log: n lines from start.
func docAuthPart(start time.Time, n int) []docLogLine {
	messages := []string{
		"web01 sshd[%d]: Accepted publickey for deploy from 10.0.4.17 port %d ssh2: ED25519 SHA256:3f9Qe0r1",
		"web01 sshd[%d]: pam_unix(sshd:session): session opened for user deploy(uid=1000) by (uid=0), port %d",
		"web01 sshd[%d]: Connection closed by 198.51.100.7 port %d [preauth]",
	}
	lines := make([]docLogLine, n)
	for i := range lines {
		pid := 20000 + i*13
		lines[i] = docLogLine{start.Add(time.Duration(i) * 7 * time.Minute),
			fmt.Sprintf(messages[i%len(messages)], pid, 40000+i)}
	}
	return lines
}

// syslogLines writes lines with the traditional syslog timestamp
// (`Oct  3 00:00:05`, no year), as rsyslog writes them by default. A
// reader takes the year from each file's modification time.
func syslogLines(lines []docLogLine) []byte {
	var buf bytes.Buffer
	for _, l := range lines {
		fmt.Fprintf(&buf, "%s %s\n", l.at.UTC().Format("Jan _2 15:04:05"), l.text)
	}
	return buf.Bytes()
}

// spaceLines writes lines with `yyyy-mm-dd HH:MM:SS` timestamps, as
// dpkg writes them.
func spaceLines(lines []docLogLine) []byte {
	var buf bytes.Buffer
	for _, l := range lines {
		fmt.Fprintf(&buf, "%s %s\n", l.at.UTC().Format("2006-01-02 15:04:05"), l.text)
	}
	return buf.Bytes()
}

// gzipped is body compressed with gzip.
func gzipped(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// docLogFile is one file of the generated host.
type docLogFile struct {
	name    string
	body    []byte
	modTime time.Time
}

// writeDocLogFiles writes files into dir, each with its modification
// time.
func writeDocLogFiles(t *testing.T, dir string, files []docLogFile) {
	t.Helper()
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, f.modTime, f.modTime); err != nil {
			t.Fatal(err)
		}
	}
}

// day is midnight UTC of a day in 2026.
func day(month time.Month, d int) time.Time {
	return time.Date(2026, month, d, 0, 0, 0, 0, time.UTC)
}

// logsDocHost generates the host the examples run on.
//
// /var/log holds the chains syslog (numbered, one day per part, three
// out-of-memory lines), auth.log (dated names), kern.log, dpkg.log (part
// 3 missing, the active file empty) and dmesg (no timestamps: invalid),
// the binary wtmp, wtmp.1 and lastlog, and notes.txt. /srv/app/logs
// holds a logback chain, app.log.
func logsDocHost(t *testing.T) logsDocDirs {
	t.Helper()
	varLog, appLogs := t.TempDir(), t.TempDir()
	const daySeconds = 86400
	syslogDays := []struct {
		name  string
		start time.Time
		lines int
		oom   map[int]int
	}{
		{"syslog.4.gz", day(time.September, 29), 3120, nil},
		{"syslog.3.gz", day(time.September, 30), 2988, map[int]int{1500: 30211}},
		{"syslog.2.gz", day(time.October, 1), 3305, nil},
		{"syslog.1", day(time.October, 2), 3012, map[int]int{2200: 41877}},
	}
	var files []docLogFile
	for _, d := range syslogDays {
		step := time.Duration(daySeconds/d.lines) * time.Second
		body := syslogLines(docSyslogPart(d.start.Add(5*time.Second), d.lines, step, d.oom))
		if strings.HasSuffix(d.name, ".gz") {
			body = gzipped(t, body)
		}
		files = append(files, docLogFile{d.name, body, d.start.Add(24 * time.Hour)})
	}
	active := syslogLines(docSyslogPart(day(time.October, 3).Add(5*time.Second), 1980, 30*time.Second, map[int]int{900: 52004}))
	files = append(files, docLogFile{"syslog", active, day(time.October, 3).Add(17 * time.Hour)})

	authDays := []struct {
		name  string
		start time.Time
		gz    bool
	}{
		{"auth.log-20261001.gz", day(time.September, 30), true},
		{"auth.log-20261002.gz", day(time.October, 1), true},
		{"auth.log-20261003", day(time.October, 2), false},
		{"auth.log", day(time.October, 3), false},
	}
	for _, d := range authDays {
		body := syslogLines(docAuthPart(d.start.Add(17*time.Second), 180))
		if d.gz {
			body = gzipped(t, body)
		}
		files = append(files, docLogFile{d.name, body, d.start.Add(21 * time.Hour)})
	}

	kern := func(start time.Time, n int) []byte {
		lines := make([]docLogLine, n)
		for i := range lines {
			lines[i] = docLogLine{start.Add(time.Duration(i) * 41 * time.Minute),
				fmt.Sprintf("web01 kernel: [UFW BLOCK] IN=eth0 OUT= SRC=198.51.100.%d DST=10.0.4.2 PROTO=TCP DPT=22", 10+i)}
		}
		return syslogLines(lines)
	}
	files = append(files,
		docLogFile{"kern.log.2.gz", gzipped(t, kern(day(time.September, 26).Add(3*time.Hour), 12)), day(time.September, 27)},
		docLogFile{"kern.log.1", kern(day(time.September, 30).Add(time.Hour), 31), day(time.October, 1)},
		docLogFile{"kern.log", kern(day(time.October, 3).Add(2*time.Hour), 7), day(time.October, 3).Add(7 * time.Hour)},
	)

	dpkg := func(start time.Time, n int) []byte {
		lines := make([]docLogLine, n)
		for i := range lines {
			lines[i] = docLogLine{start.Add(time.Duration(i) * time.Second),
				fmt.Sprintf("status installed libssl3:amd64 3.0.%d-1~deb12u1", 11+i%5)}
		}
		return spaceLines(lines)
	}
	files = append(files,
		docLogFile{"dpkg.log.4.gz", gzipped(t, dpkg(day(time.June, 2).Add(6*time.Hour), 48)), day(time.July, 1)},
		docLogFile{"dpkg.log.2.gz", gzipped(t, dpkg(day(time.August, 11).Add(7*time.Hour), 25)), day(time.September, 1)},
		docLogFile{"dpkg.log.1", dpkg(day(time.September, 22).Add(7*time.Hour), 24), day(time.October, 1)},
		docLogFile{"dpkg.log", nil, day(time.October, 1)},
		docLogFile{"dmesg.0", []byte("[    0.000000] Linux version 6.1.0-26-amd64\n[    0.000000] Command line: ro quiet\n"), day(time.October, 1)},
		docLogFile{"dmesg", []byte("[    0.000000] Linux version 6.1.0-26-amd64\n[    1.204518] EXT4-fs (sda1): mounted\n"), day(time.October, 3)},
		docLogFile{"wtmp", []byte("\x07\x00\x00\x00\x8c\x02\x00\x00pts/0\x00\x00\x00"), day(time.October, 3)},
		docLogFile{"wtmp.1", []byte("\x07\x00\x00\x00\x8c\x02\x00\x00pts/1\x00\x00\x00"), day(time.October, 1)},
		docLogFile{"lastlog", []byte("\x00\x00\x00\x00pts/0\x00\x00"), day(time.October, 3)},
		docLogFile{"notes.txt", []byte("2026-10-02 web01 maintenance: upstream timeout raised to 30s\n"), day(time.October, 2)},
	)
	writeDocLogFiles(t, varLog, files)

	app := func(start time.Time, n int) []byte {
		var buf bytes.Buffer
		for i := 0; i < n; i++ {
			at := start.Add(time.Duration(i) * 90 * time.Second)
			fmt.Fprintf(&buf, "%s INFO  [http-nio-8080-exec-%d] c.e.shop.OrderController - order %d placed\n",
				at.Format("2006-01-02 15:04:05.000"), 1+i%8, 7000+i)
		}
		return buf.Bytes()
	}
	writeDocLogFiles(t, appLogs, []docLogFile{
		{"app-2026-10-01.0.log.gz", gzipped(t, app(day(time.October, 1), 400)), day(time.October, 1).Add(10 * time.Hour)},
		{"app-2026-10-01.1.log.gz", gzipped(t, app(day(time.October, 1).Add(10*time.Hour), 400)), day(time.October, 1).Add(20 * time.Hour)},
		{"app-2026-10-02.0.log.gz", gzipped(t, app(day(time.October, 2), 600)), day(time.October, 2).Add(15 * time.Hour)},
		{"app.log", app(day(time.October, 3), 120), day(time.October, 3).Add(3 * time.Hour)},
	})
	return logsDocDirs{"/var/log": varLog, "/srv/app/logs": appLogs}
}

// The parts of a line normalize replaces: values that differ from run
// to run or from host to host, and spacing.
var outputNormalizations = []struct {
	pattern *regexp.Regexp
	with    string
}{
	{regexp.MustCompile(`\b[0-9a-f]{16}\b`), "FINGERPRINT"},
	{regexp.MustCompile(`^Request ID: .*$`), "Request ID: ID"},
	{regexp.MustCompile(`^Time: [0-9.]+s$`), "Time: SECONDS"},
	{regexp.MustCompile(`\bin [0-9]+\.[0-9]+s\b`), "in SECONDS"},
	{regexp.MustCompile(`\b[0-9]+(\.[0-9]+)? (B|KB|MB|GB)\b`), "SIZE"},
	{regexp.MustCompile(`\s+`), " "},
}

// normalize is line without what varies between runs: the words of the
// line one space apart, with fingerprints, request ids, times taken and
// sizes replaced by a placeholder.
func normalize(line string) string {
	line = strings.TrimSpace(line)
	for _, n := range outputNormalizations {
		line = n.pattern.ReplaceAllString(line, n.with)
	}
	return line
}

// docLineMatches reports whether the real line matches the documented
// one, both normalized; `…` in the documented line stands for any text.
func docLineMatches(doc, real string) bool {
	doc, real = normalize(doc), normalize(real)
	if !strings.Contains(doc, "…") {
		return doc == real
	}
	pieces := strings.Split(doc, "…")
	for i, piece := range pieces {
		pieces[i] = regexp.QuoteMeta(piece)
	}
	return regexp.MustCompile("^" + strings.Join(pieces, ".*") + "$").MatchString(real)
}

// outputMatches reports whether the real lines match the documented
// ones, where a documented line `...` stands for any number of lines.
func outputMatches(doc, real []string) bool {
	if len(doc) == 0 {
		return len(real) == 0
	}
	if strings.TrimSpace(doc[0]) == "..." {
		for skip := 0; skip <= len(real); skip++ {
			if outputMatches(doc[1:], real[skip:]) {
				return true
			}
		}
		return false
	}
	return len(real) > 0 && docLineMatches(doc[0], real[0]) && outputMatches(doc[1:], real[1:])
}

// jsonAnswers are the wire types the --json output of each rx logs
// subcommand decodes into: one value, or an array of them for several
// directories or chains.
var jsonAnswers = map[string]func() any{
	"list":    func() any { return &rxtypes.ChainsResponse{} },
	"show":    func() any { return &rxtypes.ChainResponse{} },
	"samples": func() any { return &rxtypes.ChainSamplesResponse{} },
	"trace":   func() any { return &rxtypes.ChainTraceResponse{} },
}

// jsonAnswerOf returns what the --json output of the rx logs command
// args decodes into: its wire type, or any JSON value for a subcommand
// without one.
func jsonAnswerOf(args []string) func() any {
	if at := slices.Index(args, "logs"); at >= 0 && at+1 < len(args) {
		if newValue, ok := jsonAnswers[args[at+1]]; ok {
			return newValue
		}
	}
	return func() any { return new(any) }
}

// decodeStrictly decodes text into the value newValue makes, or into an
// array of them when text is an array, refusing unknown fields.
func decodeStrictly(text string, newValue func() any) error {
	trimmed := strings.TrimSpace(text)
	values := []string{trimmed}
	if strings.HasPrefix(trimmed, "[") {
		var raw []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			return err
		}
		values = values[:0]
		for _, r := range raw {
			values = append(values, string(r))
		}
	}
	for _, v := range values {
		dec := json.NewDecoder(strings.NewReader(v))
		dec.DisallowUnknownFields()
		if err := dec.Decode(newValue()); err != nil {
			return err
		}
	}
	return nil
}

// documentedLogsCommand is one rx logs command of a page, with the
// output the page shows for it, if any.
type documentedLogsCommand struct {
	location string
	words    []string
	output   []string // nil when the page shows none
}

// documentedLogsFences returns the rx logs commands of the README and
// the pages under docs/, one slice per shell fence that holds any.
func documentedLogsFences(t *testing.T) [][]documentedLogsCommand {
	t.Helper()
	var all [][]documentedLogsCommand
	for _, page := range docPages(t) {
		fences := docFences(t, page)
		for i, fence := range fences {
			if !shellFenceLanguages[fence.language] {
				continue
			}
			var found []documentedLogsCommand
			lastIsLogs := false
			for k, line := range fence.lines {
				if strings.TrimSpace(line) == "" {
					continue
				}
				words := rxCommandWords(strings.TrimSpace(line))
				lastIsLogs = len(words) >= 2 && words[1] == "logs"
				if !lastIsLogs {
					continue
				}
				location := fmt.Sprintf("%s:%d", strings.TrimPrefix(page, "../../"), fence.line+1+k)
				found = append(found, documentedLogsCommand{location: location, words: words})
			}
			if len(found) == 0 {
				continue
			}
			if lastIsLogs && i+1 < len(fences) && fences[i+1].language == "text" && fences[i+1].adjacent {
				found[len(found)-1].output = fences[i+1].lines
			}
			all = append(all, found)
		}
	}
	return all
}

func TestLogsDocExamples_RunAndPrintWhatThePagesShow(t *testing.T) {
	dirs := logsDocHost(t)
	commands, shown := 0, 0
	for _, fence := range documentedLogsFences(t) {
		cache := []string{"RX_CACHE_DIR=" + t.TempDir()}
		for _, command := range fence {
			commands++
			if command.output != nil {
				shown++
			}
			runDocumentedLogsCommand(t, dirs, cache, command)
		}
	}
	// An extractor that finds nothing would pass everything.
	if commands < 10 || shown < 5 {
		t.Fatalf("found %d rx logs commands, %d with their output; the extractor is broken", commands, shown)
	}
}

// runDocumentedLogsCommand runs one documented command on the generated
// host with the given cache, and checks its exit code, its JSON and the
// output the page shows for it.
func runDocumentedLogsCommand(t *testing.T, dirs logsDocDirs, cache []string, command documentedLogsCommand) {
	t.Helper()
	args := make([]string, 0, len(command.words)-1)
	for _, word := range command.words[1:] {
		moved, err := dirs.toHost(word)
		if err != nil {
			t.Errorf("%s: %v", command.location, err)
			return
		}
		args = append(args, moved)
	}
	shownAs := strings.Join(command.words, " ")
	code, stdout, stderr := runRxIn(t, dirs[docRunDir], cache, args...)
	stdout = dirs.fromHost(stdout)
	if code != 0 {
		t.Errorf("%s: %s exited %d\n%s", command.location, shownAs, code, dirs.fromHost(stderr))
		return
	}
	if slices.Contains(args, "--json") {
		if err := decodeStrictly(stdout, jsonAnswerOf(args)); err != nil {
			t.Errorf("%s: %s: %v", command.location, shownAs, err)
		}
	}
	if command.output != nil && !outputMatches(command.output, strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")) {
		t.Errorf("%s: %s prints\n%s\nwhere the page shows\n%s", command.location, shownAs, stdout,
			strings.Join(command.output, "\n"))
	}
}

// The comparison has to tell a wrong output from a right one.
func TestLogsDocExamples_TheComparisonRejectsAnotherOutput(t *testing.T) {
	doc := []string{"/var/log: 2 chains", "NAME  PARTS  SIZE", "...", "syslog  5  1.20 MB", "Time: 0.041s", "Request ID: 0199c8a2-…"}
	same := []string{"/var/log: 2 chains", "NAME PARTS SIZE", "auth.log 4 3.10 KB", "syslog   5   80.00 KB", "Time: 1.5s", "Request ID: abc"}
	if !outputMatches(doc, same) {
		t.Fatal("an output with other spacing, sizes, times and request ids does not match")
	}
	for _, wrong := range [][]string{
		{"/var/log: 3 chains", "NAME PARTS SIZE", "syslog 5 1.20 MB", "Time: 0.041s", "Request ID: x"},
		{"/var/log: 2 chains", "NAME PARTS SIZE", "syslog 6 1.20 MB", "Time: 0.041s", "Request ID: x"},
		{"/var/log: 2 chains", "NAME PARTS SIZE", "syslog 5 1.20 MB", "Time: 0.041s"},
	} {
		if outputMatches(doc, wrong) {
			t.Errorf("%q matches the documented output", wrong)
		}
	}
	if !docLineMatches("Pattern: … error …", "Pattern: an error here") || docLineMatches("Pattern: error", "Pattern: errors") {
		t.Error("… does not stand for any text, or a line matches another")
	}
}

// A documented command runs on the generated host only: a path under
// a documented directory moves onto it, as a word or as a flag's value;
// any other absolute path is refused; and a word that would name a path
// outside the generated directories is refused too, with `..` in an
// absolute path, as a relative path from the directory rx runs in, as
// a flag's value, as the word after a flag and inside quotes, so no
// example can read or write the real filesystem.
func TestLogsDocExamples_RefusesAnAbsolutePathItCannotMove(t *testing.T) {
	dirs := logsDocDirs{"/var/log": "/host/var-log", "/srv/app/logs": "/host/app-logs"}
	for word, want := range map[string]string{
		"/var/log":                    "/host/var-log",
		"/var/log/syslog":             "/host/var-log/syslog",
		"/srv/app/logs/app.log":       "/host/app-logs/app.log",
		"--search-root=/var/log":      "--search-root=/host/var-log",
		"--search-root=/srv/app/logs": "--search-root=/host/app-logs",
		"--part=syslog.3.gz":          "--part=syslog.3.gz",
		"--timestamps=2026-10-03T14:00..2026-10-03T15:00": "--timestamps=2026-10-03T14:00..2026-10-03T15:00",
		"syslog":   "syslog",
		"./syslog": "./syslog",
		"error..x": "error..x",
		"-e":       "-e",
	} {
		if got, err := dirs.toHost(word); err != nil || got != want {
			t.Errorf("toHost(%q) = %q, %v; want %q", word, got, err, want)
		}
	}
	for _, word := range []string{
		"/etc/passwd", "/var/logs/x", "/srv/app", "--search-root=/x", "--file=/var/log2",
		"/var/log/../../../../../private/var/log", "/srv/app/logs/../../../etc", "--search-root=/var/log/..",
		"../../../../private/var/log", "..", "../x", "--part=../x", "syslog/../../x",
	} {
		if got, err := dirs.toHost(word); err == nil {
			t.Errorf("toHost(%q) = %q with no error; want it refused", word, got)
		}
	}
	// The words of a documented line reach toHost with their quotes
	// removed, as the shell passes them, and a flag's value may be the
	// next word.
	for _, line := range []string{
		`rx logs list '/var/log/../../../etc'`,
		`rx logs list "../../../../private/var/log"`,
		`rx logs samples /var/log/syslog --part ../x --lines=1`,
	} {
		refused := false
		for _, word := range rxCommandWords(line)[1:] {
			if _, err := dirs.toHost(word); err != nil {
				refused = true
			}
		}
		if !refused {
			t.Errorf("%s: no word refused; want the path that leaves the generated host refused", line)
		}
	}
}
