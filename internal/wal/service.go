package wal

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service manages WAL operations
type Service struct {
	db         *mongo.Database
	collection *mongo.Collection
	sequencer  *Sequencer
	metrics    *Metrics
	compressor *Compressor
}

// legacyIndexNames are indexes from earlier releases whose keys or options
// conflict with the current definitions and must be dropped before creating
// the new ones. Creating an index whose name matches an existing one with
// different options fails, so these are removed up front.
var legacyIndexNames = []string{"lsn_1", "project_id_1_lsn_1"}

// NewService creates a new WAL service
func NewService(db *mongo.Database) (*Service, error) {
	// Initialize compressor with default config
	compressor, err := NewCompressor(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create compressor: %w", err)
	}

	s := &Service{
		db:         db,
		collection: db.Collection("wal_log"),
		sequencer:  NewSequencer(db),
		metrics:    GlobalMetrics,
		compressor: compressor,
	}

	ctx := context.Background()

	// Earlier releases enforced a globally unique lsn; LSNs are now scoped
	// per project, so the old definitions conflict and must be dropped.
	for _, name := range legacyIndexNames {
		_, _ = s.collection.Indexes().DropOne(ctx, name)
	}

	indexes := []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "project_id", Value: 1},
				{Key: "lsn", Value: 1},
			},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys: bson.D{
				{Key: "branch_id", Value: 1},
				{Key: "collection", Value: 1},
				{Key: "lsn", Value: 1},
			},
		},
		{
			// Point lookups of a single document's history are the hot
			// path for filter resolution on the write path.
			Keys: bson.D{
				{Key: "branch_id", Value: 1},
				{Key: "collection", Value: 1},
				{Key: "document_id", Value: 1},
				{Key: "lsn", Value: 1},
			},
		},
		{
			Keys: bson.M{"timestamp": 1},
		},
	}

	if _, err2 := s.collection.Indexes().CreateMany(ctx, indexes); err2 != nil {
		_ = compressor.Close()
		return nil, fmt.Errorf("failed to create indexes: %w", err2)
	}

	return s, nil
}

// Append adds a new entry to the WAL
func (s *Service) Append(entry *Entry) (int64, error) {
	if err := entry.ValidateForAppend(); err != nil {
		return 0, err
	}
	entry.SchemaVersion = EntrySchemaVersion

	lsn, err := s.sequencer.Reserve(entry.ProjectID, 1)
	if err != nil {
		return 0, err
	}
	entry.LSN = lsn
	entry.Timestamp = time.Now()

	// Compress entry before storing
	if err := s.compressor.CompressEntry(entry); err != nil {
		return 0, fmt.Errorf("failed to compress WAL entry: %w", err)
	}

	ctx := context.Background()
	if _, err := s.collection.InsertOne(ctx, entry); err != nil {
		// The reserved LSN becomes a gap in the sequence. Gaps are
		// harmless: consumers rely on ordering, never on density, so
		// reservations are never rolled back (a rollback under
		// concurrency could hand an already-used LSN to a later writer).
		return 0, fmt.Errorf("failed to append WAL entry: %w", err)
	}

	return lsn, nil
}

// AppendBatch adds multiple entries to the WAL in a single operation for
// optimal performance. All entries must belong to the same project because
// the batch is allocated one contiguous per-project LSN range.
func (s *Service) AppendBatch(entries []*Entry) ([]int64, error) {
	if len(entries) == 0 {
		return []int64{}, nil
	}

	projectID := entries[0].ProjectID
	for i, entry := range entries {
		if entry.ProjectID != projectID {
			return nil, fmt.Errorf("batch entry %d belongs to project %q, expected %q: batches must be single-project", i, entry.ProjectID, projectID)
		}
		if err := entry.ValidateForAppend(); err != nil {
			return nil, fmt.Errorf("batch entry %d is invalid: %w", i, err)
		}
		entry.SchemaVersion = EntrySchemaVersion
	}

	firstLSN, err := s.sequencer.Reserve(projectID, int64(len(entries)))
	if err != nil {
		return nil, err
	}

	now := time.Now()
	lsns := make([]int64, len(entries))
	documents := make([]interface{}, len(entries))

	for i, entry := range entries {
		entry.LSN = firstLSN + int64(i)
		entry.Timestamp = now
		lsns[i] = entry.LSN

		// Compress entry before storing
		if err := s.compressor.CompressEntry(entry); err != nil {
			return nil, fmt.Errorf("failed to compress WAL entry %d: %w", i, err)
		}

		documents[i] = entry
	}

	ctx := context.Background()
	if _, err := s.collection.InsertMany(ctx, documents, options.InsertMany().SetOrdered(true)); err != nil {
		// Any unwritten reserved LSNs become gaps, which are harmless.
		return nil, fmt.Errorf("failed to append WAL entries batch: %w", err)
	}

	return lsns, nil
}

// GetEntry retrieves a single WAL entry by project and LSN. LSNs are unique
// only within a project.
func (s *Service) GetEntry(projectID string, lsn int64) (*Entry, error) {
	ctx := context.Background()
	var entry Entry
	err := s.collection.FindOne(ctx, bson.M{"project_id": projectID, "lsn": lsn}).Decode(&entry)
	if err != nil {
		return nil, err
	}

	// Decompress entry after retrieval
	if err := s.compressor.DecompressEntry(&entry); err != nil {
		return nil, fmt.Errorf("failed to decompress WAL entry: %w", err)
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

	// Decompress all entries
	for _, entry := range entries {
		if err := s.compressor.DecompressEntry(entry); err != nil {
			return nil, fmt.Errorf("failed to decompress WAL entry LSN %d: %w", entry.LSN, err)
		}
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

// GetCurrentLSN returns the most recently allocated LSN for a project, or 0
// if the project has no entries yet. This is an upper bound on written LSNs:
// the latest reservation may still be in flight, or may have failed and left
// a gap.
func (s *Service) GetCurrentLSN(projectID string) int64 {
	lsn, err := s.sequencer.Current(projectID)
	if err != nil {
		return 0
	}
	return lsn
}

// GetDocumentHistory retrieves WAL entries for a specific document
func (s *Service) GetDocumentHistory(branchID, collection, documentID string, startLSN, endLSN int64) ([]*Entry, error) {
	filter := bson.M{
		"branch_id":   branchID,
		"collection":  collection,
		"document_id": documentID,
		"lsn": bson.M{
			"$gte": startLSN,
			"$lte": endLSN,
		},
	}

	opts := options.Find().SetSort(bson.M{"lsn": 1})
	return s.GetEntries(filter, opts)
}

// DistinctCollections returns the collections touched by a branch's own
// entries within an LSN range.
func (s *Service) DistinctCollections(branchID string, startLSN, endLSN int64) ([]string, error) {
	ctx := context.Background()
	values, err := s.collection.Distinct(ctx, "collection", bson.M{
		"branch_id":  branchID,
		"collection": bson.M{"$ne": ""},
		"lsn": bson.M{
			"$gte": startLSN,
			"$lte": endLSN,
		},
	})
	if err != nil {
		return nil, err
	}
	collections := make([]string, 0, len(values))
	for _, v := range values {
		if name, ok := v.(string); ok {
			collections = append(collections, name)
		}
	}
	return collections, nil
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

// Close cleans up resources used by the service
func (s *Service) Close() error {
	if s.compressor != nil {
		return s.compressor.Close()
	}
	return nil
}
