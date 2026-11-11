package wal

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// Service manages WAL operations
type Service struct {
	db         *mongo.Database
	collection *mongo.Collection
	sequencer  *Sequencer
	metrics    *Metrics
	compressor *Compressor
}

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

	// Only migrate incompatible legacy definitions. Dropping the current
	// project/LSN index on every connection races another process's index
	// creation (e.g. a CLI command starting alongside `argon watch`).
	specs, err := s.collection.Indexes().ListSpecifications(ctx)
	if err != nil {
		_ = compressor.Close()
		return nil, fmt.Errorf("inspect WAL indexes: %w", err)
	}
	for _, spec := range specs {
		legacy := spec.Name == "lsn_1"
		if spec.Name == "project_id_1_lsn_1" {
			legacy = spec.Unique == nil || !*spec.Unique
		}
		if legacy {
			if _, err := s.collection.Indexes().DropOne(ctx, spec.Name); err != nil {
				_ = compressor.Close()
				return nil, fmt.Errorf("migrate WAL index %s: %w", spec.Name, err)
			}
		}
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
	return s.AppendContext(context.Background(), entry)
}

// AppendContext participates in the caller's MongoDB session/transaction.
func (s *Service) AppendContext(ctx context.Context, entry *Entry) (int64, error) {
	if err := entry.NormalizeDocumentKey(); err != nil {
		return 0, err
	}
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
	stored := *entry
	if err := s.compressor.CompressEntry(&stored); err != nil {
		return 0, fmt.Errorf("failed to compress WAL entry: %w", err)
	}

	if _, err := s.collection.InsertOne(ctx, &stored); err != nil {
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
	return s.AppendBatchContext(context.Background(), entries)
}

// AppendBatchContext participates in the caller's MongoDB transaction.
func (s *Service) AppendBatchContext(ctx context.Context, entries []*Entry) ([]int64, error) {
	if len(entries) == 0 {
		return []int64{}, nil
	}

	projectID := entries[0].ProjectID
	for i, entry := range entries {
		if err := entry.NormalizeDocumentKey(); err != nil {
			return nil, err
		}
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
		stored := *entry
		if err := s.compressor.CompressEntry(&stored); err != nil {
			return nil, fmt.Errorf("failed to compress WAL entry %d: %w", i, err)
		}

		documents[i] = &stored
	}

	if _, err := s.collection.InsertMany(ctx, documents, options.InsertMany().SetOrdered(true)); err != nil {
		// Any unwritten reserved LSNs become gaps, which are harmless.
		return nil, fmt.Errorf("failed to append WAL entries batch: %w", err)
	}

	return lsns, nil
}

// WithTransaction runs a retryable, atomic metadata/data mutation against
// the same MongoDB deployment as the WAL.
func (s *Service) WithTransaction(ctx context.Context, fn func(mongo.SessionContext) (interface{}, error)) (interface{}, error) {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(context.Background())
	return session.WithTransaction(ctx, fn, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
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
	id, err := DocumentIDValue(documentID)
	if err != nil {
		return nil, err
	}
	legacyKey := LegacyDocumentIDString(id)
	filter := bson.M{
		"branch_id":   branchID,
		"collection":  collection,
		"document_id": bson.M{"$in": []string{documentID, legacyKey}},
		"lsn": bson.M{
			"$gte": startLSN,
			"$lte": endLSN,
		},
	}

	opts := options.Find().SetSort(bson.M{"lsn": 1})
	entries, err := s.GetEntries(filter, opts)
	if err != nil {
		return nil, err
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if len(entry.PostImage) > 0 || len(entry.PreImage) > 0 {
			if err := entry.NormalizeDocumentKey(); err != nil {
				return nil, err
			}
			if entry.DocumentID != documentID {
				continue
			}
		} else if entry.Operation == OpDelete && entry.DocumentKeyVersion == 0 && DocumentIDString(entry.DocumentID) != entry.DocumentID {
			return nil, fmt.Errorf("legacy delete at LSN %d has an ambiguous BSON ID and no pre-image; full collection replay is required", entry.LSN)
		}
		filtered = append(filtered, entry)
	}
	return filtered, nil
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

// LatestLSNBefore returns the highest LSN of the branch's entries whose
// timestamp is at or before the given time, or 0 if none. GC uses this to
// translate the retention window into an LSN cutoff.
func (s *Service) LatestLSNBefore(branchID string, cutoff time.Time) (int64, error) {
	ctx := context.Background()
	var entry struct {
		LSN int64 `bson:"lsn"`
	}
	err := s.collection.FindOne(ctx,
		bson.M{"branch_id": branchID, "timestamp": bson.M{"$lte": cutoff}},
		options.FindOne().SetSort(bson.M{"lsn": -1}).SetProjection(bson.M{"lsn": 1}),
	).Decode(&entry)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, err
	}
	return entry.LSN, nil
}

// DataEntrySpan summarizes a branch's data entries up to maxLSN: how many
// there are and the LSN and timestamp of the first and last. It reads two
// index-ordered entries and a count instead of the entries themselves.
type DataEntrySpan struct {
	Count                    int64
	EarliestLSN, LatestLSN   int64
	EarliestTime, LatestTime time.Time
}

// DataEntrySpan returns the span of a branch's data entries at or below
// maxLSN. Count is zero when there are none.
func (s *Service) DataEntrySpan(branchID string, maxLSN int64) (*DataEntrySpan, error) {
	ctx := context.Background()
	filter := bson.M{
		"branch_id": branchID,
		"lsn":       bson.M{"$lte": maxLSN},
		"operation": bson.M{"$in": []OperationType{OpPut, OpDelete, LegacyOpInsert, LegacyOpUpdate}},
	}
	span := &DataEntrySpan{}
	var err error
	if span.Count, err = s.collection.CountDocuments(ctx, filter); err != nil {
		return nil, err
	}
	if span.Count == 0 {
		return span, nil
	}
	edge := func(order int) (int64, time.Time, error) {
		var entry struct {
			LSN       int64     `bson:"lsn"`
			Timestamp time.Time `bson:"timestamp"`
		}
		err := s.collection.FindOne(ctx, filter,
			options.FindOne().SetSort(bson.D{{Key: "lsn", Value: order}}).SetProjection(bson.M{"lsn": 1, "timestamp": 1}),
		).Decode(&entry)
		return entry.LSN, entry.Timestamp, err
	}
	if span.EarliestLSN, span.EarliestTime, err = edge(1); err != nil {
		return nil, err
	}
	if span.LatestLSN, span.LatestTime, err = edge(-1); err != nil {
		return nil, err
	}
	return span, nil
}

// FirstLSNAfter returns the lowest LSN among the branch's entries stamped
// after the given time, or 0 if none is. Everything below it is older than
// that time. GC bounds reclamation with this rather than with the newest old
// entry alone: timestamps are taken after an LSN is reserved, so concurrent
// writers can leave a slightly newer entry below an older one.
func (s *Service) FirstLSNAfter(branchID string, cutoff time.Time) (int64, error) {
	ctx := context.Background()
	var entry struct {
		LSN int64 `bson:"lsn"`
	}
	err := s.collection.FindOne(ctx,
		bson.M{"branch_id": branchID, "timestamp": bson.M{"$gt": cutoff}},
		options.FindOne().SetSort(bson.M{"lsn": 1}).SetProjection(bson.M{"lsn": 1}),
	).Decode(&entry)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, err
	}
	return entry.LSN, nil
}

// LatestLSNAtOrBefore returns the highest LSN, no greater than maxLSN, among
// a branch's entries stamped at or before the given time, or 0 if none is.
// It reads one index entry instead of the entries themselves.
func (s *Service) LatestLSNAtOrBefore(projectID, branchID string, at time.Time, maxLSN int64) (int64, error) {
	ctx := context.Background()
	var entry struct {
		LSN int64 `bson:"lsn"`
	}
	// project_id lets the (project_id, lsn) index supply the order, so the
	// scan runs backwards from maxLSN and stops at the first match.
	err := s.collection.FindOne(ctx,
		bson.M{
			"project_id": projectID,
			"branch_id":  branchID,
			"timestamp":  bson.M{"$lte": at},
			"lsn":        bson.M{"$lte": maxLSN},
		},
		options.FindOne().SetSort(bson.D{{Key: "lsn", Value: -1}}).SetProjection(bson.M{"lsn": 1}),
	).Decode(&entry)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, err
	}
	return entry.LSN, nil
}

// DeleteDataEntriesUpTo removes a collection's data entries (puts and
// deletes) on one branch with LSN at or below cutoff. Control entries are
// kept — they are tiny and carry branch/project lineage. Callers are
// responsible for the coverage argument (see the gc package).
func (s *Service) DeleteDataEntriesUpTo(branchID, collection string, cutoff int64) (int64, error) {
	ctx := context.Background()
	res, err := s.collection.DeleteMany(ctx, bson.M{
		"branch_id":  branchID,
		"collection": collection,
		"operation":  bson.M{"$in": []OperationType{OpPut, OpDelete}},
		"lsn":        bson.M{"$lte": cutoff},
	})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

// DeleteBranchEntries removes every entry belonging to a branch. Only safe
// once the branch is deleted and no live descendant can traverse it.
func (s *Service) DeleteBranchEntries(branchID string) (int64, error) {
	ctx := context.Background()
	res, err := s.collection.DeleteMany(ctx, bson.M{"branch_id": branchID})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
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
