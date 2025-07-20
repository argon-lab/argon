package workers

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// JobType represents different types of background jobs
type JobType string

const (
	JobTypeSync        JobType = "sync"
	JobTypeCompression JobType = "compression"
	JobTypeNotification JobType = "notification"
	JobTypeCleanup     JobType = "cleanup"
)

// JobStatus represents the status of a job
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusRetrying  JobStatus = "retrying"
)

// JobPriority represents the priority of a job
type JobPriority int

const (
	JobPriorityLow    JobPriority = 1
	JobPriorityNormal JobPriority = 5
	JobPriorityHigh   JobPriority = 9
)

// Job represents a background job
type Job struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Type      JobType           `bson:"type" json:"type"`
	Status    JobStatus         `bson:"status" json:"status"`
	Priority  JobPriority       `bson:"priority" json:"priority"`
	
	// Job data
	Payload   map[string]interface{} `bson:"payload" json:"payload"`
	Result    map[string]interface{} `bson:"result,omitempty" json:"result,omitempty"`
	Error     string                 `bson:"error,omitempty" json:"error,omitempty"`
	
	// Retry configuration
	MaxRetries   int   `bson:"max_retries" json:"max_retries"`
	CurrentRetry int   `bson:"current_retry" json:"current_retry"`
	RetryDelay   int64 `bson:"retry_delay" json:"retry_delay"` // seconds
	
	// Timing
	CreatedAt   time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `bson:"updated_at" json:"updated_at"`
	StartedAt   *time.Time `bson:"started_at,omitempty" json:"started_at,omitempty"`
	CompletedAt *time.Time `bson:"completed_at,omitempty" json:"completed_at,omitempty"`
	
	// Worker information
	WorkerID string `bson:"worker_id,omitempty" json:"worker_id,omitempty"`
}

// SyncJobPayload represents the payload for sync jobs
type SyncJobPayload struct {
	BranchID     string                   `json:"branch_id"`
	ProjectID    string                   `json:"project_id"`
	Changes      []ChangeEventPayload     `json:"changes"`
	ResumeToken  interface{}              `json:"resume_token,omitempty"`
	BatchSize    int                      `json:"batch_size,omitempty"`
}

// ChangeEventPayload represents a change event in job payload
type ChangeEventPayload struct {
	OperationType string                 `json:"operation_type"`
	Collection    string                 `json:"collection"`
	DocumentID    interface{}            `json:"document_id"`
	FullDocument  map[string]interface{} `json:"full_document,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
}

// CompressionJobPayload represents the payload for compression jobs
type CompressionJobPayload struct {
	BranchID    string   `json:"branch_id"`
	ProjectID   string   `json:"project_id"`
	DeltaPaths  []string `json:"delta_paths"`
	TargetRatio float64  `json:"target_ratio"`
}

// NotificationJobPayload represents the payload for notification jobs
type NotificationJobPayload struct {
	Type      string                 `json:"type"`
	Recipients []string               `json:"recipients"`
	Subject   string                 `json:"subject"`
	Message   string                 `json:"message"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// CleanupJobPayload represents the payload for cleanup jobs
type CleanupJobPayload struct {
	BranchID      string    `json:"branch_id"`
	ProjectID     string    `json:"project_id"`
	CleanupType   string    `json:"cleanup_type"` // "archived", "expired", "unused"
	CutoffDate    time.Time `json:"cutoff_date"`
	DryRun        bool      `json:"dry_run"`
}

// WorkerStats represents statistics about worker performance
type WorkerStats struct {
	WorkerID      string    `json:"worker_id"`
	JobsProcessed int64     `json:"jobs_processed"`
	JobsSucceeded int64     `json:"jobs_succeeded"`
	JobsFailed    int64     `json:"jobs_failed"`
	AvgProcessTime float64   `json:"avg_process_time"`
	LastActiveAt  time.Time `json:"last_active_at"`
	IsActive      bool      `json:"is_active"`
}

// QueueStats represents statistics about the job queue
type QueueStats struct {
	TotalJobs     int64            `json:"total_jobs"`
	PendingJobs   int64            `json:"pending_jobs"`
	RunningJobs   int64            `json:"running_jobs"`
	CompletedJobs int64            `json:"completed_jobs"`
	FailedJobs    int64            `json:"failed_jobs"`
	JobsByType    map[JobType]int64 `json:"jobs_by_type"`
	AvgWaitTime   float64          `json:"avg_wait_time"`
}

// Worker interface defines the behavior of a background worker
type Worker interface {
	// Start begins processing jobs
	Start(ctx context.Context) error
	
	// Stop gracefully stops the worker
	Stop(ctx context.Context) error
	
	// ProcessJob processes a single job
	ProcessJob(ctx context.Context, job *Job) error
	
	// GetStats returns worker statistics
	GetStats() *WorkerStats
	
	// GetID returns the worker ID
	GetID() string
	
	// CanProcess returns true if the worker can process the given job type
	CanProcess(jobType JobType) bool
	
	// IsRunning returns true if the worker is currently running
	IsRunning() bool
}

// Queue interface defines the behavior of a job queue
type Queue interface {
	// Enqueue adds a job to the queue
	Enqueue(ctx context.Context, job *Job) error
	
	// Dequeue gets the next available job
	Dequeue(ctx context.Context, workerID string) (*Job, error)
	
	// UpdateJob updates a job's status and data
	UpdateJob(ctx context.Context, job *Job) error
	
	// GetJob retrieves a job by ID
	GetJob(ctx context.Context, jobID primitive.ObjectID) (*Job, error)
	
	// ListJobs lists jobs with filtering
	ListJobs(ctx context.Context, filter JobFilter) ([]*Job, error)
	
	// GetStats returns queue statistics
	GetStats(ctx context.Context) (*QueueStats, error)
	
	// Cleanup removes old completed/failed jobs
	Cleanup(ctx context.Context, cutoffDate time.Time) error
}

// JobFilter represents filters for listing jobs
type JobFilter struct {
	Status   []JobStatus `json:"status,omitempty"`
	Type     []JobType   `json:"type,omitempty"`
	Priority []JobPriority `json:"priority,omitempty"`
	Limit    int         `json:"limit,omitempty"`
	Offset   int         `json:"offset,omitempty"`
}

// WorkerPool interface defines the behavior of a worker pool
type WorkerPool interface {
	// Start starts all workers in the pool
	Start(ctx context.Context) error
	
	// Stop stops all workers gracefully
	Stop(ctx context.Context) error
	
	// SubmitJob submits a job to the pool
	SubmitJob(ctx context.Context, job *Job) error
	
	// GetWorkerStats returns statistics for all workers
	GetWorkerStats() []*WorkerStats
	
	// GetQueueStats returns queue statistics
	GetQueueStats(ctx context.Context) (*QueueStats, error)
	
	// ScaleWorkers adjusts the number of workers
	ScaleWorkers(ctx context.Context, targetCount int) error
	
	// GetWorkerCount returns the current number of workers
	GetWorkerCount() int
}