package streams

import (
	"context"
	"fmt"
	"log"
	"time"

	"argon/engine/internal/branch"
	"argon/engine/internal/storage"
	"argon/engine/internal/workers"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Service struct {
	client     *mongo.Client
	db         *mongo.Database
	storage    storage.Service
	workerPool workers.WorkerPool
	
	// Change batching
	changeBatch     []workers.ChangeEventPayload
	batchSize       int
	batchTimeout    time.Duration
	lastBatchTime   time.Time
	
	// Branch tracking
	activeBranches  map[string]string // collection -> branch_id mapping
}

func NewService(client *mongo.Client, storage storage.Service, workerPool workers.WorkerPool) *Service {
	return &Service{
		client:         client,
		db:             client.Database("argon"),
		storage:        storage,
		workerPool:     workerPool,
		batchSize:      100,                 // Default batch size
		batchTimeout:   5 * time.Second,     // Flush batch every 5 seconds
		lastBatchTime:  time.Now(),
		activeBranches: make(map[string]string),
	}
}

// Start begins the change streams processor
func (s *Service) Start(ctx context.Context) error {
	log.Println("Starting change streams processor...")
	
	// Watch for changes on all collections except our metadata collections
	pipeline := []bson.M{
		{
			"$match": bson.M{
				"ns.coll": bson.M{
					"$nin": []string{"branches", "change_events", "users", "projects"},
				},
			},
		},
	}
	
	// Start change stream
	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup)
	changeStream, err := s.client.Watch(ctx, pipeline, opts)
	if err != nil {
		return fmt.Errorf("failed to start change stream: %w", err)
	}
	defer changeStream.Close(ctx)
	
	log.Println("Change streams processor started successfully")
	
	// Process changes with batching and timeout
	ticker := time.NewTicker(1 * time.Second) // Check for batch timeout every second
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			// Flush any remaining changes before stopping
			if len(s.changeBatch) > 0 {
				s.flushChangeBatch(ctx)
			}
			return ctx.Err()
			
		case <-ticker.C:
			// Check if batch timeout reached
			if len(s.changeBatch) > 0 && time.Since(s.lastBatchTime) >= s.batchTimeout {
				s.flushChangeBatch(ctx)
			}
			
		default:
			// Try to get next change (non-blocking)
			if changeStream.TryNext(ctx) {
				var change bson.M
				if err := changeStream.Decode(&change); err != nil {
					log.Printf("Failed to decode change: %v", err)
					continue
				}
				
				if err := s.processChange(ctx, change); err != nil {
					log.Printf("Failed to process change: %v", err)
				}
				
				// Check if batch is full
				if len(s.changeBatch) >= s.batchSize {
					s.flushChangeBatch(ctx)
				}
			}
		}
	}
	
	if err := changeStream.Err(); err != nil {
		return fmt.Errorf("change stream error: %w", err)
	}
	
	return nil
}

// GetChanges retrieves recent changes for a branch
func (s *Service) GetChanges(ctx context.Context, branchID primitive.ObjectID, limit int) ([]branch.ChangeEvent, error) {
	collection := s.db.Collection("change_events")
	
	opts := options.Find().
		SetLimit(int64(limit)).
		SetSort(bson.D{{Key: "timestamp", Value: -1}})
	
	cursor, err := collection.Find(ctx, bson.M{"branch_id": branchID}, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get changes: %w", err)
	}
	defer cursor.Close(ctx)
	
	var changes []branch.ChangeEvent
	for cursor.Next(ctx) {
		var change branch.ChangeEvent
		if err := cursor.Decode(&change); err != nil {
			return nil, fmt.Errorf("failed to decode change: %w", err)
		}
		changes = append(changes, change)
	}
	
	return changes, nil
}

// processChange processes a single change event by adding it to the batch
func (s *Service) processChange(ctx context.Context, change bson.M) error {
	// Extract change information
	operationType, ok := change["operationType"].(string)
	if !ok {
		return fmt.Errorf("missing operation type")
	}
	
	ns, ok := change["ns"].(bson.M)
	if !ok {
		return fmt.Errorf("missing namespace")
	}
	
	collection, ok := ns["coll"].(string)
	if !ok {
		return fmt.Errorf("missing collection name")
	}
	
	// Extract document ID
	var documentID interface{}
	if doc, ok := change["documentKey"].(bson.M); ok {
		documentID = doc["_id"]
	}
	
	// Extract full document (for inserts and updates)
	var fullDocument map[string]interface{}
	if fullDoc, ok := change["fullDocument"].(bson.M); ok {
		fullDocument = fullDoc
	}
	
	// Create change event payload
	changeEvent := workers.ChangeEventPayload{
		OperationType: operationType,
		Collection:    collection,
		DocumentID:    documentID,
		FullDocument:  fullDocument,
		Timestamp:     time.Now(),
	}
	
	// Add to batch
	s.changeBatch = append(s.changeBatch, changeEvent)
	
	// Update batch time if this is the first change in the batch
	if len(s.changeBatch) == 1 {
		s.lastBatchTime = time.Now()
	}
	
	return nil
}

// flushChangeBatch sends the current batch of changes to workers
func (s *Service) flushChangeBatch(ctx context.Context) error {
	if len(s.changeBatch) == 0 {
		return nil
	}
	
	// Determine which branch this batch belongs to
	// For now, assume all changes in a batch belong to the main branch
	// In a real implementation, this would be more sophisticated
	branchID, projectID := s.determineBranchForChanges(s.changeBatch)
	
	if branchID == "" || projectID == "" {
		log.Printf("Skipping batch of %d changes - unable to determine branch", len(s.changeBatch))
		s.changeBatch = nil
		return nil
	}
	
	// Create sync job
	job := &workers.Job{
		Type:     workers.JobTypeSync,
		Priority: workers.JobPriorityNormal,
		Payload: map[string]interface{}{
			"branch_id":  branchID,
			"project_id": projectID,
			"changes":    s.changeBatch,
			"batch_size": len(s.changeBatch),
		},
		MaxRetries: 3,
		RetryDelay: 30,
	}
	
	// Submit job to worker pool
	if err := s.workerPool.SubmitJob(ctx, job); err != nil {
		log.Printf("Failed to submit sync job: %v", err)
		return err
	}
	
	log.Printf("Submitted sync job with %d changes for branch %s", len(s.changeBatch), branchID)
	
	// Clear the batch
	s.changeBatch = nil
	s.lastBatchTime = time.Now()
	
	return nil
}

// determineBranchForChanges determines which branch the changes belong to
func (s *Service) determineBranchForChanges(changes []workers.ChangeEventPayload) (branchID, projectID string) {
	// For now, return a default main branch
	// In a real implementation, this would:
	// 1. Check if there's an active branch session for this connection
	// 2. Look up branch mapping based on collection patterns
	// 3. Default to main branch for the project
	
	// TODO: Implement proper branch determination logic
	// For demo purposes, use a default project and main branch
	return "66a8f1b4c9d5e7f8a9b0c1d2", "66a8f1b4c9d5e7f8a9b0c1d1" // Example ObjectIDs
}

// SetBatchConfiguration configures batching behavior
func (s *Service) SetBatchConfiguration(batchSize int, batchTimeout time.Duration) {
	if batchSize > 0 {
		s.batchSize = batchSize
	}
	if batchTimeout > 0 {
		s.batchTimeout = batchTimeout
	}
}

// RegisterBranchMapping registers a collection-to-branch mapping
func (s *Service) RegisterBranchMapping(collection, branchID string) {
	s.activeBranches[collection] = branchID
}

// WatchBranch sets up real-time change watching for a specific branch
func (s *Service) WatchBranch(ctx context.Context, branchID primitive.ObjectID) (<-chan branch.ChangeEvent, error) {
	// TODO: Implement WebSocket-based real-time change streaming
	// For now, return an empty channel
	changes := make(chan branch.ChangeEvent)
	close(changes)
	return changes, nil
}