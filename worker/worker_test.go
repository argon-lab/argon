package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockJob implements the Job interface for testing
type MockJob struct {
	id       string
	priority int
	duration time.Duration
	fail     bool
	executed bool
	mu       sync.Mutex
}

func NewMockJob(id string, priority int, duration time.Duration, fail bool) *MockJob {
	return &MockJob{
		id:       id,
		priority: priority,
		duration: duration,
		fail:     fail,
	}
}

func (j *MockJob) ID() string {
	return j.id
}

func (j *MockJob) Priority() int {
	return j.priority
}

func (j *MockJob) Execute(ctx context.Context) error {
	j.mu.Lock()
	j.executed = true
	j.mu.Unlock()

	select {
	case <-time.After(j.duration):
		if j.fail {
			return fmt.Errorf("job %s failed", j.id)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (j *MockJob) IsExecuted() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.executed
}

func TestNewWorkerPool(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 4,
		QueueSize:  100,
	}

	pool := NewWorkerPool(config)
	assert.NotNil(t, pool)
	assert.Equal(t, 4, len(pool.workers))
	assert.Equal(t, 100, cap(pool.jobQueue))
}

func TestWorkerPoolStartStop(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 2,
		QueueSize:  10,
	}

	pool := NewWorkerPool(config)

	// Start pool
	err := pool.Start()
	assert.NoError(t, err)

	// Verify workers are running
	assert.True(t, pool.IsRunning())

	// Stop pool
	err = pool.Stop()
	assert.NoError(t, err)

	// Verify workers stopped
	assert.False(t, pool.IsRunning())
}

func TestSubmitJob(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 2,
		QueueSize:  10,
	}

	pool := NewWorkerPool(config)
	err := pool.Start()
	require.NoError(t, err)
	defer pool.Stop()

	// Submit a job
	job := NewMockJob("test-1", 1, 10*time.Millisecond, false)
	err = pool.Submit(job)
	assert.NoError(t, err)

	// Wait for job completion
	time.Sleep(50 * time.Millisecond)

	// Verify job was executed
	assert.True(t, job.IsExecuted())
}

func TestSubmitMultipleJobs(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 4,
		QueueSize:  100,
	}

	pool := NewWorkerPool(config)
	err := pool.Start()
	require.NoError(t, err)
	defer pool.Stop()

	// Submit multiple jobs
	jobs := make([]*MockJob, 20)
	for i := 0; i < 20; i++ {
		jobs[i] = NewMockJob(fmt.Sprintf("job-%d", i), i%3, 5*time.Millisecond, false)
		err := pool.Submit(jobs[i])
		assert.NoError(t, err)
	}

	// Wait for all jobs to complete
	time.Sleep(100 * time.Millisecond)

	// Verify all jobs were executed
	for i, job := range jobs {
		assert.True(t, job.IsExecuted(), "Job %d was not executed", i)
	}
}

func TestJobPriority(t *testing.T) {
	config := WorkerConfig{
		NumWorkers:   1, // Single worker to ensure sequential processing
		QueueSize:    100,
		UsePriority:  true,
	}

	pool := NewWorkerPool(config)
	err := pool.Start()
	require.NoError(t, err)
	defer pool.Stop()

	// Submit jobs with different priorities
	var executionOrder []string
	var mu sync.Mutex

	// Create jobs that record their execution order
	createJob := func(id string, priority int) *MockJob {
		job := NewMockJob(id, priority, 1*time.Millisecond, false)
		originalExecute := job.Execute
		job.Execute = func(ctx context.Context) error {
			mu.Lock()
			executionOrder = append(executionOrder, id)
			mu.Unlock()
			return originalExecute(ctx)
		}
		return job
	}

	// Submit low priority job first
	pool.Submit(createJob("low", 0))
	// Give it time to start processing
	time.Sleep(5 * time.Millisecond)
	
	// Submit high priority jobs
	pool.Submit(createJob("high-1", 10))
	pool.Submit(createJob("high-2", 10))
	pool.Submit(createJob("medium", 5))

	// Wait for completion
	time.Sleep(50 * time.Millisecond)

	// Verify high priority jobs were executed before medium priority
	mu.Lock()
	defer mu.Unlock()
	
	// Find positions
	var highPos, mediumPos int
	for i, id := range executionOrder {
		if id == "high-1" || id == "high-2" {
			highPos = i
		}
		if id == "medium" {
			mediumPos = i
		}
	}
	
	assert.Less(t, highPos, mediumPos, "High priority job should execute before medium priority")
}

func TestJobFailure(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 2,
		QueueSize:  10,
	}

	pool := NewWorkerPool(config)
	err := pool.Start()
	require.NoError(t, err)
	defer pool.Stop()

	// Submit failing job
	failJob := NewMockJob("fail-1", 1, 10*time.Millisecond, true)
	err = pool.Submit(failJob)
	assert.NoError(t, err)

	// Submit successful job
	successJob := NewMockJob("success-1", 1, 10*time.Millisecond, false)
	err = pool.Submit(successJob)
	assert.NoError(t, err)

	// Wait for completion
	time.Sleep(50 * time.Millisecond)

	// Both jobs should have been executed
	assert.True(t, failJob.IsExecuted())
	assert.True(t, successJob.IsExecuted())

	// Check error metrics
	metrics := pool.GetMetrics()
	assert.Greater(t, metrics.FailedJobs, int64(0))
	assert.Greater(t, metrics.SuccessfulJobs, int64(0))
}

func TestWorkerPoolMetrics(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 4,
		QueueSize:  100,
	}

	pool := NewWorkerPool(config)
	err := pool.Start()
	require.NoError(t, err)
	defer pool.Stop()

	// Submit various jobs
	for i := 0; i < 10; i++ {
		fail := i%3 == 0 // Every third job fails
		job := NewMockJob(fmt.Sprintf("job-%d", i), 1, 5*time.Millisecond, fail)
		pool.Submit(job)
	}

	// Wait for completion
	time.Sleep(100 * time.Millisecond)

	// Check metrics
	metrics := pool.GetMetrics()
	assert.Equal(t, int64(10), metrics.TotalJobs)
	assert.Greater(t, metrics.SuccessfulJobs, int64(0))
	assert.Greater(t, metrics.FailedJobs, int64(0))
	assert.Equal(t, int64(10), metrics.SuccessfulJobs+metrics.FailedJobs)
	assert.Greater(t, metrics.TotalProcessingTime, time.Duration(0))
}

func TestWorkerPoolShutdown(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 2,
		QueueSize:  10,
	}

	pool := NewWorkerPool(config)
	err := pool.Start()
	require.NoError(t, err)

	// Submit long-running jobs
	for i := 0; i < 5; i++ {
		job := NewMockJob(fmt.Sprintf("long-%d", i), 1, 1*time.Second, false)
		pool.Submit(job)
	}

	// Give jobs time to start
	time.Sleep(50 * time.Millisecond)

	// Shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = pool.ShutdownGracefully(ctx)
	assert.NoError(t, err)

	// Pool should not be running
	assert.False(t, pool.IsRunning())
}

func TestBatchProcessing(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 2,
		QueueSize:  100,
		BatchSize:  5,
	}

	pool := NewWorkerPool(config)
	pool.EnableBatching(5, 50*time.Millisecond)
	
	err := pool.Start()
	require.NoError(t, err)
	defer pool.Stop()

	// Track batch execution
	var batchSizes []int
	var mu sync.Mutex

	// Submit batch jobs
	for i := 0; i < 12; i++ {
		job := &BatchJob{
			MockJob: *NewMockJob(fmt.Sprintf("batch-%d", i), 1, 1*time.Millisecond, false),
			OnBatch: func(size int) {
				mu.Lock()
				batchSizes = append(batchSizes, size)
				mu.Unlock()
			},
		}
		pool.Submit(job)
	}

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// Verify batching occurred
	mu.Lock()
	defer mu.Unlock()
	
	assert.Greater(t, len(batchSizes), 0)
	// Should have batches of size 5 (except possibly the last one)
	for _, size := range batchSizes[:len(batchSizes)-1] {
		assert.Equal(t, 5, size)
	}
}

func TestRateLimiting(t *testing.T) {
	config := WorkerConfig{
		NumWorkers:    4,
		QueueSize:     100,
		RateLimit:     10, // 10 jobs per second
	}

	pool := NewWorkerPool(config)
	pool.EnableRateLimiting(10)
	
	err := pool.Start()
	require.NoError(t, err)
	defer pool.Stop()

	// Submit jobs rapidly
	start := time.Now()
	for i := 0; i < 20; i++ {
		job := NewMockJob(fmt.Sprintf("rate-%d", i), 1, 1*time.Millisecond, false)
		pool.Submit(job)
	}

	// Wait for completion
	time.Sleep(3 * time.Second)

	duration := time.Since(start)
	// With rate limit of 10/sec, 20 jobs should take ~2 seconds
	assert.Greater(t, duration, 1900*time.Millisecond)
}

func TestConcurrentJobSubmission(t *testing.T) {
	config := WorkerConfig{
		NumWorkers: 8,
		QueueSize:  1000,
	}

	pool := NewWorkerPool(config)
	err := pool.Start()
	require.NoError(t, err)
	defer pool.Stop()

	// Concurrent job submission
	var wg sync.WaitGroup
	jobCount := int64(0)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				job := NewMockJob(
					fmt.Sprintf("concurrent-%d-%d", goroutineID, j),
					j%3,
					1*time.Millisecond,
					false,
				)
				if err := pool.Submit(job); err == nil {
					atomic.AddInt64(&jobCount, 1)
				}
			}
		}(i)
	}

	wg.Wait()
	
	// Wait for processing
	time.Sleep(500 * time.Millisecond)

	// Verify all jobs were submitted
	assert.Equal(t, int64(1000), jobCount)

	// Verify metrics
	metrics := pool.GetMetrics()
	assert.Equal(t, int64(1000), metrics.TotalJobs)
}

func BenchmarkWorkerPoolThroughput(b *testing.B) {
	config := WorkerConfig{
		NumWorkers: 8,
		QueueSize:  1000,
	}

	pool := NewWorkerPool(config)
	pool.Start()
	defer pool.Stop()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		job := NewMockJob(fmt.Sprintf("bench-%d", i), 1, 0, false)
		pool.Submit(job)
	}

	// Wait for all jobs to complete
	for pool.GetMetrics().TotalJobs < int64(b.N) {
		time.Sleep(10 * time.Millisecond)
	}
}

func BenchmarkJobExecution(b *testing.B) {
	job := NewMockJob("bench", 1, 0, false)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		job.Execute(ctx)
	}
}

// BatchJob extends MockJob to track batch execution
type BatchJob struct {
	MockJob
	OnBatch func(size int)
}

func (b *BatchJob) Execute(ctx context.Context) error {
	if b.OnBatch != nil {
		b.OnBatch(1) // For testing, we track individual execution
	}
	return b.MockJob.Execute(ctx)
}