package workers

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoQueue implements the Queue interface using MongoDB
type MongoQueue struct {
	client     *mongo.Client
	db         *mongo.Database
	collection *mongo.Collection
}

// NewMongoQueue creates a new MongoDB-based job queue
func NewMongoQueue(client *mongo.Client, dbName string) *MongoQueue {
	db := client.Database(dbName)
	collection := db.Collection("jobs")
	
	return &MongoQueue{
		client:     client,
		db:         db,
		collection: collection,
	}
}

// Initialize sets up indexes and prepares the queue
func (q *MongoQueue) Initialize(ctx context.Context) error {
	// Create indexes for efficient job processing
	indexes := []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "status", Value: 1},
				{Key: "priority", Value: -1},
				{Key: "created_at", Value: 1},
			},
			Options: options.Index().SetName("status_priority_created"),
		},
		{
			Keys: bson.D{
				{Key: "type", Value: 1},
				{Key: "status", Value: 1},
			},
			Options: options.Index().SetName("type_status"),
		},
		{
			Keys: bson.D{
				{Key: "worker_id", Value: 1},
				{Key: "status", Value: 1},
			},
			Options: options.Index().SetName("worker_status"),
		},
		{
			Keys: bson.D{
				{Key: "created_at", Value: 1},
			},
			Options: options.Index().SetName("created_at").SetExpireAfterSeconds(7 * 24 * 60 * 60), // 7 days
		},
	}
	
	_, err := q.collection.Indexes().CreateMany(ctx, indexes)
	return err
}

// Enqueue adds a job to the queue
func (q *MongoQueue) Enqueue(ctx context.Context, job *Job) error {
	if job.ID.IsZero() {
		job.ID = primitive.NewObjectID()
	}
	
	now := time.Now()
	job.CreatedAt = now
	job.UpdatedAt = now
	job.Status = JobStatusPending
	
	// Set defaults
	if job.MaxRetries == 0 {
		job.MaxRetries = 3
	}
	if job.RetryDelay == 0 {
		job.RetryDelay = 30 // 30 seconds
	}
	if job.Priority == 0 {
		job.Priority = JobPriorityNormal
	}
	
	_, err := q.collection.InsertOne(ctx, job)
	return err
}

// Dequeue gets the next available job for processing
func (q *MongoQueue) Dequeue(ctx context.Context, workerID string) (*Job, error) {
	// Find and update the highest priority pending job atomically
	filter := bson.M{
		"status": JobStatusPending,
	}
	
	update := bson.M{
		"$set": bson.M{
			"status":     JobStatusRunning,
			"worker_id":  workerID,
			"started_at": time.Now(),
			"updated_at": time.Now(),
		},
	}
	
	opts := options.FindOneAndUpdate().
		SetSort(bson.D{
			{Key: "priority", Value: -1},
			{Key: "created_at", Value: 1},
		}).
		SetReturnDocument(options.After)
	
	var job Job
	err := q.collection.FindOneAndUpdate(ctx, filter, update, opts).Decode(&job)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil // No jobs available
		}
		return nil, err
	}
	
	return &job, nil
}

// UpdateJob updates a job's status and data
func (q *MongoQueue) UpdateJob(ctx context.Context, job *Job) error {
	job.UpdatedAt = time.Now()
	
	// Set completion time if job is completed or failed
	if job.Status == JobStatusCompleted || job.Status == JobStatusFailed {
		now := time.Now()
		job.CompletedAt = &now
	}
	
	filter := bson.M{"_id": job.ID}
	update := bson.M{"$set": job}
	
	_, err := q.collection.UpdateOne(ctx, filter, update)
	return err
}

// GetJob retrieves a job by ID
func (q *MongoQueue) GetJob(ctx context.Context, jobID primitive.ObjectID) (*Job, error) {
	var job Job
	err := q.collection.FindOne(ctx, bson.M{"_id": jobID}).Decode(&job)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("job not found: %s", jobID.Hex())
		}
		return nil, err
	}
	return &job, nil
}

// ListJobs lists jobs with filtering
func (q *MongoQueue) ListJobs(ctx context.Context, filter JobFilter) ([]*Job, error) {
	mongoFilter := bson.M{}
	
	if len(filter.Status) > 0 {
		mongoFilter["status"] = bson.M{"$in": filter.Status}
	}
	
	if len(filter.Type) > 0 {
		mongoFilter["type"] = bson.M{"$in": filter.Type}
	}
	
	if len(filter.Priority) > 0 {
		mongoFilter["priority"] = bson.M{"$in": filter.Priority}
	}
	
	opts := options.Find().
		SetSort(bson.D{
			{Key: "priority", Value: -1},
			{Key: "created_at", Value: -1},
		})
	
	if filter.Limit > 0 {
		opts.SetLimit(int64(filter.Limit))
	}
	
	if filter.Offset > 0 {
		opts.SetSkip(int64(filter.Offset))
	}
	
	cursor, err := q.collection.Find(ctx, mongoFilter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	
	var jobs []*Job
	for cursor.Next(ctx) {
		var job Job
		if err := cursor.Decode(&job); err != nil {
			return nil, err
		}
		jobs = append(jobs, &job)
	}
	
	return jobs, cursor.Err()
}

// GetStats returns queue statistics
func (q *MongoQueue) GetStats(ctx context.Context) (*QueueStats, error) {
	pipeline := []bson.M{
		{
			"$group": bson.M{
				"_id": "$status",
				"count": bson.M{"$sum": 1},
			},
		},
	}
	
	cursor, err := q.collection.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	
	stats := &QueueStats{
		JobsByType: make(map[JobType]int64),
	}
	
	for cursor.Next(ctx) {
		var result struct {
			ID    JobStatus `bson:"_id"`
			Count int64     `bson:"count"`
		}
		
		if err := cursor.Decode(&result); err != nil {
			continue
		}
		
		stats.TotalJobs += result.Count
		
		switch result.ID {
		case JobStatusPending:
			stats.PendingJobs = result.Count
		case JobStatusRunning:
			stats.RunningJobs = result.Count
		case JobStatusCompleted:
			stats.CompletedJobs = result.Count
		case JobStatusFailed:
			stats.FailedJobs = result.Count
		}
	}
	
	// Get job type distribution
	typePipeline := []bson.M{
		{
			"$group": bson.M{
				"_id": "$type",
				"count": bson.M{"$sum": 1},
			},
		},
	}
	
	typeCursor, err := q.collection.Aggregate(ctx, typePipeline)
	if err == nil {
		defer typeCursor.Close(ctx)
		
		for typeCursor.Next(ctx) {
			var result struct {
				ID    JobType `bson:"_id"`
				Count int64   `bson:"count"`
			}
			
			if err := typeCursor.Decode(&result); err == nil {
				stats.JobsByType[result.ID] = result.Count
			}
		}
	}
	
	// Calculate average wait time for recently completed jobs
	waitTimePipeline := []bson.M{
		{
			"$match": bson.M{
				"status":       bson.M{"$in": []JobStatus{JobStatusCompleted, JobStatusFailed}},
				"completed_at": bson.M{"$gte": time.Now().Add(-24 * time.Hour)}, // Last 24 hours
				"started_at":   bson.M{"$ne": nil},
			},
		},
		{
			"$project": bson.M{
				"wait_time": bson.M{
					"$subtract": []string{"$started_at", "$created_at"},
				},
			},
		},
		{
			"$group": bson.M{
				"_id": nil,
				"avg_wait_time": bson.M{"$avg": "$wait_time"},
			},
		},
	}
	
	waitCursor, err := q.collection.Aggregate(ctx, waitTimePipeline)
	if err == nil {
		defer waitCursor.Close(ctx)
		
		if waitCursor.Next(ctx) {
			var result struct {
				AvgWaitTime float64 `bson:"avg_wait_time"`
			}
			
			if err := waitCursor.Decode(&result); err == nil {
				stats.AvgWaitTime = result.AvgWaitTime / 1000 // Convert to seconds
			}
		}
	}
	
	return stats, nil
}

// Cleanup removes old completed/failed jobs
func (q *MongoQueue) Cleanup(ctx context.Context, cutoffDate time.Time) error {
	filter := bson.M{
		"status": bson.M{"$in": []JobStatus{JobStatusCompleted, JobStatusFailed}},
		"completed_at": bson.M{"$lt": cutoffDate},
	}
	
	result, err := q.collection.DeleteMany(ctx, filter)
	if err != nil {
		return err
	}
	
	fmt.Printf("Cleaned up %d old jobs\n", result.DeletedCount)
	return nil
}

// RetryFailedJobs moves failed jobs back to pending status if they have retries left
func (q *MongoQueue) RetryFailedJobs(ctx context.Context) error {
	filter := bson.M{
		"status": JobStatusFailed,
		"$expr": bson.M{
			"$lt": []string{"$current_retry", "$max_retries"},
		},
	}
	
	update := bson.M{
		"$set": bson.M{
			"status":     JobStatusPending,
			"worker_id":  "",
			"started_at": nil,
			"updated_at": time.Now(),
		},
		"$inc": bson.M{
			"current_retry": 1,
		},
	}
	
	result, err := q.collection.UpdateMany(ctx, filter, update)
	if err != nil {
		return err
	}
	
	fmt.Printf("Retried %d failed jobs\n", result.ModifiedCount)
	return nil
}

// ResetStuckJobs resets jobs that have been running for too long
func (q *MongoQueue) ResetStuckJobs(ctx context.Context, timeout time.Duration) error {
	cutoffTime := time.Now().Add(-timeout)
	
	filter := bson.M{
		"status": JobStatusRunning,
		"started_at": bson.M{"$lt": cutoffTime},
	}
	
	update := bson.M{
		"$set": bson.M{
			"status":     JobStatusPending,
			"worker_id":  "",
			"started_at": nil,
			"updated_at": time.Now(),
		},
	}
	
	result, err := q.collection.UpdateMany(ctx, filter, update)
	if err != nil {
		return err
	}
	
	fmt.Printf("Reset %d stuck jobs\n", result.ModifiedCount)
	return nil
}