package clicommand

// The same-answer suite's cell for the head of a file: the first samples
// lookup in a file that wants a line index and has none, in a cache that
// can store one, is answered from the head of the file's text
// (samples.ResolveFromHead) when the head holds the answer. The suite's
// other cells ask with no index stored or with one built; this one asks
// the head, and requires the journal, its text and its `rx compress`
// copy to answer alike from it.

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// wantsAnIndex are the copies of a text a samples lookup wants a line
// index of: the journal and the seekable zstd copy, which are
// compressed. The plain text is smaller than the least large-file size
// (RX_LARGE_FILE_MB is 1 MB at least), so a lookup reads it without an
// index and never from the head: it is the reference the others are
// compared with.
var wantsAnIndex = map[string]bool{asJournal: true, asText: false, asCopy: true}

// headQueries are the samples queries of text that the head of a file
// can answer: those of samplesQueries without a line counted from the
// end, whose number needs the whole text counted.
func headQueries(t *testing.T, text []byte) []samplesQuery {
	t.Helper()
	queries := samplesQueries(t, text)
	for i := range queries {
		queries[i].lines = slices.DeleteFunc(slices.Clone(queries[i].lines), func(v string) bool {
			return strings.HasPrefix(v, "-")
		})
	}
	return queries
}

// TestJournal_TheHeadAnswersAsItsTextAndItsCompressedCopy asks, on each
// surface, every samples query the head can answer of the journal, its
// text and its copy, each in a fresh cache that can store an index, and
// requires the head to have answered the journal's and the copy's, and
// the three answers to be equal.
func TestJournal_TheHeadAnswersAsItsTextAndItsCompressedCopy(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	t.Setenv("RX_LOG_TZ", "")
	for _, journal := range sameAnswerJournals {
		t.Run(journal.name(), func(t *testing.T) {
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			files := writeSameAnswerFiles(t, journal)
			queries := headQueries(t, files.text)
			t.Run("CLI", func(t *testing.T) {
				answers := askHeadSamples(t, cliSurface{}, files, queries)
				requireSamplesAcrossStorages(t, "from the head", answers)
			})
			t.Run("HTTP", func(t *testing.T) {
				answers := askHeadSamples(t, newHTTPSurface(t, rg, files.dir), files, queries)
				requireSamplesAcrossStorages(t, "from the head", answers)
			})
		})
	}
}

// askHeadSamples asks every query of every copy, each in a subtest with a
// fresh cache that can store an index (the subtest restores RX_CACHE_DIR
// when it ends), so no answer can use an index another one built. The
// head must answer the journal's and the copy's queries
// (sameAnswerSurface.answeredFromTheHead); the text's is read without an
// index, which starts no build and stores none.
func askHeadSamples(t *testing.T, surface sameAnswerSurface, files sameAnswerFiles, queries []samplesQuery) requestAnswers {
	t.Helper()
	answers := requestAnswers{}
	for _, storage := range sameAnswerStorages {
		path := files.paths[storage]
		for _, q := range queries {
			asked := t.Run(storage+" by "+q.name, func(t *testing.T) {
				t.Setenv("RX_CACHE_DIR", t.TempDir())
				answer := surface.samples(t, path, q)
				switch {
				case wantsAnIndex[storage] && !surface.answeredFromTheHead(t, answer):
					t.Fatalf("the head of the %s did not answer samples by %s: %v", storage, q.name, answer)
				case !wantsAnIndex[storage] && answer["index_build"] != nil:
					t.Fatalf("samples by %s of the %s started an index build: %v", q.name, storage, answer["index_build"])
				case !wantsAnIndex[storage]:
					requireNoIndexStored(t, "after samples by "+q.name+" of the "+storage)
				}
				answers.add("samples by "+q.name, storage,
					fileNeutral(t, answer, path, "is_compressed", "compression_format", "index_build"))
			})
			if !asked {
				t.FailNow()
			}
		}
	}
	return answers
}

// answeredFromTheHead reports, for the CLI, whether a samples answer came
// from the head of the file: `rx samples` answered that way builds no
// index, so the cache holds none after it (a lookup the head cannot
// answer builds and stores the index first).
func (cliSurface) answeredFromTheHead(t *testing.T, _ map[string]any) bool {
	t.Helper()
	return !indexStored(t)
}

// answeredFromTheHead reports, for HTTP, whether a samples answer came
// from the head of the file: such an answer names the background build
// it started in index_build, which is null for an answer that waited for
// the build or needed none.
func (httpSurface) answeredFromTheHead(_ *testing.T, answer map[string]any) bool {
	return answer["index_build"] != nil
}
