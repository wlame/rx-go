package webapi

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainIndexOperation is the task operation of a log chain's index
// task, as GET /v1/tasks/{id} shows it.
const chainIndexOperation = "chain_index"

// chainTaskKey is the key a chain's index task holds in the task
// manager's path locks when its directory's identity is not known
// (chainKeyOf). The handle alone would collide with a task on the
// chain's active file, whose path equals the handle.
func chainTaskKey(handle string) string { return "chain:" + handle }

// chainIndexTasks runs the index task of each log chain: one background
// task per chain that builds the line indexes of the chain's parts
// through the per-file builds of samplesIndexBuilds, the builds a
// samples lookup starts and waits for.
//
// # One task per chain
//
// The task holds the chain's key (chainKeyOf: its directory's device
// and inode, and its name) in the task manager, so a second start for
// the same chain, from a describe or from POST /v1/logs/index, through
// the same handle or another path to the same directory, finds the
// running task and joins it (CreateKeyed hands it back). The task shows
// the handle it was started with as its path. A joined task builds what its first start asked for:
// a POST with force=true that joins a running task does not force.
//
// # The parts, a few at a time
//
// A chain may have 10,000 parts. The task never hands them all to the
// build queue: it keeps at most maxRunning part builds of its own in
// flight (RX_MAX_INDEX_BUILDS, the number of builds that run at once)
// and submits the next part only when one of them ends. So a chain adds
// at most that many entries to the queue, whatever its size. All chains
// together add at most half the queue (samplesIndexBuilds.maxChainParts):
// a chain that finds that half taken waits in line for room, one chain
// woken per build's end, so a samples lookup in another file still
// finds room in the queue however many chains are pending.
//
// # What is remembered
//
// records maps each chain to the last index task started for it, the
// fingerprint of the chain's files then, and, once the task has ended,
// how it ended. A description shows that task in index_build while it
// runs and after it ends. The record keeps the end itself, so the end
// outlives the task's place in the task table, which a burst of other
// tasks can take; it is dropped RX_TASK_TTL_MINUTES after the end (as
// the sweeper drops a finished task), or once the chain's files have
// another fingerprint, since the end no longer describes them. A task
// whose goroutine panicked records no end; each pruning takes its end
// from the task table, or drops its record once the table has dropped
// the task (settleLocked).
//
// A pending chain whose last task failed for the same files does not
// start another on describe: the same files would fail the same way, on
// every poll of a viewer. POST /v1/logs/index starts one whatever the
// last ended as.
//
// # Locks
//
// Lock order: mu, then the task manager's lock (CreateKeyed, Get, and
// the Get of each record without an end when the records are pruned).
// The task goroutine takes mu once, alone, to record its end
// (recordEnd), and never while it holds the task manager's lock or
// chainIndexRun.mu.
type chainIndexTasks struct {
	tasks  *tasks.Manager
	builds *samplesIndexBuilds
	logger *slog.Logger

	mu sync.Mutex
	// records maps a chain's key (chainTaskKey) to the last index task
	// started for it.
	records map[string]chainTaskRecord
	// now is the clock the records' ends are stamped and expired by:
	// time.Now, which a test replaces to reach past the task TTL.
	now func() time.Time
}

// maxEndedChainRecords is how many records of ended chain index tasks
// are kept at most. Past it, the record that ended first is dropped:
// a chain whose failed record goes is then started again by its next
// describe, which costs one more failing build after this many other
// chains' tasks have ended. Records of running tasks are never dropped
// (one per chain whose task runs).
const maxEndedChainRecords = 1024

// chainTaskRecord is the last index task started for one chain.
type chainTaskRecord struct {
	taskID      string
	fingerprint string
	// startedAt is when the task was created, for index_build.
	startedAt time.Time
	// end is how the task ended; nil while it runs.
	end *chainTaskEnd
}

// chainTaskEnd is how a chain's index task ended, kept in its record.
type chainTaskEnd struct {
	status tasks.Status
	// err is the task's error: the failing part's name and its build's
	// error. "" for a task that completed.
	err string
	// at is when the end was recorded, for the TTL.
	at time.Time
}

// task is the record's task as index_build shows it once it has ended:
// what the record keeps, whether or not the task table still holds it.
// The caller checks that the record has an end.
func (r chainTaskRecord) task() tasks.Task {
	return tasks.Task{
		TaskID: r.taskID, Operation: chainIndexOperation,
		Status: r.end.status, Error: r.end.err, StartedAt: r.startedAt,
	}
}

// newChainIndexTasks returns a registry whose tasks build the parts'
// indexes through builds.
func newChainIndexTasks(manager *tasks.Manager, logger *slog.Logger, builds *samplesIndexBuilds) *chainIndexTasks {
	if logger == nil {
		logger = slog.Default()
	}
	return &chainIndexTasks{
		tasks: manager, builds: builds, logger: logger,
		records: map[string]chainTaskRecord{}, now: time.Now,
	}
}

// chainTaskStart is what starting a chain's index task needs: the
// chain's handle (the path the task shows) and key (chainKeyOf), its
// fingerprint, the parts to build in the chain's order, and whether
// they are rebuilt even when current (for the task's cli_command).
type chainTaskStart struct {
	handle      string
	key         string
	fingerprint string
	parts       []logchain.Part
	force       bool
}

// chainKeyOf is the key of the chain d describes: the key its index
// task holds in the task manager and its record's key here. It is made
// of the device and inode of the chain's directory, as the listing
// pinned it, and the chain's name as listed, so every path that leads
// to the same directory gives the same key: another case on a
// case-insensitive disk, or a symbolic link to the directory. Two such
// handles then share one task instead of building the same parts twice
// at once.
//
// A description without an identity of the directory
// (logchain.DirectoryIdentity: no stat, a platform that gives no inode,
// or inode 0, which a filesystem that numbers no file gives every
// directory) keys the chain by its handle (chainTaskKey), so two
// directories of such a filesystem never share one task. The key starts
// with "chain:", which no absolute path does, so it never collides with
// a file's lock.
func chainKeyOf(d *logchain.Description) string {
	if device, inode, ok := logchain.DirectoryIdentity(d.Candidate.DirInfo); ok {
		return fmt.Sprintf("chain:%d:%d/%s", device, inode, d.Candidate.Name)
	}
	return chainTaskKey(d.Response.Path)
}

// start starts the index task of a chain for the parts in s, or joins
// the one running for the chain, and returns the task (a copy) and
// whether it is new.
func (c *chainIndexTasks) start(s chainTaskStart) (tasks.Task, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startLocked(s)
}

// startLocked is start; the caller holds c.mu.
func (c *chainIndexTasks) startLocked(s chainTaskStart) (tasks.Task, bool) {
	task, isNew := c.tasks.CreateKeyed(chainIndexOperation, s.handle, s.key)
	if !isNew {
		// The running task is the manager's own, which its goroutine
		// changes under the manager's lock: only its ID (never changed)
		// is read here, and Get copies the rest under the lock.
		running, known := c.tasks.Get(task.TaskID)
		if !known {
			return tasks.Task{TaskID: task.TaskID, Path: s.handle, Operation: chainIndexOperation}, false
		}
		return *running, false
	}
	// A copy taken before the task goroutine exists: once it runs, it
	// changes the task's status under the manager's lock, and reading
	// the manager's own Task then would be a data race.
	snapshot := *task
	c.rememberLocked(s.key, chainTaskRecord{
		taskID: snapshot.TaskID, fingerprint: s.fingerprint, startedAt: snapshot.StartedAt,
	})
	run := &chainIndexRun{
		tasks: c.tasks, builds: c.builds, logger: c.logger, owner: c,
		taskID: snapshot.TaskID, start: s, window: c.builds.maxRunning,
	}
	c.tasks.ReportProgress(snapshot.TaskID, run.progress)
	// The task goroutine. It outlives the request that started it: no
	// request context reaches it, so a client that goes away stops
	// nothing. runDetached turns a panic inside it into a failed task.
	go runDetached(c.tasks, snapshot.TaskID, chainIndexOperation, c.logger, run.run)
	return snapshot, true
}

// rememberLocked records record as the last index task of the chain
// key, in place of any earlier one, then drops the records that have
// expired or are too many (pruneLocked). The caller holds c.mu.
func (c *chainIndexTasks) rememberLocked(key string, record chainTaskRecord) {
	c.records[key] = record
	c.pruneLocked()
}

// recordEnd records how the index task taskID of the chain key ended,
// when it is still the chain's last task: a newer task may have taken
// the record's place, or the record may have been dropped. It is called
// from the task goroutine just before the task is marked ended in the
// task manager, so a describe never finds the task ended without its
// end in the record.
func (c *chainIndexTasks) recordEnd(key, taskID string, status tasks.Status, err string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, recorded := c.records[key]
	if !recorded || record.taskID != taskID {
		return
	}
	record.end = &chainTaskEnd{status: status, err: err, at: c.now()}
	c.records[key] = record
	c.pruneLocked()
}

// pruneLocked brings the records without an end up to date with the
// task table (settleLocked), drops the records of tasks that ended more
// than the task TTL ago, then, while more than maxEndedChainRecords
// ended records remain, the ones that ended first. The caller holds
// c.mu; settleLocked takes the task manager's lock under it.
//
// SECURITY: the records are bounded whatever clients describe: one per
// chain whose task runs (the task holds the chain's key, so a chain has
// at most one), plus at most maxEndedChainRecords ended ones, each a few
// short strings. A task whose goroutine panicked never records its end,
// so without settleLocked its record would stay, uncapped, for as long
// as the server runs. Each call costs one pass over the records and one
// look into the task table per record without an end.
func (c *chainIndexTasks) pruneLocked() {
	now := c.now()
	ended := make([]string, 0, len(c.records))
	for key, record := range c.records {
		if record.end == nil {
			settled, running, kept := c.settleLocked(key, record)
			if running != nil || !kept {
				continue
			}
			record = settled
		}
		if c.expired(record, now) {
			delete(c.records, key)
			continue
		}
		ended = append(ended, key)
	}
	excess := len(ended) - maxEndedChainRecords
	if excess <= 0 {
		return
	}
	slices.SortFunc(ended, func(a, b string) int {
		return c.records[a].end.at.Compare(c.records[b].end.at)
	})
	for _, key := range ended[:excess] {
		delete(c.records, key)
	}
}

// settleLocked brings record, the record of the chain key without an
// end, up to date with the task table, and returns what it found:
//
//   - the task still runs: the record as it is, and the task (a copy);
//   - the task has ended in the table: that is a task whose goroutine
//     panicked (runDetached failed it before its run could record the
//     end). Its end is taken from the table now and kept in the record,
//     which is returned, so that it outlives the task's place there and
//     is capped and expired like any ended record;
//   - the table no longer holds the task: its end is lost, and the
//     record is dropped (kept is false).
//
// The caller holds c.mu; the task manager's lock is taken under it
// (Get), in the order the Locks section of chainIndexTasks gives.
func (c *chainIndexTasks) settleLocked(key string, record chainTaskRecord) (settled chainTaskRecord, running *tasks.Task, kept bool) {
	task, known := c.tasks.Get(record.taskID)
	switch {
	case !known:
		delete(c.records, key)
		return record, nil, false
	case !task.IsTerminal():
		return record, task, true
	}
	record.end = &chainTaskEnd{status: task.Status, err: task.Error, at: c.now()}
	// Go note: assigning to a key that is already in the map is allowed
	// while the caller ranges over the map (pruneLocked does); it neither
	// adds an entry nor makes the range visit one twice.
	c.records[key] = record
	return record, nil, true
}

// expired reports whether an ended record is older than the task TTL at
// now: the time the sweeper drops a finished task from the table.
func (c *chainIndexTasks) expired(record chainTaskRecord, now time.Time) bool {
	return now.Sub(record.end.at) > c.tasks.TTL()
}

// forDescription is the index_build of a chain's description d: for a
// pending chain, the task that builds the indexes it waits for, started
// now or joined, unless the last task of these same files failed; for
// every other chain, the last task started for it (lastLocked); nil
// when there is none.
func (c *chainIndexTasks) forDescription(d *logchain.Description) *rxtypes.SamplesIndexBuild {
	handle, key := d.Response.Path, chainKeyOf(d)
	c.mu.Lock()
	defer c.mu.Unlock()
	last := c.lastLocked(key, d.Response.Fingerprint)
	failedForTheseFiles := last != nil && last.Status == tasks.StatusFailed
	waiting := d.WaitingParts()
	if d.Response.State != rxtypes.ChainStatePending || len(waiting) == 0 || failedForTheseFiles {
		if last == nil {
			return nil
		}
		return chainIndexBuildOf(*last, handle)
	}
	task, _ := c.startLocked(chainTaskStart{
		handle: handle, key: key, fingerprint: d.Response.Fingerprint, parts: waiting,
	})
	return chainIndexBuildOf(task, handle)
}

// last names the last index task started for the chain d describes, as
// lastLocked finds it, in a description's index_build; nil when there
// is none.
func (c *chainIndexTasks) last(d *logchain.Description) *rxtypes.SamplesIndexBuild {
	c.mu.Lock()
	defer c.mu.Unlock()
	task := c.lastLocked(chainKeyOf(d), d.Response.Fingerprint)
	if task == nil {
		return nil
	}
	return chainIndexBuildOf(*task, d.Response.Path)
}

// lastLocked returns the last index task started for the chain key, as
// a description of the chain's files with fingerprint shows it, or nil
// when there is none. A running task is read from the task manager; an
// ended one from its record, whether or not the task table still holds
// it.
//
// An ended record is dropped instead when it has expired, or when it
// was made for files with another fingerprint: its end describes files
// that are no longer the chain's. A running task is kept whatever its
// fingerprint: it holds the chain's key, so a start joins it anyway.
//
// A record without an end is settled against the table first
// (settleLocked). The caller holds c.mu.
func (c *chainIndexTasks) lastLocked(key, fingerprint string) *tasks.Task {
	record, recorded := c.records[key]
	if !recorded {
		return nil
	}
	if record.end == nil {
		settled, running, kept := c.settleLocked(key, record)
		switch {
		case running != nil:
			return running
		case !kept:
			return nil
		}
		record = settled
	}
	if c.expired(record, c.now()) || record.fingerprint != fingerprint {
		delete(c.records, key)
		return nil
	}
	task := record.task()
	return &task
}

// chainTaskMessages words a chain's index task by its status. Each
// format takes, by explicit argument index, the handle (1), the task's
// ID (2) and the task's error (3); a format need not use all three.
var chainTaskMessages = map[tasks.Status]string{
	tasks.StatusQueued:    "Building the line indexes of the parts of the log chain %[1]s; follow GET /v1/tasks/%[2]s",
	tasks.StatusRunning:   "Building the line indexes of the parts of the log chain %[1]s; follow GET /v1/tasks/%[2]s",
	tasks.StatusCompleted: "Built the line indexes of the parts of the log chain %[1]s (task %[2]s)",
	tasks.StatusFailed: "Building the line indexes of the parts of the log chain %[1]s failed (task %[2]s): %[3]s; " +
		"POST /v1/logs/index starts it again",
}

// chainIndexBuildOf names a chain's index task in a description's
// index_build, shown as handle. A failed task's message carries its
// error, the failing part and why, which outlives the task's place in
// the task table.
//
// Go note: the message is formatted here, not by taskResponseOf, whose
// format takes two arguments; the error is an argument, never part of
// a format, so a % in a file name is printed as it is.
func chainIndexBuildOf(task tasks.Task, handle string) *rxtypes.SamplesIndexBuild {
	started := formatTaskTime(task.StartedAt)
	return &rxtypes.SamplesIndexBuild{
		TaskID:    task.TaskID,
		Status:    string(task.Status),
		Message:   fmt.Sprintf(chainTaskMessages[task.Status], handle, task.TaskID, task.Error),
		Path:      handle,
		StartedAt: &started,
	}
}

// chainIndexRun is one run of a chain's index task: the parts it
// builds and how far it has got.
type chainIndexRun struct {
	tasks  *tasks.Manager
	builds *samplesIndexBuilds
	logger *slog.Logger
	// owner keeps the chain's record, where the run records its end.
	owner  *chainIndexTasks
	taskID string
	start  chainTaskStart
	// window is how many part builds of this run are in flight at most.
	window int

	// mu guards done and inFlight, which the task goroutine changes and
	// a status request reads (progress). It is never held while the task
	// manager's lock is taken.
	mu sync.Mutex
	// done is how many parts' builds have ended.
	done int
	// inFlight are the task IDs of the part builds submitted and not
	// ended yet.
	inFlight []string
}

// partWait is one part whose build the run waits for: its place in
// start.parts and the task that builds its index (or holds its path),
// with the watch the run reads the build's end from.
type partWait struct {
	position int
	build    chainPartBuild
}

// run is the body of the task goroutine: it submits the parts' builds,
// at most window at a time, waits for each, and ends the task: failed
// with the first part build that failed, completed otherwise.
//
// When the build queue is full, or the part builds of all chains take
// their half of it, a part is not submitted: the run waits for one of
// its own builds to end, or, with none in flight, waits in line with
// the other chains for room (samplesIndexBuilds.awaitChainRoom), and
// submits the part again. A build's end wakes one run in line, not all,
// so the waiting costs nothing while the queue stays full, and the work
// one build's end sets off does not grow with the chains that wait.
//
// # How the waits work
//
// A part's build is a task of its own, and its done channel closes when
// it ends (tasks.Watch.Done). The run cannot block on one done
// channel while others close first, so each submitted part gets a small
// waiter goroutine that blocks on its done channel and then sends the
// part on ended. The run blocks on ended alone: it wakes for whichever
// part ends first, and submits the next part in its place.
//
// ended has room for window values. At most window parts are in flight
// and each waiter sends once, so no waiter ever blocks on its send,
// even after the run has returned early on a failure and stopped
// receiving: the waiters left then end on their own once their builds
// do, and the channel is collected with them.
func (r *chainIndexRun) run() {
	r.tasks.MarkRunning(r.taskID)
	parts := r.start.parts
	ended := make(chan partWait, r.window)
	built := make([]bool, len(parts))
	next, inFlight := 0, 0
	for next < len(parts) || inFlight > 0 {
		for inFlight < r.window && next < len(parts) {
			wait, submitted := r.submit(next)
			if !submitted {
				break
			}
			r.awaitInBackground(wait, ended)
			next++
			inFlight++
		}
		if inFlight == 0 {
			// No room for the part, and no part of this run in flight:
			// block in line until a build's end leaves room and wakes this
			// run (at once, when room has freed since the refusal).
			<-r.builds.awaitChainRoom()
			continue
		}
		// Blocks until one in-flight part's build ends.
		wait := <-ended
		inFlight--
		r.finish(wait.build.taskID)
		ok, failure := r.outcome(wait)
		if failure != "" {
			r.fail(failure)
			return
		}
		built[wait.position] = ok
	}
	r.complete(built)
}

// fail ends the task as failed with failure: in the chain's record
// first, then in the task manager, so a describe never finds the task
// ended without its end recorded.
func (r *chainIndexRun) fail(failure string) {
	r.owner.recordEnd(r.start.key, r.taskID, tasks.StatusFailed, failure)
	r.tasks.Fail(r.taskID, failure)
}

// complete ends the task as completed, the parts in built as built, in
// the same order as fail.
func (r *chainIndexRun) complete(built []bool) {
	r.owner.recordEnd(r.start.key, r.taskID, tasks.StatusCompleted, "")
	r.tasks.Complete(r.taskID, r.result(built))
}

// submit starts the build of parts[position]'s line index, or joins the
// task that builds it or holds its path (samplesIndexBuilds.joinChainPart),
// and records it as in flight. It returns false when nothing holds the
// path and the build queue is full or the chains' half of it is taken:
// the part is to be submitted again once there is room.
//
// The run waits for the task whether it gives this file an index or
// not: when it does not (a compression holds the path, or a build of
// the file as it was before it changed), the part counts as neither
// built nor failed once that task ends (outcome).
func (r *chainIndexRun) submit(position int) (partWait, bool) {
	part := r.start.parts[position]
	identity := index.IdentityFromInfo(part.Path, part.Info)
	build, submitted := r.builds.joinChainPart(part.Path, identity, part.Info.Size())
	if !submitted {
		return partWait{}, false
	}
	r.mu.Lock()
	r.inFlight = append(r.inFlight, build.taskID)
	r.mu.Unlock()
	return partWait{position: position, build: build}, true
}

// closedChannel is a channel that is already closed: a receive from it
// returns at once.
var closedChannel = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// awaitInBackground starts the waiter goroutine of one in-flight part:
// it blocks until the part's task ends, then sends wait on ended. A task
// that could not be watched had ended already, so its waiter sends at
// once.
func (r *chainIndexRun) awaitInBackground(wait partWait, ended chan<- partWait) {
	var done <-chan struct{} = closedChannel
	if wait.build.watched {
		done = wait.build.watch.Done()
	}
	// The waiter goroutine. runDetached keeps a panic in it from
	// crashing the server; it cannot panic, but every goroutine of the
	// HTTP layer goes through it.
	go runDetached(r.tasks, r.taskID, chainIndexOperation, r.logger, func() {
		<-done
		ended <- wait
	})
}

// finish moves a part's build from in flight to done.
func (r *chainIndexRun) finish(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := slices.Index(r.inFlight, taskID); i >= 0 {
		r.inFlight = slices.Delete(r.inFlight, i, i+1)
	}
	r.done++
}

// outcome reads how the task a part waited for ended, from the watch
// taken when the part was submitted, so the answer is the same whether
// or not the task table still holds the task: built when the task gave
// this file an index and completed; a failure naming the part when it
// failed. A task that gives this file no index (a compression held the
// path, or a build of the file as it was before) builds nothing and
// fails nothing, and neither does one that had ended and left the table
// before it could be watched, whose end is unknown.
func (r *chainIndexRun) outcome(wait partWait) (built bool, failure string) {
	if !wait.build.watched || !wait.build.givesIndex {
		return false, ""
	}
	end := wait.build.watch.End()
	if end.Status == tasks.StatusFailed {
		return false, fmt.Sprintf("%s: %s", r.start.parts[wait.position].Name, end.Error)
	}
	return end.Status == tasks.StatusCompleted, ""
}

// progress is the task's progress: the share of its parts done, a part
// whose build runs counted by that build's own progress. It is the
// task's tasks.ProgressFunc, called from a status request's goroutine.
func (r *chainIndexRun) progress() (float64, bool) {
	total := len(r.start.parts)
	if total == 0 {
		return 1, true
	}
	r.mu.Lock()
	sum := float64(r.done)
	running := slices.Clone(r.inFlight)
	r.mu.Unlock()
	// The builds' own progress is read after r.mu is released: Get takes
	// the task manager's lock, and the two locks are never held together.
	for _, id := range running {
		if task, known := r.tasks.Get(id); known {
			if p := task.Progress(); p != nil {
				sum += *p
			}
		}
	}
	return min(sum/float64(total), 1), true
}

// result is the result the task completes with: the names of the parts
// whose index a build completed, in the chain's order.
func (r *chainIndexRun) result(built []bool) rxtypes.ChainIndexTaskResult {
	names := []string{}
	for position, ok := range built {
		if ok {
			names = append(names, r.start.parts[position].Name)
		}
	}
	return rxtypes.ChainIndexTaskResult{
		Path:  r.start.handle,
		Built: names,
		CLICommand: BuildCLICommand("logs_index", map[string]any{
			"path": r.start.handle, "force": r.start.force,
		}),
	}
}
