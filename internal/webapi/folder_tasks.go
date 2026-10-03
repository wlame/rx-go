package webapi

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wlame/rx-go/internal/cachefill"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A task over a directory is what POST /v1/index and POST /v1/findings
// start when their path is a directory: one task that takes every file
// the file tree lists under it, recursively, one at a time.
//
// It goes through the steps `rx index --recursive DIR` and `rx scan
// --recursive DIR` take (internal/cachefill): the same walk lists the
// same files in the same order, and each file is decided by the same
// step, so the task's cli_command does what the task did. The task is
// cancellable (tasks.Manager.CreateCancellable, under a key of its
// operation and the directory): DELETE /v1/tasks/{task_id} stops it
// between two files, and a findings task inside a file's scan too. The
// files ended before the cancel keep what was written for them; the file
// a cancel stops writes nothing. A folder task never settles
// (tasks.Manager.Settle): it stays cancellable until it ends.

// folderTally folds the results of a task over a directory into the
// result it completes with: the counts of every entry, and the entries
// skipped (other than current ones) and failed, each list cut at
// rxtypes.MaxFolderListedFiles so the result's size is bounded however
// many files the directory holds.
type folderTally struct {
	path, cli string
	counts    cachefill.Counts
	// findings sums the findings of every scanned or current file of a
	// findings task; nil for an index task.
	findings *int64
	skipped  []rxtypes.SkippedFile
	failed   []rxtypes.FailedFile
}

// newFolderTally starts the result of a task over dir whose command is
// cli; countFindings sums the findings of a findings task.
func newFolderTally(dir, cli string, countFindings bool) *folderTally {
	tally := &folderTally{path: dir, cli: cli, skipped: []rxtypes.SkippedFile{}, failed: []rxtypes.FailedFile{}}
	if countFindings {
		tally.findings = new(int64)
	}
	return tally
}

// add records one entry's result. Each calls it from the task's worker
// goroutine only, so it takes no lock.
func (t *folderTally) add(r cachefill.FileResult) {
	t.counts.Add(r)
	switch r.Outcome {
	case cachefill.Skipped:
		if len(t.skipped) < rxtypes.MaxFolderListedFiles {
			t.skipped = append(t.skipped, rxtypes.SkippedFile{Path: r.Path, Reason: r.Reason})
		}
	case cachefill.Failed:
		if len(t.failed) < rxtypes.MaxFolderListedFiles {
			t.failed = append(t.failed, rxtypes.FailedFile{Path: r.Path, Error: r.Reason})
		}
	}
	if t.findings != nil && r.Answer != nil {
		for _, id := range r.Answer.Checked {
			*t.findings += r.Answer.BySignature[id].Count
		}
	}
}

// result is the task's result.
func (t *folderTally) result() rxtypes.FolderTaskResult {
	return rxtypes.FolderTaskResult{
		Path: t.path, FilesTotal: t.counts.FilesTotal, Done: t.counts.Done, Skipped: t.counts.Skipped,
		Current: t.counts.Current, Failed: t.counts.Failed, Findings: t.findings,
		SkippedFiles: t.skipped, FailedFiles: t.failed, CLICommand: t.cli,
	}
}

// listFolder lists the task's directory (cachefill.List, recursively)
// and gives the task the progress its status reads, files_done of
// files_total from the start. A directory that cannot be listed fails
// the task, with the fixed wording of the failure (paths.FailureReason),
// never the error's text, which can name what a link leads to.
func listFolder(mgr *tasks.Manager, taskID string, dir paths.Pinned) ([]cachefill.Entry, *cachefill.Progress, bool) {
	entries, err := cachefill.List(dir, true)
	if err != nil {
		mgr.Fail(taskID, "cannot list the directory: "+paths.FailureReason(err))
		return nil, nil, false
	}
	progress := cachefill.NewProgress(len(entries))
	mgr.ReportFiles(taskID, progress.Files)
	mgr.ReportProgress(taskID, progress.Fraction)
	return entries, progress, true
}

// folderIndex is one index task over a directory.
type folderIndex struct {
	taskID   string
	dir      paths.Pinned
	settings cachefill.IndexSettings
	cli      string
}

// startIndexFolder starts the index task of POST /v1/index for the
// directory dir, pinned by the caller, or answers 409 with the task of
// the directory already running. The task runs detached from the
// request (runDetached), under the context its task keeps.
func (s *Server) startIndexFolder(req rxtypes.IndexRequest, dir paths.Pinned) (*postIndexOutput, error) {
	mgr := s.cfg.TaskManager
	task, ctx, isNew := mgr.CreateCancellable(dir.Path(), indexOperation)
	if !isNew {
		return nil, runningTaskConflict(req.Path, task)
	}
	cli := BuildCLICommand("index_post", map[string]any{
		"path": dir.Path(), "recursive": true, "force": req.Force, "threshold": req.Threshold,
	})
	mgr.SetCLICommand(task.TaskID, cli)
	// Snapshot the new task before its worker starts: from then on the
	// worker updates it, and reading it here would be a data race.
	taskID, status, started := task.TaskID, string(task.Status), formatTaskTime(task.StartedAt)
	work := folderIndex{taskID: taskID, dir: dir, cli: cli, settings: cachefill.IndexSettings{
		ThresholdBytes: thresholdBytesOf(req.Threshold),
		Force:          req.Force,
		// Asked once per task: a cache that cannot store fails every
		// file with the same cause without probing again.
		CheckStorable: sync.OnceValue(index.CheckStorable),
	}}
	// Go note: the goroutine outlives this request; runDetached turns a
	// panic in it into a failed task instead of a crashed server.
	go runDetached(mgr, taskID, indexOperation, s.cfg.Logger, func() {
		s.runIndexFolder(ctx, work)
	})
	return &postIndexOutput{Body: rxtypes.TaskResponse{
		TaskID: taskID, Status: status, Path: dir.Path(), StartedAt: &started, CLICommand: cli,
		Message: fmt.Sprintf("Indexing every file under %s; follow GET /v1/tasks/%s", dir.Path(), taskID),
	}}, nil
}

// thresholdBytesOf is the index threshold of a request in bytes: the
// request's threshold in MB, else RX_LARGE_FILE_MB.
func thresholdBytesOf(thresholdMB *int) int64 {
	if thresholdMB != nil {
		return int64(*thresholdMB) * 1024 * 1024
	}
	return int64(config.LargeFileMB()) * 1024 * 1024
}

// runIndexFolder is the worker of an index task over a directory: it
// lists the directory, builds the index of each file in turn as
// `rx index --recursive` does (Config.indexFolderFile, which is
// cachefill.IndexFile), and completes with the counts. Each file's build
// reports how far it has read, so progress moves inside a file too. A
// cancel stops the task between two files: a build that has started
// runs to its end, and its index is not stored.
func (s *Server) runIndexFolder(ctx context.Context, work folderIndex) {
	mgr := s.cfg.TaskManager
	mgr.MarkRunning(work.taskID)
	entries, progress, ok := listFolder(mgr, work.taskID, work.dir)
	if !ok {
		return
	}
	tally := newFolderTally(work.dir.Path(), work.cli, false)
	err := cachefill.Each(ctx, entries, progress, func(ctx context.Context, e cachefill.Entry) cachefill.FileResult {
		build := &index.Progress{}
		progress.TrackBuild(build)
		return s.cfg.indexFolderFile(ctx, e, work.settings, build)
	}, tally.add)
	if err != nil {
		// Each ends early only when ctx is canceled: Cancel ended the
		// task already.
		return
	}
	// A cancel that came after the last file leaves the task cancelled:
	// Complete does not change a cancelled task.
	mgr.Complete(work.taskID, tally.result())
}

// folderScan is one findings task over a directory.
type folderScan struct {
	taskID string
	dir    paths.Pinned
	sel    []*signatures.Signature
	force  bool
	cli    string
}

// runFindingsFolder is the worker of a findings task over a directory:
// it lists the directory, scans each file in turn as `rx scan
// --recursive` does (cachefill.ScanFile through Config.scanFindings,
// each scan waiting for a place in the scan queue, so
// RX_MAX_SIGNATURE_SCANS still bounds the files read at once), and
// completes with the counts. bytes_read and bytes_total follow the file
// being read. A cancel stops the scan inside the file it reads, which
// then writes nothing, and no later file is scanned.
func (s *Server) runFindingsFolder(ctx context.Context, work folderScan) {
	mgr := s.cfg.TaskManager
	start := time.Now()
	mgr.MarkRunning(work.taskID)
	entries, progress, ok := listFolder(mgr, work.taskID, work.dir)
	if !ok {
		prometheus.RecordSignatureScan("failed", false, 0)
		return
	}
	mgr.ReportBytes(work.taskID, progress.Bytes)
	opts := s.cfg.Findings.Scan
	opts.Force = work.force
	// The scan calls this from its own goroutines as it reads; the
	// progress keeps it in atomics.
	opts.Progress = progress.FileBytes
	settings := cachefill.ScanSettings{Catalogue: s.cfg.Signatures, Selection: work.sel, Options: opts, Scan: s.cfg.scanFindings}
	tally := newFolderTally(work.dir.Path(), work.cli, true)
	err := cachefill.Each(ctx, entries, progress, func(ctx context.Context, e cachefill.Entry) cachefill.FileResult {
		return cachefill.ScanFile(ctx, e, settings)
	}, tally.add)
	if err != nil {
		prometheus.RecordSignatureScan("cancelled", false, 0)
		return
	}
	mgr.Complete(work.taskID, tally.result())
	prometheus.RecordSignatureScan("completed", tally.counts.Done > 0, time.Since(start))
}
