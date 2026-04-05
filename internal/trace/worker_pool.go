package trace

import (
	"context"
	"sync"
)

// WorkerPool manages a pool of goroutines for parallel task processing
type WorkerPool struct {
	maxWorkers    int
	taskChan      chan Task
	matchChan     chan MatchResult // Stream matches here
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	patterns      []string
	caseSensitive bool
}

// NewWorkerPool creates a new worker pool with streaming support
func NewWorkerPool(maxWorkers int, patterns []string, caseSensitive bool, matchChan chan MatchResult) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())

	return &WorkerPool{
		maxWorkers:    maxWorkers,
		taskChan:      make(chan Task, maxWorkers*2), // Buffered channel
		matchChan:     matchChan,
		ctx:           ctx,
		cancel:        cancel,
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
	// Note: matchChan is owned by engine, not closed here
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

			// Process the task with streaming pipeline
			p.processTask(task)
		}
	}
}

// processTask executes a single task using streaming pipeline
func (p *WorkerPool) processTask(task Task) {
	// Create streaming pipeline
	pipeline := NewStreamingPipeline(p.ctx, task, p.patterns, p.caseSensitive, p.matchChan)

	// Execute pipeline - it will stream matches directly to matchChan
	// Errors are logged but don't stop processing (consistent with batch behavior)
	_ = pipeline.Run()
}
