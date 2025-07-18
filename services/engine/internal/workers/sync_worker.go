package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"argon/engine/internal/branch"
	"argon/engine/internal/storage"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// SyncWorker handles synchronization of MongoDB changes to storage
type SyncWorker struct {
	id           string
	queue        Queue
	branchSvc    *branch.Service
	storageSvc   storage.Service
	
	// Statistics
	mu            sync.RWMutex
	jobsProcessed int64
	jobsSucceeded int64
	jobsFailed    int64
	totalDuration time.Duration
	lastActiveAt  time.Time
	isActive      bool
	
	// Configuration
	batchSize     int
	processTimeout time.Duration
}

// NewSyncWorker creates a new sync worker
func NewSyncWorker(id string, queue Queue, branchSvc *branch.Service, storageSvc storage.Service) *SyncWorker {
	return &SyncWorker{
		id:             id,
		queue:          queue,
		branchSvc:      branchSvc,
		storageSvc:     storageSvc,
		batchSize:      100, // Default batch size
		processTimeout: 5 * time.Minute, // Default timeout
		lastActiveAt:   time.Now(),
	}
}

// Start begins processing jobs
func (w *SyncWorker) Start(ctx context.Context) error {
	w.mu.Lock()
	w.isActive = true
	w.mu.Unlock()
	
	log.Printf("SyncWorker %s starting...", w.id)
	
	ticker := time.NewTicker(1 * time.Second) // Poll every second
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			log.Printf("SyncWorker %s stopping due to context cancellation", w.id)
			w.mu.Lock()
			w.isActive = false
			w.mu.Unlock()
			return ctx.Err()
			
		case <-ticker.C:
			// Try to get and process a job
			if err := w.processNextJob(ctx); err != nil {
				log.Printf("SyncWorker %s error processing job: %v", w.id, err)
			}
		}
	}
}

// Stop gracefully stops the worker
func (w *SyncWorker) Stop(ctx context.Context) error {
	w.mu.Lock()
	w.isActive = false
	w.mu.Unlock()
	
	log.Printf("SyncWorker %s stopped", w.id)
	return nil
}

// ProcessJob processes a single job
func (w *SyncWorker) ProcessJob(ctx context.Context, job *Job) error {
	if !w.CanProcess(job.Type) {
		return fmt.Errorf("worker %s cannot process job type %s", w.id, job.Type)
	}
	
	startTime := time.Now()
	defer func() {
		w.updateStats(time.Since(startTime))
	}()
	
	switch job.Type {
	case JobTypeSync:
		return w.processSyncJob(ctx, job)
	default:
		return fmt.Errorf("unsupported job type: %s", job.Type)
	}
}

// GetStats returns worker statistics
func (w *SyncWorker) GetStats() *WorkerStats {
	w.mu.RLock()
	defer w.mu.RUnlock()
	
	avgTime := float64(0)
	if w.jobsProcessed > 0 {
		avgTime = float64(w.totalDuration.Milliseconds()) / float64(w.jobsProcessed)
	}
	
	return &WorkerStats{
		WorkerID:       w.id,
		JobsProcessed:  w.jobsProcessed,
		JobsSucceeded:  w.jobsSucceeded,
		JobsFailed:     w.jobsFailed,
		AvgProcessTime: avgTime,
		LastActiveAt:   w.lastActiveAt,
		IsActive:       w.isActive,
	}
}

// GetID returns the worker ID
func (w *SyncWorker) GetID() string {
	return w.id
}

// CanProcess returns true if the worker can process the given job type
func (w *SyncWorker) CanProcess(jobType JobType) bool {
	return jobType == JobTypeSync
}

// processNextJob tries to dequeue and process the next available job
func (w *SyncWorker) processNextJob(ctx context.Context) error {
	// Create a timeout context for processing
	processCtx, cancel := context.WithTimeout(ctx, w.processTimeout)
	defer cancel()
	
	// Try to get a job
	job, err := w.queue.Dequeue(processCtx, w.id)
	if err != nil {
		return fmt.Errorf("failed to dequeue job: %w", err)
	}
	
	if job == nil {
		// No jobs available
		return nil
	}
	
	w.mu.Lock()
	w.lastActiveAt = time.Now()
	w.mu.Unlock()
	
	log.Printf("SyncWorker %s processing job %s (type: %s)", w.id, job.ID.Hex(), job.Type)
	
	// Process the job
	err = w.ProcessJob(processCtx, job)
	
	// Update job status
	if err != nil {
		job.Status = JobStatusFailed
		job.Error = err.Error()
		
		w.mu.Lock()
		w.jobsFailed++
		w.mu.Unlock()
		
		log.Printf("SyncWorker %s failed to process job %s: %v", w.id, job.ID.Hex(), err)
	} else {
		job.Status = JobStatusCompleted
		job.Error = ""
		
		w.mu.Lock()
		w.jobsSucceeded++
		w.mu.Unlock()
		
		log.Printf("SyncWorker %s completed job %s", w.id, job.ID.Hex())
	}
	
	// Update the job in the queue
	if updateErr := w.queue.UpdateJob(processCtx, job); updateErr != nil {
		log.Printf("SyncWorker %s failed to update job %s: %v", w.id, job.ID.Hex(), updateErr)
	}
	
	return err
}

// processSyncJob handles synchronization jobs
func (w *SyncWorker) processSyncJob(ctx context.Context, job *Job) error {
	// Parse the payload
	var payload SyncJobPayload
	payloadBytes, err := json.Marshal(job.Payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return fmt.Errorf("failed to unmarshal sync payload: %w", err)
	}
	
	// Validate payload
	if payload.BranchID == "" || payload.ProjectID == "" {
		return fmt.Errorf("missing required fields in sync payload")
	}
	
	if len(payload.Changes) == 0 {
		// No changes to process
		return nil
	}
	
	log.Printf("Processing %d changes for branch %s", len(payload.Changes), payload.BranchID)
	
	// Convert payload changes to ChangeEvent format
	changes := make([]branch.ChangeEvent, len(payload.Changes))
	branchObjID, err := primitive.ObjectIDFromHex(payload.BranchID)
	if err != nil {
		return fmt.Errorf("invalid branch ID: %w", err)
	}
	
	projectObjID, err := primitive.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("invalid project ID: %w", err)
	}
	
	for i, change := range payload.Changes {
		changes[i] = branch.ChangeEvent{
			ID:            primitive.NewObjectID(),
			ProjectID:     projectObjID,
			BranchID:      branchObjID,
			OperationType: change.OperationType,
			Collection:    change.Collection,
			DocumentID:    change.DocumentID,
			FullDocument:  change.FullDocument,
			Timestamp:     change.Timestamp,
		}
	}
	
	// Process changes in batches
	batchSize := w.batchSize
	if payload.BatchSize > 0 && payload.BatchSize < w.batchSize {
		batchSize = payload.BatchSize
	}
	
	totalProcessed := 0
	for i := 0; i < len(changes); i += batchSize {
		end := i + batchSize
		if end > len(changes) {
			end = len(changes)
		}
		
		batch := changes[i:end]
		
		// Store the batch of changes
		if err := w.branchSvc.StoreBranchChanges(ctx, branchObjID, batch); err != nil {
			return fmt.Errorf("failed to store changes batch %d-%d: %w", i, end-1, err)
		}
		
		totalProcessed += len(batch)
		
		// Check for context cancellation between batches
		select {
		case <-ctx.Done():
			return fmt.Errorf("sync cancelled after processing %d/%d changes", totalProcessed, len(changes))
		default:
		}
	}
	
	// Store result in job
	job.Result = map[string]interface{}{
		"changes_processed": totalProcessed,
		"batches_processed": (len(changes) + batchSize - 1) / batchSize,
		"resume_token":      payload.ResumeToken,
		"completed_at":      time.Now(),
	}
	
	log.Printf("Successfully processed %d changes for branch %s", totalProcessed, payload.BranchID)
	
	return nil
}

// updateStats updates worker statistics
func (w *SyncWorker) updateStats(duration time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	
	w.jobsProcessed++
	w.totalDuration += duration
	w.lastActiveAt = time.Now()
}

// SetBatchSize sets the batch size for processing changes
func (w *SyncWorker) SetBatchSize(size int) {
	if size > 0 {
		w.batchSize = size
	}
}

// SetProcessTimeout sets the timeout for processing individual jobs
func (w *SyncWorker) SetProcessTimeout(timeout time.Duration) {
	if timeout > 0 {
		w.processTimeout = timeout
	}
}