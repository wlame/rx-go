package webapi

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// respondAsync is the RFC 7240 preference a client sends to say it can
// take a 202 and follow the task, and the value of Preference-Applied
// when the server did so.
const respondAsync = "respond-async"

// prefersRespondAsync reports whether a Prefer header value holds the
// respond-async preference. The value is a comma-separated list of
// preferences, each a token with an optional "=value" and optional
// ";"-separated parameters; tokens compare without regard to case.
func prefersRespondAsync(header string) bool {
	for _, preference := range strings.Split(header, ",") {
		token, _, _ := strings.Cut(preference, ";")
		token, _, _ = strings.Cut(token, "=")
		if strings.EqualFold(strings.TrimSpace(token), respondAsync) {
			return true
		}
	}
	return false
}

// samplesDeadline returns the deadline a samples request waits for an
// index build until: wait from now for a client that prefers
// respond-async, and none (a nil channel) for any other, whose request
// then waits as long as the build. stop releases the timer.
func samplesDeadline(prefer string, wait time.Duration) (deadline <-chan time.Time, stop func()) {
	if !prefersRespondAsync(prefer) {
		return nil, func() {}
	}
	// time.NewTimer delivers on timer.C once wait has passed.
	timer := time.NewTimer(wait)
	return timer.C, func() { timer.Stop() }
}

// indexOperation is the task operation of a line-index build, whoever
// started it: POST /v1/index or a samples lookup.
const indexOperation = "index"

// samplesIndexBuilder builds and stores the line index of path,
// counting its reads into progress, and returns the index and where it
// was stored. samples.BuildIndex in production.
type samplesIndexBuilder func(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error)

// samplesIndexBuilds runs the line-index builds GET /v1/samples needs,
// at most one per file at a time, and lets any number of requests wait
// for the same one.
//
// # How a build and its waiters meet
//
// A build is a background task of the task manager, so it outlives the
// request that started it and anyone can follow it at
// GET /v1/tasks/{id}. The manager already allows one task per path, and
// hands back the running task to a second Create for the same path;
// that is what makes the build shared.
//
// Three kinds of goroutine take part:
//
//  1. The request goroutine that finds no build for the file creates the
//     task and starts (2) with the `go` statement in join. It does not
//     run the build itself.
//  2. The build goroutine runs the build, then marks the task completed
//     or failed. Marking it terminal closes the task's done channel
//     (tasks.Manager.Done).
//  3. Every request goroutine, the one that started the build included,
//     then waits in await's `select` for whichever comes first: the done
//     channel closing (the build ended: answer the lines), its own
//     deadline timer firing (answer 202 with the task), or its request
//     context ending (the client went away: give up). A closed channel
//     wakes every goroutine receiving from it, so one close releases all
//     the waiters at once, however many there are.
//
// Only a client that sends `Prefer: respond-async` (RFC 7240) has a
// deadline: it can follow a task. Any other client, an older viewer or a
// script, waits as long as the build and gets the lines, as it did when
// the build ran inside its request; it still shares the one build.
//
// A waiter that leaves, by its deadline or because its client
// disconnected, only stops waiting; the build goes on. The other waiters
// still get its index, and so does every later lookup, because the
// build stores it in the cache.
//
// # Keyed by path and identity
//
// running remembers, per path, the identity (index.SourceIdentity) of
// the file each build of ours was started for. A request for a file
// whose identity has changed since (rewritten, grown, replaced) does not
// wait for that build: its index will describe the earlier file and be
// refused for this one. The task manager will not start a second build
// for the path while the first runs, so such a request answers from the
// file without an index, which gives the same lines, only slower.
//
// # A limit on the builds running at once
//
// At most maxRunning builds run at a time (RX_MAX_INDEX_BUILDS). A
// build started past that waits in queue, in the order it was started,
// as a task whose status stays queued; it is a task from the start, so
// a lookup can name it and wait for it like a running one. A queued
// build is an entry in a slice, not a goroutine: its build goroutine is
// started only when a running build ends and frees its slot (finish).
// The queue holds at most maxQueued builds. Past that a lookup starts
// no build: an answer from the head names none, and a lookup that needs
// the index reads the file without one, as when a compression holds
// the file. POST /v1/index starts its builds outside this limit.
//
// # Half the queue for the parts of log chains
//
// The index tasks of log chains submit their parts' builds here too
// (joinChainPart). Each chain keeps only a few of its parts in flight,
// but many chains at once could still take every place in the queue,
// and a lookup in a large file would then read it without an index. So
// the builds joinChainPart creates, queued or running, are counted in
// chainParts, and while they number half the queue (maxChainParts) a
// chain starts no other: its task waits for a build to end, as when the
// queue is full. The other half is always left to lookups.
//
// # Chains that wait for room, in line
//
// A chain's task that finds no room for a part, and has none of its
// parts in flight, waits in line (chainWaiters, awaitChainRoom), each
// task on a channel of its own. A build's end that leaves room wakes
// the first task in line, one task and not all of them, so the work a
// build's end sets off does not grow with the number of chains waiting.
// The woken task takes the room, or, when its part needs none (a build
// of the part runs already), passes the wake-up on to the next.
//
// INVARIANT: while tasks wait in line, a build of this registry is
// queued or running, and its end will wake one. A task joins the line
// only when there is no room, under the lock that finish frees room
// under, and no room means a full queue (a build holds every slot) or
// the chains' half taken (chain part builds are queued or running).
type samplesIndexBuilds struct {
	tasks  *tasks.Manager
	logger *slog.Logger
	build  samplesIndexBuilder

	// maxRunning is how many builds run at once, maxQueued how many
	// wait for a slot.
	maxRunning int
	maxQueued  int

	// mu guards running, active, queue and the chain fields below. It is
	// taken before the task
	// manager's own lock (join calls Create while holding it), never the
	// other way round, so the two cannot deadlock.
	mu      sync.Mutex
	running map[string]runningIndexBuild
	// active is how many builds hold a slot: their goroutine has been
	// started and has not finished.
	active int
	// queue lists the builds waiting for a slot, the oldest first.
	queue []queuedIndexBuild
	// chainParts is how many builds that joinChainPart created are
	// queued or running: counted up when one is created, down when it
	// finishes (finish).
	chainParts int
	// chainWaiters lists the index tasks of log chains that wait for room
	// to submit a part, oldest first: each is the channel its task blocks
	// on, closed when the task is woken (wakeChainWaiterLocked).
	chainWaiters []chan struct{}
	// chainPartJoins counts the calls of joinChainPart: how many times
	// the index tasks of log chains have asked for a part's build. Tests
	// read it to see how much work one build's end sets off.
	chainPartJoins int
}

// maxQueuedIndexBuilds is how many samples index builds may wait for a
// slot at once: as many as the task table keeps, so a tree's worth of
// large files can each get its build while the queue stays a bounded
// list.
const maxQueuedIndexBuilds = tasks.DefaultMaxTasks

// queuedIndexBuild is a build whose task exists and whose goroutine
// has not been started yet.
type queuedIndexBuild struct {
	taskID string
	path   string
	size   int64
	// chainPart says that a log chain's index task created the build
	// (joinChainPart): it counts in chainParts until it finishes.
	chainPart bool
}

// runningIndexBuild is one build this registry started and that has not
// returned yet.
type runningIndexBuild struct {
	taskID   string
	identity index.SourceIdentity
	// reach is how far the head of the file reaches, once a lookup the
	// head could not answer has measured it (samples.HeadReach); nil
	// until then. It lives as long as the build: once the build has
	// stored the index, lookups no longer read the head.
	reach *samples.HeadReach
}

// newSamplesIndexBuilds returns a registry that runs at most
// maxRunning builds at once.
func newSamplesIndexBuilds(manager *tasks.Manager, logger *slog.Logger, build samplesIndexBuilder, maxRunning int) *samplesIndexBuilds {
	return &samplesIndexBuilds{
		tasks:      manager,
		logger:     logger,
		build:      build,
		maxRunning: max(1, maxRunning),
		maxQueued:  maxQueuedIndexBuilds,
		running:    map[string]runningIndexBuild{},
	}
}

// await waits for the line index of path, as info saw the file, to be
// built, starting the build when none is running, until deadline
// delivers a value. A nil deadline never does: the wait lasts as long
// as the build. reach, when not nil, is how far the head of the file
// reaches, as the lookup's attempt to answer from the head measured it;
// the build keeps it for the next lookup (reachOf).
//
// It returns (nil, nil) when the lookup should go ahead and read the
// file: the build ended in time (whether it succeeded or not: an index
// only makes the answer faster), or no running build can give this file
// an index. It returns the body of a 202 answer when the deadline came
// first, and ctx's error when the request ended before either.
func (b *samplesIndexBuilds) await(
	ctx context.Context,
	path string,
	info os.FileInfo,
	reach *samples.HeadReach,
	deadline <-chan time.Time,
) (*rxtypes.TaskResponse, error) {
	identity := index.IdentityFromInfo(path, info)
	taskID, found := b.join(path, identity, info.Size())
	if !found {
		return nil, nil
	}
	b.keepReach(path, taskID, identity, reach)
	done, known := b.tasks.Done(taskID)
	if !known {
		// The task has already ended and been dropped from the table.
		return nil, nil
	}

	// select blocks until one of its channels is ready and runs that
	// case. If several are ready at once it picks one at random, which
	// is harmless here: a build that ended exactly at the deadline is
	// checked again below. A receive from a nil channel blocks forever,
	// so with a nil deadline that case is never chosen.
	select {
	case <-done:
		return nil, nil
	case <-ctx.Done():
		// ctx is the request's context: it ends when the client
		// disconnects or the server shuts down. Only this wait stops.
		return nil, fmt.Errorf("waiting for the index of %s: %w", path, ctx.Err())
	case <-deadline:
	}

	task, known := b.tasks.Get(taskID)
	if !known || task.IsTerminal() {
		return nil, nil
	}
	pending := taskResponseOf(task, path,
		"Building the line index of %s; poll GET /v1/tasks/%s and ask again when it completes")
	return &pending, nil
}

// start starts the build of path's line index for the file info
// describes, or joins the one running, without waiting for it, and
// returns the build's task for a samples answer's index_build. It
// returns nil when no task will give this file an index (join), or when
// the task has already ended and been dropped from the table.
//
// It is what a lookup answered from the head of the file calls: the
// lines are in hand, and the build goes on in the background (the
// build goroutine of join) so that the next lookup, one the head cannot
// answer, finds the index.
func (b *samplesIndexBuilds) start(path string, info os.FileInfo) *rxtypes.SamplesIndexBuild {
	taskID, found := b.join(path, index.IdentityFromInfo(path, info), info.Size())
	if !found {
		return nil
	}
	task, known := b.tasks.Get(taskID)
	if !known {
		return nil
	}
	named := taskResponseOf(task, path, "Building the line index of %s in the background; follow GET /v1/tasks/%s")
	return &rxtypes.SamplesIndexBuild{
		TaskID: named.TaskID, Status: named.Status, Message: named.Message,
		Path: named.Path, StartedAt: named.StartedAt,
	}
}

// reachOf returns how far the head of path reaches, as kept by the
// running build of the file identity describes, and nil when no build
// of that identity keeps one. The answer is a copy, safe to use after
// the lock is released.
func (b *samplesIndexBuilds) reachOf(path string, identity index.SourceIdentity) *samples.HeadReach {
	b.mu.Lock()
	defer b.mu.Unlock()
	build, ok := b.running[path]
	if !ok || build.reach == nil || !build.identity.Equal(identity) {
		return nil
	}
	reach := *build.reach
	return &reach
}

// keepReach records reach, measured for the file identity describes,
// on taskID's build of path, when that build is still running for the
// same identity. A build that has ended, or one of another identity,
// keeps nothing: its reach would describe another text.
func (b *samplesIndexBuilds) keepReach(path, taskID string, identity index.SourceIdentity, reach *samples.HeadReach) {
	if reach == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	build, ok := b.running[path]
	if !ok || build.taskID != taskID || !build.identity.Equal(identity) {
		return
	}
	kept := *reach
	build.reach = &kept
	b.running[path] = build
}

// taskResponseOf is the body that names task, a build of path's line
// index, with message, a format whose two verbs take the path and the
// task's ID.
func taskResponseOf(task *tasks.Task, path, message string) rxtypes.TaskResponse {
	started := formatTaskTime(task.StartedAt)
	return rxtypes.TaskResponse{
		TaskID:    task.TaskID,
		Status:    string(task.Status),
		Message:   fmt.Sprintf(message, path, task.TaskID),
		Path:      path,
		StartedAt: &started,
	}
}

// join returns the task building path's index for the file identity
// describes, starting one when nothing holds the path (or queueing it,
// past the limit on running builds), and false when no task will give
// that file an index: a compress task holds the path, a build of ours
// for an earlier version of the file does, or the queue is full and
// nothing holds the path.
//
// An index task started by POST /v1/index is joined as well: it builds
// the same line index, with an analysis on top when it was asked for
// one.
func (b *samplesIndexBuilds) join(path string, identity index.SourceIdentity, size int64) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if build, ok := b.running[path]; ok {
		return build.taskID, build.identity.Equal(identity)
	}
	if b.queueFullLocked() {
		return b.holderLocked(path)
	}
	task, isNew := b.tasks.Create(path, indexOperation)
	if !isNew {
		return task.TaskID, task.Operation == indexOperation
	}
	b.addLocked(queuedIndexBuild{taskID: task.TaskID, path: path, size: size}, identity)
	return task.TaskID, true
}

// chainPartBuild is the task one part of a log chain's index task waits
// for, as joinChainPart found or started it.
type chainPartBuild struct {
	// taskID is the task's ID.
	taskID string
	// watch says when the task ends and how, even once the task has
	// left the task table; it is valid only when watched is true.
	watch   tasks.Watch
	watched bool
	// givesIndex says that the task builds this file's line index. It is
	// false for a compression that holds the path, and for a build of
	// the file as it was before it changed.
	givesIndex bool
}

// chainPartOf names the task taskID of manager as the build a chain's
// part waits for, with a watch of it. watched is false when the task had
// already ended and left the table: its end is then unknown.
func chainPartOf(manager *tasks.Manager, taskID string, givesIndex bool) chainPartBuild {
	watch, watched := manager.Watch(taskID)
	return chainPartBuild{taskID: taskID, watch: watch, watched: watched, givesIndex: givesIndex}
}

// joinChainPart is join for one part of a log chain's index task: it
// returns the task that builds path's index for the file identity
// describes, or that holds the path, and false when the part is to be
// submitted again once there is room (awaitChainRoom): nothing holds
// the path, and the queue is full or the chains' builds already take
// their half of it (maxChainParts).
//
// When it leaves room behind, it wakes the next chain's task in line
// (wakeChainWaiterLocked): a task woken for room its part did not need
// passes the wake-up on, so no task is left waiting while room is free.
//
// A build it starts is a subtask (tasks.Manager.CreateSubtask): once
// finished it counts toward the cap of the subtasks, so the parts of a
// chain of any size never drop another client's task from the table.
// The answer carries a watch of the task, taken while the task is in
// the table, so the chain reads how the part's build ended even after
// the task has left the table.
func (b *samplesIndexBuilds) joinChainPart(path string, identity index.SourceIdentity, size int64) (chainPartBuild, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Go note: deferred calls run last-in first-out, so this one runs
	// before the Unlock deferred above, while b.mu is still held.
	defer b.wakeChainWaiterLocked()
	b.chainPartJoins++

	if build, ok := b.running[path]; ok {
		return chainPartOf(b.tasks, build.taskID, build.identity.Equal(identity)), true
	}
	if b.queueFullLocked() || b.chainParts >= b.maxChainParts() {
		taskID, givesIndex := b.holderLocked(path)
		if taskID == "" {
			return chainPartBuild{}, false
		}
		return chainPartOf(b.tasks, taskID, givesIndex), true
	}
	task, isNew := b.tasks.CreateSubtask(path, indexOperation)
	if !isNew {
		return chainPartOf(b.tasks, task.TaskID, task.Operation == indexOperation), true
	}
	// The watch is taken before the build goroutine exists, so the task
	// cannot have ended, let alone left the table.
	part := chainPartOf(b.tasks, task.TaskID, true)
	b.chainParts++
	b.addLocked(queuedIndexBuild{taskID: task.TaskID, path: path, size: size, chainPart: true}, identity)
	return part, true
}

// maxChainParts is how many builds the index tasks of log chains may
// have queued or running at once, all chains together: half the queue,
// and at least one.
//
// SECURITY: however many chains clients describe, their part builds
// take at most this many places, so a lookup in another file always
// finds the other half of the queue free of them.
func (b *samplesIndexBuilds) maxChainParts() int {
	return max(1, b.maxQueued/2)
}

// chainRoomLocked reports whether a chain's task may create a part
// build now: the queue has room, and the chains' builds take less than
// their half of it. joinChainPart refuses a part exactly when this is
// false and nothing holds the part's path. The caller holds b.mu.
func (b *samplesIndexBuilds) chainRoomLocked() bool {
	return !b.queueFullLocked() && b.chainParts < b.maxChainParts()
}

// awaitChainRoom returns the channel a chain's task receives from before
// it submits a part again, once joinChainPart refused the part and none
// of the task's parts is in flight. The channel is closed already when
// there is room now. Otherwise the task joins the end of the line of
// waiting tasks, and the channel is closed when the task is first in
// line and a build's end leaves room (wakeChainWaiterLocked).
//
// The check for room and the joining of the line happen under one hold
// of b.mu, the lock finish frees room under, so no build can end
// between them unseen: a wake-up is never lost.
//
// SECURITY: the line holds at most one entry per unfinished chain index
// task, and a build's end wakes one of them, whatever their number.
func (b *samplesIndexBuilds) awaitChainRoom() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.chainRoomLocked() {
		return closedChannel
	}
	wake := make(chan struct{})
	b.chainWaiters = append(b.chainWaiters, wake)
	return wake
}

// wakeChainWaiterLocked wakes the first chain's task in line when there
// is room for a part build: one task, which then tries its part again.
// The caller holds b.mu.
//
// Go note: closing a channel releases the goroutine blocked on it. Each
// channel belongs to one waiting task, leaves the line when it is
// closed, and is closed once.
func (b *samplesIndexBuilds) wakeChainWaiterLocked() {
	if len(b.chainWaiters) == 0 || !b.chainRoomLocked() {
		return
	}
	close(b.chainWaiters[0])
	// Clear the slot before reslicing, so the backing array does not keep
	// the closed channel alive.
	b.chainWaiters[0] = nil
	b.chainWaiters = b.chainWaiters[1:]
}

// queueFullLocked reports whether there is no room for another build:
// every slot is taken and the queue is full. The caller holds b.mu.
func (b *samplesIndexBuilds) queueFullLocked() bool {
	return b.active >= b.maxRunning && len(b.queue) >= b.maxQueued
}

// holderLocked returns the unfinished task that holds path and whether
// it builds an index, or "" and false when none does. It is what a
// lookup gets when there is no room for another build: a task already
// holding the path is still joined, and none is created. The caller
// holds b.mu.
func (b *samplesIndexBuilds) holderLocked(path string) (string, bool) {
	holder, held := b.tasks.Holder(path)
	if !held {
		return "", false
	}
	return holder.TaskID, holder.Operation == indexOperation
}

// addLocked records build, a new task for the file identity describes,
// as the running build of its path, and starts it when a slot is free
// or queues it otherwise. The caller holds b.mu.
func (b *samplesIndexBuilds) addLocked(build queuedIndexBuild, identity index.SourceIdentity) {
	b.running[build.path] = runningIndexBuild{taskID: build.taskID, identity: identity}
	if b.active < b.maxRunning {
		b.startLocked(build)
		return
	}
	b.queue = append(b.queue, build)
}

// startLocked takes a slot for build and starts its goroutine. The
// caller holds b.mu.
func (b *samplesIndexBuilds) startLocked(build queuedIndexBuild) {
	b.active++
	// The build goroutine. runDetached turns a panic inside it into a
	// failed task instead of a crashed server, and its task ending
	// closes the done channel every waiter selects on.
	go runDetached(b.tasks, build.taskID, indexOperation, b.logger, func() {
		b.run(build)
	})
}

// run is the body of the build goroutine for build's task.
func (b *samplesIndexBuilds) run(build queuedIndexBuild) {
	// Deferred so that it also runs when the build panics: deferred
	// calls run while a panic unwinds, before runDetached recovers it.
	// The slot is freed whatever happened, so a failed build never
	// holds back the queue.
	defer b.finish(build)

	taskID, path := build.taskID, build.path
	b.tasks.MarkRunning(taskID)
	progress := &index.Progress{}
	b.tasks.ReportProgress(taskID, progress.Fraction)

	idx, cachePath, err := b.build(path, progress)
	if err != nil {
		b.tasks.Fail(taskID, fmt.Sprintf("build index: %v", err))
		return
	}
	b.tasks.Complete(taskID, indexTaskResultFrom(idx, cachePath, path, samplesIndexRequest(path, build.size)))
}

// finish runs once build has returned: it drops the path's entry,
// unless a newer build has taken its place, frees the build's slot and,
// for a chain's part, its place in chainParts, starts the oldest queued
// builds while slots are free, and wakes the first chain's task waiting
// for room, when there is room now.
func (b *samplesIndexBuilds) finish(build queuedIndexBuild) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running[build.path].taskID == build.taskID {
		delete(b.running, build.path)
	}
	if build.chainPart {
		b.chainParts--
	}
	b.active--
	for b.active < b.maxRunning && len(b.queue) > 0 {
		next := b.queue[0]
		// Clear the slot before reslicing, so the backing array does
		// not keep the started build's path alive.
		b.queue[0] = queuedIndexBuild{}
		b.queue = b.queue[1:]
		b.startLocked(next)
	}
	b.wakeChainWaiterLocked()
}

// samplesIndexRequest is the POST /v1/index request that builds what a
// samples lookup builds: a line index without analysis. It is what the
// task's cli_command reproduces. `rx index` skips a file below the
// large-file size unless told otherwise, and samples indexes every
// compressed file, so a smaller one gets --threshold=0.
func samplesIndexRequest(path string, size int64) rxtypes.IndexRequest {
	request := rxtypes.IndexRequest{Path: path}
	if size < int64(config.LargeFileMB())*1024*1024 {
		zero := 0
		request.Threshold = &zero
	}
	return request
}
