package wal

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service manages WAL operations
type Service struct {
	db         *mongo.Database
	collection *mongo.Collection
	lsnCounter atomic.Int64
	metrics    *Metrics
}

// NewService creates a new WAL service
func NewService(db *mongo.Database) (*Service, error) {
	s := &Service{
		db:         db,
		collection: db.Collection("wal_log"),
		metrics:    GlobalMetrics,
	}

	// Create indexes
	ctx := context.Background()
	indexes := []mongo.IndexModel{
		{
			Keys:    bson.M{"lsn": 1},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys: bson.D{
				{Key: "project_id", Value: 1},
				{Key: "lsn", Value: 1},
			},
		},
		{
			Keys: bson.D{
				{Key: "branch_id", Value: 1},
				{Key: "collection", Value: 1},
				{Key: "lsn", Value: 1},
			},
		},
		{
			Keys: bson.M{"timestamp": 1},
		},
	}

	_, err := s.collection.Indexes().CreateMany(ctx, indexes)
	if err != nil {
		return nil, fmt.Errorf("failed to create indexes: %w", err)
	}

	// Initialize LSN counter
	if err := s.initializeLSN(); err != nil {
		return nil, fmt.Errorf("failed to initialize LSN: %w", err)
	}

	return s, nil
}

// initializeLSN sets the LSN counter to the highest existing LSN
func (s *Service) initializeLSN() error {
	ctx := context.Background()
	opts := options.FindOne().SetSort(bson.M{"lsn": -1})

	var lastEntry Entry
	err := s.collection.FindOne(ctx, bson.M{}, opts).Decode(&lastEntry)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// No entries yet, start at 0
			s.lsnCounter.Store(0)
			return nil
		}
		return err
	}

	s.lsnCounter.Store(lastEntry.LSN)
	return nil
}

// Append adds a new entry to the WAL
func (s *Service) Append(entry *Entry) (int64, error) {
	// Generate LSN atomically
	lsn := s.lsnCounter.Add(1)
	entry.LSN = lsn
	entry.Timestamp = time.Now()

	ctx := context.Background()
	_, err := s.collection.InsertOne(ctx, entry)
	if err != nil {
		// Rollback LSN on error
		s.lsnCounter.Add(-1)
		return 0, fmt.Errorf("failed to append WAL entry: %w", err)
	}

	return lsn, nil
}

// GetEntry retrieves a single WAL entry by LSN
func (s *Service) GetEntry(lsn int64) (*Entry, error) {
	ctx := context.Background()
	var entry Entry
	err := s.collection.FindOne(ctx, bson.M{"lsn": lsn}).Decode(&entry)
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// GetEntries retrieves WAL entries within an LSN range
func (s *Service) GetEntries(filter bson.M, opts ...*options.FindOptions) ([]*Entry, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var entries []*Entry
	if err := cursor.All(ctx, &entries); err != nil {
		return nil, err
	}

	return entries, nil
}

// GetBranchEntries retrieves all entries for a specific branch and collection
func (s *Service) GetBranchEntries(branchID, collection string, startLSN, endLSN int64) ([]*Entry, error) {
	filter := bson.M{
		"branch_id": branchID,
		"lsn": bson.M{
			"$gte": startLSN,
			"$lte": endLSN,
		},
	}

	if collection != "" {
		filter["collection"] = collection
	}

	opts := options.Find().SetSort(bson.M{"lsn": 1})
	return s.GetEntries(filter, opts)
}

// GetEntriesByTimestamp retrieves entries up to a specific timestamp
func (s *Service) GetEntriesByTimestamp(projectID string, timestamp time.Time) ([]*Entry, error) {
	filter := bson.M{
		"project_id": projectID,
		"timestamp":  bson.M{"$lte": timestamp},
	}

	opts := options.Find().SetSort(bson.M{"lsn": 1})
	return s.GetEntries(filter, opts)
}

// GetCurrentLSN returns the current LSN value
func (s *Service) GetCurrentLSN() int64 {
	return s.lsnCounter.Load()
}

// GetDocumentHistory retrieves WAL entries for a specific document
func (s *Service) GetDocumentHistory(branchID, collection, documentID string, startLSN, endLSN int64) ([]*Entry, error) {
	filter := bson.M{
		"branch_id": branchID,
		"collection": collection,
		"document_id": documentID,
		"lsn": bson.M{
			"$gte": startLSN,
			"$lte": endLSN,
		},
	}

	opts := options.Find().SetSort(bson.M{"lsn": 1})
	return s.GetEntries(filter, opts)
}

// GetProjectEntries retrieves all entries for a project within an LSN range
func (s *Service) GetProjectEntries(projectID, collection string, startLSN, endLSN int64) ([]*Entry, error) {
	filter := bson.M{
		"project_id": projectID,
		"lsn": bson.M{
			"$gte": startLSN,
			"$lte": endLSN,
		},
	}

	if collection != "" {
		filter["collection"] = collection
	}

	opts := options.Find().SetSort(bson.M{"lsn": 1})
	return s.GetEntries(filter, opts)
}

// GetMetrics returns a snapshot of current metrics
func (s *Service) GetMetrics() MetricsSnapshot {
	return s.metrics.GetSnapshot()
}

// GetSuccessRates returns success rates for operations
func (s *Service) GetSuccessRates() map[string]float64 {
	return s.metrics.GetSuccessRate()
}