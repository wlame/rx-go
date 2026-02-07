package trace

import (
	"context"
	"sync"
	"sync/atomic"
)

// WorkerPool manages a pool of goroutines for parallel task processing
type WorkerPool struct {
	maxWorkers     int
	taskChan       chan Task
	resultChan     chan Result
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	matchCounter   *int64 // Atomic counter for total matches
	maxResults     int    // Maximum results to collect (0 = unlimited)
	patterns       []string
	caseSensitive  bool
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool(maxWorkers, maxResults int, patterns []string, caseSensitive bool) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())
	var counter int64

	return &WorkerPool{
		maxWorkers:    maxWorkers,
		taskChan:      make(chan Task, maxWorkers*2),      // Buffered channel
		resultChan:    make(chan Result, maxWorkers*2),    // Buffered channel
		ctx:           ctx,
		cancel:        cancel,
		matchCounter:  &counter,
		maxResults:    maxResults,
		patterns:      patterns,
		caseSensitive: caseSensitive,
	}
}

// Start spawns worker goroutines
func (p *WorkerPool) Start() {
	for i := 0; i < p.maxWorkers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
}

// SubmitTask submits a task for processing
func (p *WorkerPool) SubmitTask(task Task) bool {
	select {
	case <-p.ctx.Done():
		return false
	case p.taskChan <- task:
		return true
	}
}

// Close closes the task channel and waits for workers to finish
func (p *WorkerPool) Close() {
	close(p.taskChan)
	p.wg.Wait()
	close(p.resultChan)
}

// Results returns the result channel
func (p *WorkerPool) Results() <-chan Result {
	return p.resultChan
}

// Cancel cancels all workers
func (p *WorkerPool) Cancel() {
	p.cancel()
}

// worker processes tasks from the task channel
func (p *WorkerPool) worker(id int) {
	defer p.wg.Done()

	for {
		select {
		case <-p.ctx.Done():
			return

		case task, ok := <-p.taskChan:
			if !ok {
				return
			}

			// Check if we've reached max results
			if p.maxResults > 0 {
				currentCount := atomic.LoadInt64(p.matchCounter)
				if int(currentCount) >= p.maxResults {
					// Send empty result and continue draining
					p.resultChan <- Result{
						TaskID:   task.ID,
						FilePath: task.FilePath,
						Matches:  nil,
						ChunkID:  task.ChunkID,
					}
					continue
				}
			}

			// Process the task
			result := p.processTask(task)

			// Update match counter
			if len(result.Matches) > 0 {
				atomic.AddInt64(p.matchCounter, int64(len(result.Matches)))
			}

			// Send result
			select {
			case <-p.ctx.Done():
				return
			case p.resultChan <- result:
			}
		}
	}
}

// processTask executes a single task
func (p *WorkerPool) processTask(task Task) Result {
	// Create pipeline
	pipeline := NewPipeline(p.ctx, task, p.patterns, p.caseSensitive)

	// Execute pipeline
	matches, err := pipeline.Run()

	return Result{
		TaskID:   task.ID,
		FilePath: task.FilePath,
		Matches:  matches,
		Error:    err,
		ChunkID:  task.ChunkID,
	}
}

// GetMatchCount returns the current match count
func (p *WorkerPool) GetMatchCount() int {
	return int(atomic.LoadInt64(p.matchCounter))
}
