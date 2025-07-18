package streams

import (
	"context"
	"fmt"
	"log"

	"argon/engine/internal/branch"
	"argon/engine/internal/storage"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Service struct {
	client  *mongo.Client
	db      *mongo.Database
	storage storage.Service
}

func NewService(client *mongo.Client, storage storage.Service) *Service {
	return &Service{
		client:  client,
		db:      client.Database("argon"),
		storage: storage,
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
	
	// Process changes
	for changeStream.Next(ctx) {
		var change bson.M
		if err := changeStream.Decode(&change); err != nil {
			log.Printf("Failed to decode change: %v", err)
			continue
		}
		
		if err := s.processChange(ctx, change); err != nil {
			log.Printf("Failed to process change: %v", err)
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

// processChange processes a single change event
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
	
	// For now, create a placeholder change event
	// In a full implementation, this would:
	// 1. Determine which branch(es) this change affects
	// 2. Compress and store the change
	// 3. Update branch metadata
	// 4. Notify subscribers via WebSocket
	
	log.Printf("Processing change: %s on collection %s", operationType, collection)
	
	// TODO: Implement full change processing logic
	// This is a placeholder that demonstrates the structure
	
	return nil
}

// WatchBranch sets up real-time change watching for a specific branch
func (s *Service) WatchBranch(ctx context.Context, branchID primitive.ObjectID) (<-chan branch.ChangeEvent, error) {
	// TODO: Implement WebSocket-based real-time change streaming
	// For now, return an empty channel
	changes := make(chan branch.ChangeEvent)
	close(changes)
	return changes, nil
}