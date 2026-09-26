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
// manager's path locks. The handle alone would collide with a task on
// the chain's active file, whose path equals the handle.
func chainTaskKey(handle string) string { return "chain:" + handle }

// chainIndexTasks runs the index task of each log chain: one background
// task per chain that builds the line indexes of the chain's parts
// through the per-file builds of samplesIndexBuilds, the builds a
// samples lookup starts and waits for.
//
// # One task per chain
//
// The task holds chainTaskKey(handle) in the task manager, so a second
// start for the same chain, from a describe or from POST
// /v1/logs/index, finds the running task and joins it (CreateKeyed
// hands it back). A joined task builds what its first start asked for:
// a POST with force=true that joins a running task does not force.
//
// # The parts, a few at a time
//
// A chain may have 10,000 parts. The task never hands them all to the
// build queue: it keeps at most maxRunning part builds of its own in
// flight (RX_MAX_INDEX_BUILDS, the number of builds that run at once)
// and submits the next part only when one of them ends. So a chain adds
// at most that many entries to the queue, whatever its size, and a
// samples lookup in another file still finds room in the queue.
//
// # What is remembered
//
// records maps each handle to the last index task started for it and
// the fingerprint of the chain's files then. A description shows that task
// in index_build while it runs and after it ends, as long as the task
// manager keeps it (RX_TASK_TTL_MINUTES after its end, or until the
// task table is full). A pending chain whose last task failed for the
// same files does not start another on describe: the same files would
// fail the same way, on every poll of a viewer. POST /v1/logs/index
// starts one whatever the last ended as.
//
// Lock order: mu, then the task manager's lock (CreateKeyed, Get). The
// task goroutine never takes mu.
type chainIndexTasks struct {
	tasks  *tasks.Manager
	builds *samplesIndexBuilds
	logger *slog.Logger

	mu sync.Mutex
	// records maps a handle to the last index task started for it.
	records map[string]chainTaskRecord
}

// chainTaskRecord is the last index task started for one chain.
type chainTaskRecord struct {
	taskID      string
	fingerprint string
}

// newChainIndexTasks returns a registry whose tasks build the parts'
// indexes through builds.
func newChainIndexTasks(manager *tasks.Manager, logger *slog.Logger, builds *samplesIndexBuilds) *chainIndexTasks {
	if logger == nil {
		logger = slog.Default()
	}
	return &chainIndexTasks{tasks: manager, builds: builds, logger: logger, records: map[string]chainTaskRecord{}}
}

// chainTaskStart is what starting a chain's index task needs: the
// chain's handle and fingerprint, the parts to build in the chain's
// order, and whether they are rebuilt even when current (for the
// task's cli_command).
type chainTaskStart struct {
	handle      string
	fingerprint string
	parts       []logchain.Part
	force       bool
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
	task, isNew := c.tasks.CreateKeyed(chainIndexOperation, s.handle, chainTaskKey(s.handle))
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
	c.rememberLocked(s.handle, chainTaskRecord{taskID: snapshot.TaskID, fingerprint: s.fingerprint})
	run := &chainIndexRun{
		tasks: c.tasks, builds: c.builds, logger: c.logger,
		taskID: snapshot.TaskID, start: s, window: c.builds.maxRunning,
	}
	c.tasks.ReportProgress(snapshot.TaskID, run.progress)
	// The task goroutine. It outlives the request that started it: no
	// request context reaches it, so a client that goes away stops
	// nothing. runDetached turns a panic inside it into a failed task.
	go runDetached(c.tasks, snapshot.TaskID, chainIndexOperation, c.logger, run.run)
	return snapshot, true
}

// rememberLocked records record as the last index task of handle, and
// drops the records of tasks the task manager no longer keeps.
//
// SECURITY: the map therefore holds at most one record per task the
// manager keeps (tasks.DefaultMaxTasks finished ones, plus the running
// ones), however many chains clients describe. The caller holds c.mu.
func (c *chainIndexTasks) rememberLocked(handle string, record chainTaskRecord) {
	for other, kept := range c.records {
		if _, known := c.tasks.Get(kept.taskID); !known {
			delete(c.records, other)
		}
	}
	c.records[handle] = record
}

// forDescription is the index_build of a chain's description d: for a
// pending chain, the task that builds the indexes it waits for, started
// now or joined, unless the last task of these same files failed; for
// every other chain, the last task started for it while the task
// manager keeps it; nil when there is none.
func (c *chainIndexTasks) forDescription(d *logchain.Description) *rxtypes.SamplesIndexBuild {
	handle, fingerprint := d.Response.Path, d.Response.Fingerprint
	c.mu.Lock()
	defer c.mu.Unlock()
	last, record := c.lastLocked(handle)
	failedForTheseFiles := last != nil && last.Status == tasks.StatusFailed && record.fingerprint == fingerprint
	waiting := d.WaitingParts()
	if d.Response.State != rxtypes.ChainStatePending || len(waiting) == 0 || failedForTheseFiles {
		if last == nil {
			return nil
		}
		return chainIndexBuildOf(*last, handle)
	}
	task, _ := c.startLocked(chainTaskStart{handle: handle, fingerprint: fingerprint, parts: waiting})
	return chainIndexBuildOf(task, handle)
}

// last names the last index task started for the chain handle, while
// the task manager keeps it, in a description's index_build; nil when
// there is none.
func (c *chainIndexTasks) last(handle string) *rxtypes.SamplesIndexBuild {
	c.mu.Lock()
	defer c.mu.Unlock()
	task, _ := c.lastLocked(handle)
	if task == nil {
		return nil
	}
	return chainIndexBuildOf(*task, handle)
}

// lastLocked returns a copy of the last index task started for handle
// and its record, or nil when there is none or the task manager no
// longer keeps it. The caller holds c.mu.
func (c *chainIndexTasks) lastLocked(handle string) (*tasks.Task, chainTaskRecord) {
	record, recorded := c.records[handle]
	if !recorded {
		return nil, chainTaskRecord{}
	}
	task, known := c.tasks.Get(record.taskID)
	if !known {
		return nil, record
	}
	return task, record
}

// chainTaskMessages words a chain's index task by its status. Each
// format takes the handle and the task's ID.
var chainTaskMessages = map[tasks.Status]string{
	tasks.StatusQueued:    "Building the line indexes of the parts of the log chain %s; follow GET /v1/tasks/%s",
	tasks.StatusRunning:   "Building the line indexes of the parts of the log chain %s; follow GET /v1/tasks/%s",
	tasks.StatusCompleted: "Built the line indexes of the parts of the log chain %s (GET /v1/tasks/%s)",
	tasks.StatusFailed:    "Building the line indexes of the parts of the log chain %s failed; GET /v1/tasks/%s says why",
}

// chainIndexBuildOf names a chain's index task in a description's
// index_build.
func chainIndexBuildOf(task tasks.Task, handle string) *rxtypes.SamplesIndexBuild {
	named := taskResponseOf(&task, handle, chainTaskMessages[task.Status])
	return &rxtypes.SamplesIndexBuild{
		TaskID: named.TaskID, Status: named.Status, Message: named.Message,
		Path: named.Path, StartedAt: named.StartedAt,
	}
}

// chainIndexRun is one run of a chain's index task: the parts it
// builds and how far it has got.
type chainIndexRun struct {
	tasks  *tasks.Manager
	builds *samplesIndexBuilds
	logger *slog.Logger
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
// start.parts and the task that builds its index (or holds its path).
type partWait struct {
	position int
	taskID   string
}

// run is the body of the task goroutine: it submits the parts' builds,
// at most window at a time, waits for each, and ends the task: failed
// with the first part build that failed, completed otherwise.
//
// When other files' builds fill the build queue, a part is not
// submitted: the run waits for one of its own builds to end, or, with
// none in flight, for a build that holds a slot to end (awaitASlot),
// and submits the part again. Each try therefore follows the end of a
// build, so the waiting costs nothing while the queue stays full.
//
// # How the waits work
//
// A part's build is a task of its own, and its done channel closes when
// it ends (tasks.Manager.Done). The run cannot block on one done
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
			// The queue is full and no part of this run is in flight.
			r.awaitASlot()
			continue
		}
		// Blocks until one in-flight part's build ends.
		wait := <-ended
		inFlight--
		r.finish(wait.taskID)
		ok, failure := r.outcome(wait)
		if failure != "" {
			r.tasks.Fail(r.taskID, failure)
			return
		}
		built[wait.position] = ok
	}
	r.tasks.Complete(r.taskID, r.result(built))
}

// submit starts the build of parts[position]'s line index, or joins the
// task that builds it or holds its path (samplesIndexBuilds.join), and
// records it as in flight. It returns false when the build queue is
// full and nothing holds the path: the part is to be submitted again
// once a build ends.
func (r *chainIndexRun) submit(position int) (partWait, bool) {
	part := r.start.parts[position]
	identity := index.IdentityFromInfo(part.Path, part.Info)
	// The task ID is the build's whether join says it gives this file
	// an index or not: when it does not (a compression holds the path,
	// or a build of the file as it was before it changed), the run waits
	// for that task to end, and the part counts as not built.
	taskID, _ := r.builds.join(part.Path, identity, part.Info.Size())
	if taskID == "" {
		return partWait{}, false
	}
	r.mu.Lock()
	r.inFlight = append(r.inFlight, taskID)
	r.mu.Unlock()
	return partWait{position: position, taskID: taskID}, true
}

// slotRetryDelay is how long awaitASlot waits when the build it would
// wait for has ended and not given its slot back yet.
const slotRetryDelay = 10 * time.Millisecond

// awaitASlot blocks until a build that holds a slot ends, so that a
// part refused by a full queue can be submitted again.
//
// A build whose task has ended gives its slot back a moment later, in
// the same goroutine (samplesIndexBuilds.finish). When the build found
// has ended already, or none is found in that moment, awaitASlot waits
// slotRetryDelay instead, so the retry never spins.
func (r *chainIndexRun) awaitASlot() {
	taskID, held := r.builds.slotHolder()
	done, known := r.tasks.Done(taskID)
	if !held || !known {
		time.Sleep(slotRetryDelay)
		return
	}
	// Go note: a select with a default case never blocks: it takes the
	// done case when the channel is closed already, and the default
	// otherwise.
	select {
	case <-done:
		time.Sleep(slotRetryDelay)
	default:
		<-done
	}
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
// the manager no longer keeps has ended, so its waiter sends at once.
func (r *chainIndexRun) awaitInBackground(wait partWait, ended chan<- partWait) {
	done, known := r.tasks.Done(wait.taskID)
	if !known {
		done = closedChannel
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

// outcome reads how the task a part waited for ended: built when it was
// an index build that completed; a failure naming the part when it was
// an index build that failed. A task of another operation (a
// compression held the path) builds nothing and fails nothing, and
// neither does a task the manager no longer keeps.
func (r *chainIndexRun) outcome(wait partWait) (built bool, failure string) {
	task, known := r.tasks.Get(wait.taskID)
	if !known || task.Operation != indexOperation {
		return false, ""
	}
	name := r.start.parts[wait.position].Name
	if task.Status == tasks.StatusFailed {
		return false, fmt.Sprintf("%s: %s", name, task.Error)
	}
	return task.Status == tasks.StatusCompleted, ""
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
