package clicommand

// The same-answer suite: an undamaged systemd journal answers every
// request exactly as its text saved as a plain file answers it, and as
// the seekable zstd copy `rx compress` writes of it does — field by
// field, except the fields that name the file or how it is stored.
//
// For each journal J below, T is its set's text written as a plain file
// and Z is what `rx compress J` writes. Each surface (the CLI, run in
// process, and the HTTP router) asks the same requests of the three
// files with an empty cache, again with the findings cache warm, and
// again once each file has a line index. (The trace cache stores no
// scan of a file under 1 MiB, and never a journal's.) The requests:
//
//   - rx trace and GET /v1/trace: three patterns, each with
//     --max-results 1, 5 and none;
//   - rx trace --signatures=default and GET /v1/trace?signatures=default;
//   - rx samples and GET /v1/samples: --lines, --offsets, --timestamps;
//   - rx time-range and GET /v1/time-range;
//   - rx index and POST /v1/index: the line count and the time section.
//
// The rule that compares two answers is the accelerator rule
// (traceanswer, samplesanswer): a trace line number that is -1 ("not
// computed") in one answer may be known in the other, and a time range's
// first or last time that is null in one may be known in the other.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/internal/webapi"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// sameAnswerJournals are the journals the suite reads: two sets, each as
// an uncompressed regular journal and as a zstd-compressed compact one,
// and the 2,000-entry set as an lz4-compressed compact one.
var sameAnswerJournals = []string{
	"basic-none-regular", "basic-zstd-compact",
	"stamps-none-regular", "stamps-zstd-compact",
	"many-lz4-compact",
}

// sameAnswerPatterns are the trace patterns: one that matches nearly
// every line, one that matches a few lines of each set, and one with an
// alternation that matches words inside lines.
var sameAnswerPatterns = []string{`\[[0-9]+\]: `, `:0[2-4]\.[0-9]{6}`, `\b(of|the|in)\b`}

// sameAnswerCaps are the --max-results values each pattern is searched
// with; 0 searches without a cap.
var sameAnswerCaps = []int{1, 5, 0}

// The three copies of one text, in the order an answer is compared: the
// journal's first, and each other copy's against it.
const (
	asJournal = "journal"
	asText    = "plain text"
	asCopy    = "rx compress copy"
)

var sameAnswerStorages = []string{asJournal, asText, asCopy}

// sameAnswerFiles are the three copies of one text, in one directory.
type sameAnswerFiles struct {
	dir   string
	text  []byte
	paths map[string]string // by storage
}

// writeSameAnswerFiles writes the journal fixture name, its set's text
// as a plain file and `rx compress` of the journal into a new directory.
func writeSameAnswerFiles(t *testing.T, name string) sameAnswerFiles {
	t.Helper()
	set, _, _ := strings.Cut(name, "-")
	dir := t.TempDir()
	journal, err := os.ReadFile(journalfixture.Journal(t, name))
	if err != nil {
		t.Fatal(err)
	}
	text := journalfixture.SetText(t, set)
	files := sameAnswerFiles{dir: dir, text: text, paths: map[string]string{
		asJournal: filepath.Join(dir, "system.journal"),
		asText:    filepath.Join(dir, "system.log"),
		asCopy:    filepath.Join(dir, "system.journal.zst"),
	}}
	if err := os.WriteFile(files.paths[asJournal], journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.paths[asText], text, 0o600); err != nil {
		t.Fatal(err)
	}
	// `rx compress J` with its defaults, which name the output
	// system.journal.zst beside the journal.
	var out bytes.Buffer
	compress := NewCompressCommand(&out)
	compress.SetArgs([]string{files.paths[asJournal]})
	if err := compress.Execute(); err != nil {
		t.Fatalf("rx compress %s: %v\n%s", files.paths[asJournal], err, out.String())
	}
	if _, err := os.Stat(files.paths[asCopy]); err != nil {
		t.Fatalf("rx compress wrote no %s: %v", filepath.Base(files.paths[asCopy]), err)
	}
	return files
}

// sameAnswerSurface asks one surface (the CLI or the HTTP router) for
// the answers the suite compares, each as a parsed JSON document.
type sameAnswerSurface interface {
	trace(t *testing.T, path, pattern string, maxResults int) map[string]any
	findings(t *testing.T, path string) map[string]any
	samples(t *testing.T, path string, q samplesQuery) map[string]any
	timeRange(t *testing.T, path string) map[string]any
	// index builds the file's line index and returns it as the surface
	// shows it.
	index(t *testing.T, path string) map[string]any
}

// samplesQuery is one samples request: lines, offsets or timestamps.
type samplesQuery struct {
	name                       string
	lines, offsets, timestamps []string
}

// samplesQueries are the samples requests of a text: three lines (the
// first, three in the middle, the last), three byte offsets (the first
// byte, one in the middle, the last byte) and two times (a time and a
// range), taken from the text's own timestamps.
func samplesQueries(t *testing.T, text []byte) []samplesQuery {
	t.Helper()
	lines := bytes.Count(text, []byte("\n"))
	stamps := regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`).FindAll(text, -1)
	if lines < 3 || len(stamps) < 3 {
		t.Fatalf("the text has %d lines and %d timestamps; the suite needs three of each", lines, len(stamps))
	}
	first, middle := string(stamps[0]), string(stamps[len(stamps)/2])
	return []samplesQuery{
		{name: "lines", lines: []string{"1", fmt.Sprintf("%d-%d", lines/2, lines/2+2), "-1"}},
		{name: "offsets", offsets: []string{"0", strconv.Itoa(len(text) / 2), strconv.Itoa(len(text) - 1)}},
		{name: "timestamps", timestamps: []string{middle, first + ".." + middle}},
	}
}

// TestJournal_AnswersAsItsTextAndItsCompressedCopy is the suite. Each
// journal is compared on each surface in four steps that share one
// cache: cold, warm, indexed (see runSameAnswerSteps).
func TestJournal_AnswersAsItsTextAndItsCompressedCopy(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	// Only the built-in signatures, secrets shown, and no zone from the
	// developer's environment: the answers depend on nothing but the
	// files.
	t.Setenv("RX_SIGNATURES_PATH", t.TempDir())
	t.Setenv("RX_MASK_SECRETS", "")
	t.Setenv("RX_LOG_TZ", "")
	for _, name := range sameAnswerJournals {
		t.Run(name, func(t *testing.T) {
			// rx compress builds the copy's index into this cache, which
			// no surface below reads.
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			files := writeSameAnswerFiles(t, name)
			t.Run("CLI", func(t *testing.T) {
				t.Setenv("RX_CACHE_DIR", t.TempDir())
				runSameAnswerSteps(t, cliSurface{}, files)
			})
			t.Run("HTTP", func(t *testing.T) {
				t.Setenv("RX_CACHE_DIR", t.TempDir())
				runSameAnswerSteps(t, newHTTPSurface(t, rg, files.dir), files)
			})
		})
	}
}

// runSameAnswerSteps asks surface for every answer of the three copies
// of files' text, with a cache that starts empty:
//
//  1. cold: searches and findings, then samples and time ranges, with
//     no index and nothing cached;
//  2. warm: the searches and findings again, the findings from the
//     findings cache the cold step filled;
//  3. indexed: each file's line index is built, then every request is
//     asked again.
//
// At each step the answers of the three copies agree, and every answer
// agrees with the cold one of the same file. A cold time range of a
// file whose index a command builds on the way (`rx time-range` of a
// stream file) is the indexed answer; the rule holds either way.
func runSameAnswerSteps(t *testing.T, surface sameAnswerSurface, files sameAnswerFiles) {
	queries := samplesQueries(t, files.text)

	coldSearches := askSearches(t, surface, files)
	requireSearchesAcrossStorages(t, "cold", coldSearches, files.text)
	// A search builds no index, so the cold searches ran without one.
	if stored, _ := filepath.Glob(filepath.Join(config.GetIndexCacheDir(), "*")); len(stored) != 0 {
		t.Fatalf("an index was stored before the cold samples: %v", stored)
	}
	coldSamples := askSamples(t, surface, files, queries)
	requireSamplesAcrossStorages(t, "cold", coldSamples)
	coldRanges := askTimeRanges(t, surface, files)
	requireTimeRangesAcrossStorages(t, "cold", coldRanges, false)
	requireTheJournalAnswered(t, coldSearches, coldSamples, coldRanges[asJournal])

	// The findings cache now holds every copy's findings. The trace cache
	// holds nothing: it stores the scans of files of 1 MiB and more
	// (trace.ShouldCache), and a journal's search is never stored.
	requireCacheEntries(t, "the findings cache", config.GetFindingsCacheDir())
	warmSearches := askSearches(t, surface, files)
	requireSearchesAcrossStorages(t, "warm", warmSearches, files.text)
	requireSearchesAgree(t, "warm against cold", warmSearches, coldSearches, files.text)

	indexes := map[string]map[string]any{}
	for _, storage := range sameAnswerStorages {
		indexes[storage] = surface.index(t, files.paths[storage])
	}
	requireIndexesAcrossStorages(t, indexes)

	indexedSearches := askSearches(t, surface, files)
	requireSearchesAcrossStorages(t, "indexed", indexedSearches, files.text)
	requireSearchesAgree(t, "indexed against cold", indexedSearches, coldSearches, files.text)
	indexedSamples := askSamples(t, surface, files, queries)
	requireSamplesAcrossStorages(t, "indexed", indexedSamples)
	requireSamplesAgree(t, "indexed against cold", indexedSamples, coldSamples)
	indexedRanges := askTimeRanges(t, surface, files)
	requireTimeRangesAcrossStorages(t, "indexed", indexedRanges, true)
	for _, storage := range sameAnswerStorages {
		requireTimeRangesAgree(t, "indexed against cold, "+storage,
			indexedRanges[storage], coldRanges[storage], false)
	}
}

// requireTheJournalAnswered fails the test when the journal's cold
// answers hold nothing to compare: the suite proves nothing about a
// journal that is skipped, or whose text is empty. The first pattern
// matches nearly every line, every samples query names lines of the
// text, and the text has timestamps.
func requireTheJournalAnswered(t *testing.T, searches, samples requestAnswers, timeRange map[string]any) {
	t.Helper()
	uncapped := searches[fmt.Sprintf("trace of pattern 1, --max-results=%d", 0)][asJournal]
	if matches, _ := uncapped["matches"].([]any); len(matches) == 0 {
		t.Fatalf("the journal's search answered no match: %v", uncapped)
	}
	if skipped, _ := uncapped["skipped_files"].([]any); len(skipped) != 0 {
		t.Fatalf("the search skipped the journal: %v", uncapped["skip_reasons"])
	}
	for request, byStorage := range samples {
		if lines, _ := byStorage[asJournal]["samples"].(map[string]any); len(lines) == 0 {
			t.Fatalf("%s of the journal gave no sample: %v", request, byStorage[asJournal])
		}
	}
	if timeRange["format"] == nil {
		t.Fatalf("the journal's time range found no timestamp format: %v", timeRange)
	}
}

// requireCacheEntries fails the test unless dir holds a file, at any
// depth: the warm step then reads a cache that the cold step filled.
func requireCacheEntries(t *testing.T, what, dir string) {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("%s holds nothing after the cold step (%s: %v)", what, dir, err)
	}
}

// requestAnswers are the answers of one step: by request, then by
// storage, each without the fields that name the file or its format
// (fileNeutral).
type requestAnswers map[string]map[string]map[string]any

// add records answer as storage's answer to request.
func (a requestAnswers) add(request, storage string, answer map[string]any) {
	if a[request] == nil {
		a[request] = map[string]map[string]any{}
	}
	a[request][storage] = answer
}

// askSearches asks every trace and the findings of every copy.
func askSearches(t *testing.T, surface sameAnswerSurface, files sameAnswerFiles) requestAnswers {
	t.Helper()
	answers := requestAnswers{}
	for _, storage := range sameAnswerStorages {
		path := files.paths[storage]
		for i, pattern := range sameAnswerPatterns {
			for _, maxResults := range sameAnswerCaps {
				request := fmt.Sprintf("trace of pattern %d, --max-results=%d", i+1, maxResults)
				answers.add(request, storage, fileNeutral(t, surface.trace(t, path, pattern, maxResults), path))
			}
		}
		answers.add("findings", storage, fileNeutral(t, surface.findings(t, path), path))
	}
	return answers
}

// askSamples asks every samples query of every copy.
func askSamples(t *testing.T, surface sameAnswerSurface, files sameAnswerFiles, queries []samplesQuery) requestAnswers {
	t.Helper()
	answers := requestAnswers{}
	for _, storage := range sameAnswerStorages {
		path := files.paths[storage]
		for _, q := range queries {
			answers.add("samples by "+q.name, storage,
				fileNeutral(t, surface.samples(t, path, q), path, "is_compressed", "compression_format"))
		}
	}
	return answers
}

// askTimeRanges asks the time range of every copy. source says how the
// range was found (the index, a scan of the head and tail, or neither),
// which depends on how the file is stored, not on its text.
func askTimeRanges(t *testing.T, surface sameAnswerSurface, files sameAnswerFiles) map[string]map[string]any {
	t.Helper()
	ranges := map[string]map[string]any{}
	for _, storage := range sameAnswerStorages {
		path := files.paths[storage]
		ranges[storage] = fileNeutral(t, surface.timeRange(t, path), path, "source", "cli_command")
	}
	return ranges
}

// requireSearchesAcrossStorages requires the text's and the copy's
// answers to agree with the journal's, request by request.
func requireSearchesAcrossStorages(t *testing.T, step string, answers requestAnswers, text []byte) {
	t.Helper()
	for request, byStorage := range answers {
		for _, storage := range sameAnswerStorages[1:] {
			traceanswer.RequireAgree(t, fmt.Sprintf("%s: %s of the %s against the journal", step, request, storage),
				byStorage[storage], byStorage[asJournal], text)
		}
	}
}

// requireSearchesAgree requires each answer of got to agree with the
// same request's answer of want, for the same copy.
func requireSearchesAgree(t *testing.T, step string, got, want requestAnswers, text []byte) {
	t.Helper()
	for request, byStorage := range got {
		for _, storage := range sameAnswerStorages {
			traceanswer.RequireAgree(t, fmt.Sprintf("%s: %s of the %s", step, request, storage),
				byStorage[storage], want[request][storage], text)
		}
	}
}

// requireSamplesAcrossStorages is requireSearchesAcrossStorages for
// samples answers, which are equal (samplesanswer).
func requireSamplesAcrossStorages(t *testing.T, step string, answers requestAnswers) {
	t.Helper()
	for request, byStorage := range answers {
		for _, storage := range sameAnswerStorages[1:] {
			samplesanswer.RequireAgree(t, fmt.Sprintf("%s: %s of the %s against the journal", step, request, storage),
				byStorage[storage], byStorage[asJournal])
		}
	}
}

// requireSamplesAgree is requireSearchesAgree for samples answers.
func requireSamplesAgree(t *testing.T, step string, got, want requestAnswers) {
	t.Helper()
	for request, byStorage := range got {
		for _, storage := range sameAnswerStorages {
			samplesanswer.RequireAgree(t, fmt.Sprintf("%s: %s of the %s", step, request, storage),
				byStorage[storage], want[request][storage])
		}
	}
}

// requireTimeRangesAcrossStorages requires the text's and the copy's
// time ranges to agree with the journal's. strict makes a time null in
// one answer and known in the other a difference too.
func requireTimeRangesAcrossStorages(t *testing.T, step string, ranges map[string]map[string]any, strict bool) {
	t.Helper()
	for _, storage := range sameAnswerStorages[1:] {
		requireTimeRangesAgree(t, fmt.Sprintf("%s: time range of the %s against the journal", step, storage),
			ranges[storage], ranges[asJournal], strict)
	}
}

// notComputedTimes are the time range's fields that may be null in one
// answer and known in the other: a stream-compressed file without an
// index has neither its first nor its last time, and null says "not
// computed".
var notComputedTimes = map[string]bool{"first_ms": true, "last_ms": true}

// requireTimeRangesAgree requires every field of got and want to be
// equal, except a notComputedTimes field that is null in one of them
// (unless strict).
func requireTimeRangesAgree(t *testing.T, name string, got, want map[string]any, strict bool) {
	t.Helper()
	for key := range union(got, want) {
		g, w := got[key], want[key]
		if !strict && notComputedTimes[key] && (g == nil || w == nil) {
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s: %s is %v, want %v", name, key, g, w)
		}
	}
}

// requireIndexesAcrossStorages requires the line count and the time
// section of every copy's index to equal the journal's. The text's index
// is built at the same step as the journal's, so its checkpoints and the
// latest time before each are equal too; the copy's checkpoints are its
// frames, and are not compared.
func requireIndexesAcrossStorages(t *testing.T, indexes map[string]map[string]any) {
	t.Helper()
	journal := indexes[asJournal]
	if journal["line_count"] == nil || timeSection(journal) == nil {
		t.Fatalf("the journal's index has no line count or no time section: %v", journal)
	}
	if format, _ := journal["compression_format"].(string); format != "journal" {
		t.Errorf("the journal's index says compression_format %q, want journal", format)
	}
	for _, storage := range sameAnswerStorages[1:] {
		index := indexes[storage]
		if !reflect.DeepEqual(index["line_count"], journal["line_count"]) {
			t.Errorf("index of the %s: line_count %v, the journal's %v", storage, index["line_count"], journal["line_count"])
		}
		if got, want := withoutKey(timeSection(index), "max_before"), withoutKey(timeSection(journal), "max_before"); !reflect.DeepEqual(got, want) {
			t.Errorf("index of the %s: time section\n%v\nthe journal's\n%v", storage, got, want)
		}
	}
	text := indexes[asText]
	for _, key := range []string{"line_index", "empty_line_count", "line_ending"} {
		if !reflect.DeepEqual(text[key], journal[key]) {
			t.Errorf("index of the plain text: %s %v, the journal's %v", key, text[key], journal[key])
		}
	}
	if got, want := timeSection(text)["max_before"], timeSection(journal)["max_before"]; !reflect.DeepEqual(got, want) {
		t.Errorf("index of the plain text: max_before %v, the journal's %v", got, want)
	}
}

// timeSection is the time section of an index as a surface shows it:
// time_index in `rx index --json`, time_summary in GET /v1/index.
func timeSection(index map[string]any) map[string]any {
	if section, ok := index["time_index"].(map[string]any); ok {
		return section
	}
	section, _ := index["time_summary"].(map[string]any)
	return section
}

// fileNeutral returns answer as a parsed JSON document in which every
// string equal to path reads "<file>", without the top-level keys drop:
// what is left names neither the file nor how it is stored.
func fileNeutral(t *testing.T, answer any, path string, drop ...string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode an answer: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode an answer: %v", err)
	}
	for _, key := range drop {
		delete(doc, key)
	}
	neutral, _ := replaceString(doc, path, "<file>").(map[string]any)
	return neutral
}

// replaceString returns v with every string equal to old, at any depth,
// replaced by replacement.
func replaceString(v any, old, replacement string) any {
	switch value := v.(type) {
	case string:
		if value == old {
			return replacement
		}
		return value
	case []any:
		for i := range value {
			value[i] = replaceString(value[i], old, replacement)
		}
		return value
	case map[string]any:
		for key := range value {
			value[key] = replaceString(value[key], old, replacement)
		}
		return value
	}
	return v
}

// withoutKey returns a copy of m without key.
func withoutKey(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// union returns the keys of a and b.
func union(a, b map[string]any) map[string]bool {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	return keys
}

// decodeAnswer parses a JSON answer; an answer that is not a JSON
// object fails the test.
func decodeAnswer(t *testing.T, what string, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: decode %q: %v", what, raw, err)
	}
	return doc
}

// ============================================================================
// The CLI, run in process
// ============================================================================

// cliSurface asks the rx commands, run in this process.
type cliSurface struct{}

func (cliSurface) trace(t *testing.T, path, pattern string, maxResults int) map[string]any {
	t.Helper()
	args := []string{"--regexp=" + pattern, path, "--json"}
	if maxResults > 0 {
		args = append(args, "--max-results="+strconv.Itoa(maxResults))
	}
	out, err := runTraceCmd(t, args...)
	if err != nil {
		t.Fatalf("rx trace %v: %v", args, err)
	}
	return decodeAnswer(t, "rx trace", []byte(out))
}

func (cliSurface) findings(t *testing.T, path string) map[string]any {
	t.Helper()
	out, err := runTraceCmd(t, "--signatures=default", path, "--json")
	if err != nil {
		t.Fatalf("rx trace --signatures=default %s: %v", path, err)
	}
	return decodeAnswer(t, "rx trace --signatures", []byte(out))
}

func (cliSurface) samples(t *testing.T, path string, q samplesQuery) map[string]any {
	t.Helper()
	var out bytes.Buffer
	err := runSamples(&out, samplesParams{
		path: path, lines: q.lines, offsets: q.offsets, timestamps: q.timestamps, ctxLines: 1, jsonOutput: true,
	})
	if err != nil {
		t.Fatalf("rx samples %s by %s: %v", path, q.name, err)
	}
	return decodeAnswer(t, "rx samples", out.Bytes())
}

func (cliSurface) timeRange(t *testing.T, path string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := runTimeRange(&out, []string{path}, true, config.Zone{}); err != nil {
		t.Fatalf("rx time-range %s: %v", path, err)
	}
	return decodeAnswer(t, "rx time-range", out.Bytes())
}

func (cliSurface) index(t *testing.T, path string) map[string]any {
	t.Helper()
	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{path}, force: true, threshold: &zero})
	indexed, _ := result["indexed"].([]any)
	if len(indexed) != 1 {
		t.Fatalf("rx index %s indexed %d files: %v", path, len(indexed), result)
	}
	entry, _ := indexed[0].(map[string]any)
	return entry
}

// ============================================================================
// The HTTP router
// ============================================================================

// httpSurface asks a server that serves the router of `rx serve`, with
// the files' directory as its search root.
type httpSurface struct{ url string }

// newHTTPSurface starts the server; it stops when the test ends.
func newHTTPSurface(t *testing.T, rg, root string) httpSurface {
	t.Helper()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set the search root: %v", err)
	}
	t.Cleanup(paths.Reset)
	// The catalogue as `rx serve` loads it, from the same search path.
	cat := signatures.Load(signatures.LoadOptions{CheckExamples: sigscan.CheckExamples})
	opts := webapi.FindingsOptionsFromEnv()
	server := httptest.NewServer(webapi.NewServer(webapi.Config{RipgrepPath: rg, Signatures: cat, Findings: &opts}))
	t.Cleanup(server.Close)
	return httpSurface{url: server.URL}
}

// get asks GET route?query and returns the body of a 200 answer.
func (s httpSurface) get(t *testing.T, route string, query url.Values) []byte {
	t.Helper()
	resp, err := http.Get(s.url + route + "?" + query.Encode())
	if err != nil {
		t.Fatalf("GET %s: %v", route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read: %v", route, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?%s: %d %s", route, query.Encode(), resp.StatusCode, body)
	}
	return body
}

func (s httpSurface) trace(t *testing.T, path, pattern string, maxResults int) map[string]any {
	t.Helper()
	query := url.Values{"path": {path}, "regexp": {pattern}}
	if maxResults > 0 {
		query.Set("max_results", strconv.Itoa(maxResults))
	}
	return decodeAnswer(t, "GET /v1/trace", s.get(t, "/v1/trace", query))
}

func (s httpSurface) findings(t *testing.T, path string) map[string]any {
	t.Helper()
	query := url.Values{"path": {path}, "signatures": {"default"}}
	return decodeAnswer(t, "GET /v1/trace?signatures", s.get(t, "/v1/trace", query))
}

// samples asks GET /v1/samples. An answer from the head of a file that
// wants an index has started its build in the background; the build is
// awaited, so that every later request of the step meets the file as it
// will stay.
func (s httpSurface) samples(t *testing.T, path string, q samplesQuery) map[string]any {
	t.Helper()
	query := url.Values{"path": {path}, "context": {"1"}}
	if q.lines != nil {
		query.Set("lines", strings.Join(q.lines, ","))
	}
	if q.offsets != nil {
		query.Set("offsets", strings.Join(q.offsets, ","))
	}
	for _, ts := range q.timestamps {
		query.Add("timestamps", ts)
	}
	answer := decodeAnswer(t, "GET /v1/samples", s.get(t, "/v1/samples", query))
	if build, ok := answer["index_build"].(map[string]any); ok {
		taskID, _ := build["task_id"].(string)
		s.awaitTask(t, taskID)
	}
	return answer
}

func (s httpSurface) timeRange(t *testing.T, path string) map[string]any {
	t.Helper()
	return decodeAnswer(t, "GET /v1/time-range", s.get(t, "/v1/time-range", url.Values{"path": {path}}))
}

// index builds the index with POST /v1/index, awaits the task and reads
// the index back with GET /v1/index.
func (s httpSurface) index(t *testing.T, path string) map[string]any {
	t.Helper()
	zero := 0
	payload, err := json.Marshal(rxtypes.IndexRequest{Path: path, Force: true, Threshold: &zero})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.url+"/v1/index", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /v1/index: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/index %s: %d %s", path, resp.StatusCode, body)
	}
	var task rxtypes.TaskResponse
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("POST /v1/index: decode %q: %v", body, err)
	}
	if status := s.awaitTask(t, task.TaskID); status != "completed" {
		t.Fatalf("the index task of %s ended %s", path, status)
	}
	return decodeAnswer(t, "GET /v1/index", s.get(t, "/v1/index", url.Values{"path": {path}}))
}

// awaitTask polls GET /v1/tasks/{id} until the task ends and returns how
// it ended.
func (s httpSurface) awaitTask(t *testing.T, taskID string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		task := decodeAnswer(t, "GET /v1/tasks", s.get(t, "/v1/tasks/"+url.PathEscape(taskID), url.Values{}))
		switch status, _ := task["status"].(string); status {
		case "completed", "failed", "cancelled":
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not end in 30 s", taskID)
	return ""
}
