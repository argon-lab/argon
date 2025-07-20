package workers

import (
	"context"
	"fmt"
	"log"
	"sync"

	"argon/engine/internal/branch"
	"argon/engine/internal/storage"
)

// WorkerPoolImpl implements the WorkerPool interface
type WorkerPoolImpl struct {
	queue      Queue
	branchSvc  *branch.Service
	storageSvc storage.Service
	
	// Worker management
	workers    []Worker
	workersMu  sync.RWMutex
	running    bool
	runningMu  sync.RWMutex
	
	// Configuration
	maxWorkers int
	workerTypes map[JobType]int // How many workers per job type
	
	// Context management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool(queue Queue, branchSvc *branch.Service, storageSvc storage.Service) *WorkerPoolImpl {
	return &WorkerPoolImpl{
		queue:      queue,
		branchSvc:  branchSvc,
		storageSvc: storageSvc,
		maxWorkers: 10, // Default
		workerTypes: map[JobType]int{
			JobTypeSync:        5, // 5 sync workers
			JobTypeCompression: 2, // 2 compression workers
			JobTypeNotification: 1, // 1 notification worker
			JobTypeCleanup:     1, // 1 cleanup worker
		},
	}
}

// Start starts all workers in the pool
func (p *WorkerPoolImpl) Start(ctx context.Context) error {
	p.runningMu.Lock()
	defer p.runningMu.Unlock()
	
	if p.running {
		return fmt.Errorf("worker pool is already running")
	}
	
	p.ctx, p.cancel = context.WithCancel(ctx)
	
	// Initialize workers based on configuration
	if err := p.initializeWorkers(); err != nil {
		return fmt.Errorf("failed to initialize workers: %w", err)
	}
	
	// Start all workers
	for _, worker := range p.workers {
		p.wg.Add(1)
		go func(w Worker) {
			defer p.wg.Done()
			
			if err := w.Start(p.ctx); err != nil && err != context.Canceled {
				log.Printf("Worker %s stopped with error: %v", w.GetID(), err)
			}
		}(worker)
	}
	
	p.running = true
	log.Printf("Worker pool started with %d workers", len(p.workers))
	
	return nil
}

// Stop stops all workers gracefully
func (p *WorkerPoolImpl) Stop(ctx context.Context) error {
	p.runningMu.Lock()
	defer p.runningMu.Unlock()
	
	if !p.running {
		return nil
	}
	
	log.Println("Stopping worker pool...")
	
	// Cancel context to signal all workers to stop
	if p.cancel != nil {
		p.cancel()
	}
	
	// Stop all workers individually
	for _, worker := range p.workers {
		if err := worker.Stop(ctx); err != nil {
			log.Printf("Error stopping worker %s: %v", worker.GetID(), err)
		}
	}
	
	// Wait for all workers to finish
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	
	select {
	case <-done:
		log.Println("All workers stopped successfully")
	case <-ctx.Done():
		log.Println("Worker pool stop timed out")
		return ctx.Err()
	}
	
	p.running = false
	p.workers = nil
	
	return nil
}

// SubmitJob submits a job to the pool
func (p *WorkerPoolImpl) SubmitJob(ctx context.Context, job *Job) error {
	p.runningMu.RLock()
	running := p.running
	p.runningMu.RUnlock()
	
	if !running {
		return fmt.Errorf("worker pool is not running")
	}
	
	return p.queue.Enqueue(ctx, job)
}

// GetWorkerStats returns statistics for all workers
func (p *WorkerPoolImpl) GetWorkerStats() []*WorkerStats {
	p.workersMu.RLock()
	defer p.workersMu.RUnlock()
	
	stats := make([]*WorkerStats, len(p.workers))
	for i, worker := range p.workers {
		stats[i] = worker.GetStats()
	}
	
	return stats
}

// GetQueueStats returns queue statistics
func (p *WorkerPoolImpl) GetQueueStats(ctx context.Context) (*QueueStats, error) {
	return p.queue.GetStats(ctx)
}

// ScaleWorkers adjusts the number of workers
func (p *WorkerPoolImpl) ScaleWorkers(ctx context.Context, targetCount int) error {
	p.workersMu.Lock()
	defer p.workersMu.Unlock()
	
	currentCount := len(p.workers)
	
	if targetCount == currentCount {
		return nil // No change needed
	}
	
	if targetCount > currentCount {
		// Add workers
		for i := currentCount; i < targetCount; i++ {
			workerID := fmt.Sprintf("sync-worker-%d", i)
			worker := NewSyncWorker(workerID, p.queue, p.branchSvc, p.storageSvc)
			
			p.workers = append(p.workers, worker)
			
			// Start the worker if pool is running
			p.runningMu.RLock()
			if p.running {
				p.wg.Add(1)
				go func(w Worker) {
					defer p.wg.Done()
					
					if err := w.Start(p.ctx); err != nil && err != context.Canceled {
						log.Printf("Worker %s stopped with error: %v", w.GetID(), err)
					}
				}(worker)
			}
			p.runningMu.RUnlock()
		}
		
		log.Printf("Scaled worker pool from %d to %d workers", currentCount, targetCount)
		
	} else {
		// Remove workers (stop the extra ones)
		for i := targetCount; i < currentCount; i++ {
			worker := p.workers[i]
			if err := worker.Stop(ctx); err != nil {
				log.Printf("Error stopping worker %s during scaling: %v", worker.GetID(), err)
			}
		}
		
		p.workers = p.workers[:targetCount]
		log.Printf("Scaled worker pool from %d to %d workers", currentCount, targetCount)
	}
	
	return nil
}

// initializeWorkers creates workers based on configuration
func (p *WorkerPoolImpl) initializeWorkers() error {
	p.workersMu.Lock()
	defer p.workersMu.Unlock()
	
	p.workers = nil // Clear existing workers
	
	workerID := 0
	
	// Create sync workers
	syncCount := p.workerTypes[JobTypeSync]
	for i := 0; i < syncCount; i++ {
		worker := NewSyncWorker(
			fmt.Sprintf("sync-worker-%d", workerID),
			p.queue,
			p.branchSvc,
			p.storageSvc,
		)
		p.workers = append(p.workers, worker)
		workerID++
	}
	
	// TODO: Create other worker types (compression, notification, cleanup)
	// For now, just focusing on sync workers
	
	return nil
}

// SetWorkerConfiguration sets the number of workers per job type
func (p *WorkerPoolImpl) SetWorkerConfiguration(workerTypes map[JobType]int) {
	p.workerTypes = workerTypes
	
	// Calculate total max workers
	total := 0
	for _, count := range workerTypes {
		total += count
	}
	p.maxWorkers = total
}

// GetConfiguration returns the current worker configuration
func (p *WorkerPoolImpl) GetConfiguration() map[JobType]int {
	config := make(map[JobType]int)
	for jobType, count := range p.workerTypes {
		config[jobType] = count
	}
	return config
}

// IsRunning returns true if the worker pool is running
func (p *WorkerPoolImpl) IsRunning() bool {
	p.runningMu.RLock()
	defer p.runningMu.RUnlock()
	return p.running
}

// GetWorkerCount returns the current number of workers
func (p *WorkerPoolImpl) GetWorkerCount() int {
	p.workersMu.RLock()
	defer p.workersMu.RUnlock()
	return len(p.workers)
}

// Health check methods required by monitoring.WorkerPoolHealth interface

// IsHealthy returns true if the worker pool is healthy
func (p *WorkerPoolImpl) IsHealthy() bool {
	p.runningMu.RLock()
	running := p.running
	p.runningMu.RUnlock()
	
	if !running {
		return false
	}
	
	// Check if we have active workers
	p.workersMu.RLock()
	activeWorkers := len(p.workers)
	p.workersMu.RUnlock()
	
	return activeWorkers > 0
}

// GetActiveWorkers returns the number of active workers
func (p *WorkerPoolImpl) GetActiveWorkers() int {
	p.workersMu.RLock()
	defer p.workersMu.RUnlock()
	
	activeCount := 0
	for _, worker := range p.workers {
		if worker.IsRunning() {
			activeCount++
		}
	}
	
	return activeCount
}

// GetQueueSize returns the current queue size
func (p *WorkerPoolImpl) GetQueueSize() int {
	ctx := context.Background()
	stats, err := p.queue.GetStats(ctx)
	if err != nil {
		return -1 // Return -1 to indicate error
	}
	
	return int(stats.PendingJobs)
}

// GetProcessedJobs returns the total number of processed jobs
func (p *WorkerPoolImpl) GetProcessedJobs() int64 {
	ctx := context.Background()
	stats, err := p.queue.GetStats(ctx)
	if err != nil {
		return -1
	}
	
	return stats.CompletedJobs
}