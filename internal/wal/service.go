package wal

import (
	"context"
	"errors"
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
	entries, _, err := s.DocumentHistoryForRead(branchID, collection, documentID, startLSN, endLSN, nil)
	return entries, err
}

// DocumentHistoryForRead returns history in LSN order. The reader applies
// its visibility rules when choosing an entry. Only an ambiguous delete
// visible to that reader can prevent the lookup. The boolean identifies
// a requested key in the historical collision class.
func (s *Service) DocumentHistoryForRead(branchID, collection, documentID string, startLSN, endLSN int64, visible func(int64) bool) ([]*Entry, bool, error) {
	entries, affected, err := s.documentHistoryCandidates(branchID, collection, documentID, startLSN, endLSN, visible)
	if err != nil {
		return nil, affected, err
	}
	filtered := entries[:0]
	for _, entry := range entries {
		named, err := settleDocumentKey(entry)
		if err != nil {
			return nil, affected, err
		}
		// An entry that names its document counts only for that document.
		// A legacy delete without an image names none: the query matched it
		// by this document's current or legacy key, and it is kept, as a
		// replay removes whichever document that key describes.
		if named && entry.DocumentID != documentID {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered, affected, nil
}

// LatestDocumentImageEntry uses the same per-key history query as
// GetDocumentHistory. Once a visible old-binary ambiguity is ruled out,
// the newest visible image identifies the document.
func (s *Service) LatestDocumentImageEntry(branchID, collection, documentID string, startLSN, endLSN int64, visible func(int64) bool) (*Entry, error) {
	entries, _, err := s.documentHistoryCandidates(branchID, collection, documentID, startLSN, endLSN, visible)
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !entry.IsData() || (visible != nil && !visible(entry.LSN)) {
			continue
		}
		storedVersion := entry.DocumentKeyVersion
		named, err := settleDocumentKey(entry)
		if errors.Is(err, ErrLegacyDocumentKeys) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if named && entry.DocumentID != documentID {
			continue
		}
		if storedVersion < 1 {
			return nil, nil
		}
		return entry, nil
	}
	return nil, nil
}

func (s *Service) documentHistoryCandidates(branchID, collection, documentID string, startLSN, endLSN int64, visible func(int64) bool) ([]*Entry, bool, error) {
	id, err := DocumentIDValue(documentID)
	if err != nil {
		return nil, false, err
	}
	candidates := []string{documentID, LegacyDocumentIDString(id)}
	alias, affected := oldBinaryIndexKey(documentID)
	if affected && alias != documentID {
		candidates = append(candidates, alias)
	}
	filter := bson.M{
		"branch_id":   branchID,
		"collection":  collection,
		"document_id": bson.M{"$in": candidates},
		"lsn": bson.M{
			"$gte": startLSN,
			"$lte": endLSN,
		},
	}

	opts := options.Find().SetSort(bson.M{"lsn": 1})
	entries, err := s.GetEntries(filter, opts)
	if err == nil && affected {
		for _, e := range entries {
			if e.Operation == OpDelete && len(e.PreImage) == 0 && e.DocumentID == alias && (visible == nil || visible(e.LSN)) {
				if err := e.CheckStoredKey(); err != nil {
					return nil, affected, err
				}
			}
		}
	}
	return entries, affected, err
}

// settleDocumentKey works out which document a stored entry is about. Entries
// are found by their stored key, and history written before typed keys (up
// to 2.0.x) stored one that different BSON IDs can share — an ObjectID and
// the string of its hex form, for example — so a lookup by stored key can
// return another document's entries.
//
// named is true when the entry itself says: an image carries the real _id
// (DocumentID is set from it), and a delete written with a current key is
// trusted as it stands. Anything else names no document and is left to the
// caller, which matched it by a stored key — except a legacy delete whose
// key reads as more than one ID, which is refused.
func settleDocumentKey(entry *Entry) (named bool, err error) {
	if len(entry.PostImage) > 0 || len(entry.PreImage) > 0 {
		image := entry.PostImage
		if entry.Operation == OpDelete {
			image = entry.PreImage
		}
		if id, err := DocumentIDFromImage(image); err == nil {
			entry.DocumentID = DocumentIDString(id)
		} else {
			if ambiguous := entry.CheckStoredKey(); ambiguous != nil {
				return false, errors.Join(ambiguous, err)
			}
			if image.Validate() != nil {
				return false, err
			}
		}
		entry.DocumentKeyVersion = max(1, entry.DocumentKeyVersion)
		return true, nil
	}
	if entry.Operation != OpDelete {
		return false, nil
	}
	if entry.DocumentKeyVersion == 0 {
		if DocumentIDString(entry.DocumentID) != entry.DocumentID {
			return false, fmt.Errorf("legacy delete at LSN %d has an ambiguous BSON ID and no pre-image; full collection replay is required: %w", entry.LSN, ErrLegacyDocumentKeys)
		}
		return false, nil
	}
	return true, nil
}

// ErrLegacyDocumentKeys reports history written before typed document keys
// (releases up to 2.0.x, and entries rewritten by the schema-v1 migration).
// Its stored keys take several forms, some shared between different _id
// values and some unrelated to the current key, so it cannot be read
// reliably one document at a time.
var ErrLegacyDocumentKeys = errors.New("history predates typed document keys; replay the collection")

var dataOperations = []OperationType{OpPut, OpDelete, LegacyOpInsert, LegacyOpUpdate}

// HasLegacyKeyedEntries reports whether the branch's log may hold, for one
// collection in [startLSN, endLSN], data entries written before typed
// document keys. Entries are appended in time order and every current
// release writes typed keys, so the oldest data entry in the range decides:
// if it has a typed key, so do all later ones. One indexed read.
func (s *Service) HasLegacyKeyedEntries(branchID, collection string, startLSN, endLSN int64) (bool, error) {
	if startLSN > endLSN {
		return false, nil
	}
	var oldest struct {
		KeyVersion int `bson:"key_v"`
	}
	err := s.collection.FindOne(context.Background(), bson.M{
		"branch_id":  branchID,
		"collection": collection,
		"lsn":        bson.M{"$gte": startLSN, "$lte": endLSN},
		"operation":  bson.M{"$in": dataOperations},
	}, options.FindOne().SetSort(bson.D{{Key: "lsn", Value: 1}}).SetProjection(bson.M{"key_v": 1})).Decode(&oldest)
	if err == mongo.ErrNoDocuments {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return oldest.KeyVersion < 1, nil
}

// HasUnkeyedDataEntries reports whether the branch's log holds, for one
// collection in [startLSN, endLSN], an entry without a document key. Current
// entries always carry one; schema-v1 updates and deletes carried a filter
// instead. Lookups by document key cannot see such entries, so callers that
// read documents one by one check for them first. Served by the (branch,
// collection, document, lsn) index.
func (s *Service) HasUnkeyedDataEntries(branchID, collection string, startLSN, endLSN int64) (bool, error) {
	if startLSN > endLSN {
		return false, nil
	}
	err := s.collection.FindOne(context.Background(), bson.M{
		"branch_id":   branchID,
		"collection":  collection,
		"document_id": bson.M{"$in": bson.A{nil, ""}},
		"lsn":         bson.M{"$gte": startLSN, "$lte": endLSN},
		"operation":   bson.M{"$in": dataOperations},
	}, options.FindOne().SetProjection(bson.M{"_id": 1})).Err()
	if err == mongo.ErrNoDocuments {
		return false, nil
	}
	return err == nil, err
}

// documentLookupBatch bounds how many documents one history query names.
const documentLookupBatch = 500

// LatestDocumentEntries returns, for each of the given document keys, the
// branch's newest data entry in [startLSN, endLSN] that the caller can see,
// keyed by document key. A document without such an entry is absent from the
// result. visible reports whether an LSN counts for this reader (nil means
// every LSN does); it is how discarded ranges are skipped.
//
// It reads in two steps so that the cost follows the number of documents
// asked for, not the size of the collection or how often a document was
// rewritten: the positions of the documents' entries come from the (branch,
// collection, document, lsn) index alone, and only the newest visible entry
// of each document is then fetched.
//
// The range must hold typed document keys only; otherwise it fails with
// ErrLegacyDocumentKeys (see HasLegacyKeyedEntries).
func (s *Service) LatestDocumentEntries(branchID, collection string, keys []string, startLSN, endLSN int64, visible func(lsn int64) bool) (map[string]*Entry, error) {
	latest := make(map[string]*Entry)
	if startLSN > endLSN || len(keys) == 0 {
		return latest, nil
	}
	legacy, err := s.HasLegacyKeyedEntries(branchID, collection, startLSN, endLSN)
	if err != nil {
		return nil, err
	}
	if legacy {
		return nil, fmt.Errorf("collection %q: %w", collection, ErrLegacyDocumentKeys)
	}

	// A historical index entry with four zero bytes may contain rows for
	// two BSON IDs. Resolve these keys from images before taking the latest
	// visible row. Other keys keep the indexed positions-only lookup.
	ordinary := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, affected := oldBinaryIndexKey(key); !affected {
			ordinary = append(ordinary, key)
			continue
		}
		entry, err := s.LatestDocumentImageEntry(branchID, collection, key, startLSN, endLSN, visible)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			latest[key] = entry
		}
	}
	keys = ordinary
	ctx := context.Background()
	for start := 0; start < len(keys); start += documentLookupBatch {
		end := start + documentLookupBatch
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[start:end]

		// Step one: where each document's newest visible entry is.
		cursor, err := s.collection.Find(ctx, bson.M{
			"branch_id":   branchID,
			"collection":  collection,
			"document_id": bson.M{"$in": batch},
			"lsn":         bson.M{"$gte": startLSN, "$lte": endLSN},
		}, options.Find().SetProjection(bson.M{"_id": 0, "document_id": 1, "lsn": 1}))
		if err != nil {
			return nil, err
		}
		newest := make(map[string]int64, len(batch))
		err = func() error {
			defer func() { _ = cursor.Close(ctx) }()
			for cursor.Next(ctx) {
				var position struct {
					DocumentID string `bson:"document_id"`
					LSN        int64  `bson:"lsn"`
				}
				if err := cursor.Decode(&position); err != nil {
					return err
				}
				if visible != nil && !visible(position.LSN) {
					continue
				}
				if position.LSN > newest[position.DocumentID] {
					newest[position.DocumentID] = position.LSN
				}
			}
			return cursor.Err()
		}()
		if err != nil {
			return nil, err
		}
		if len(newest) == 0 {
			continue
		}

		// Step two: those entries. LSNs are unique within a project.
		lsns := make([]int64, 0, len(newest))
		for _, lsn := range newest {
			lsns = append(lsns, lsn)
		}
		entries, err := s.GetEntries(bson.M{
			"branch_id":  branchID,
			"collection": collection,
			"lsn":        bson.M{"$in": lsns},
		})
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsData() || entry.DocumentKeyVersion < 1 {
				return nil, fmt.Errorf("collection %q, LSN %d: %w", collection, entry.LSN, ErrLegacyDocumentKeys)
			}
			if newest[entry.DocumentID] == entry.LSN {
				latest[entry.DocumentID] = entry
			}
		}
	}
	return latest, nil
}

// CountBranchEntries counts the branch's entries in [startLSN, endLSN], and
// stops counting at limit.
func (s *Service) CountBranchEntries(branchID string, startLSN, endLSN, limit int64) (int64, error) {
	if startLSN > endLSN {
		return 0, nil
	}
	return s.collection.CountDocuments(context.Background(), bson.M{
		"branch_id": branchID,
		"lsn":       bson.M{"$gte": startLSN, "$lte": endLSN},
	}, options.Count().SetLimit(limit))
}

// ScanBranchDataEntries calls fn for every data entry of the branch's own
// log in [startLSN, endLSN], in no particular order, one entry at a time.
// It is for callers that fold over a branch's changes without holding them
// all in memory. An error from fn ends the scan and is returned.
func (s *Service) ScanBranchDataEntries(branchID string, startLSN, endLSN int64, fn func(*Entry) error) error {
	if startLSN > endLSN {
		return nil
	}
	return s.scanDataEntries(bson.M{
		"branch_id": branchID,
		"lsn":       bson.M{"$gte": startLSN, "$lte": endLSN},
	}, nil, fn)
}

// ScanCollectionDataEntries streams the LSN range in replay order, or newest
// first when recovering the last known image of each snapshot document.
func (s *Service) ScanCollectionDataEntries(branchID, collection string, start, end int64, newestFirst bool, fn func(*Entry) error) error {
	filter := bson.M{"branch_id": branchID, "lsn": bson.M{"$gte": start, "$lte": end}}
	if collection != "" {
		filter["collection"] = collection
	}
	order := 1
	if newestFirst {
		order = -1
	}
	return s.scanDataEntries(filter, options.Find().SetSort(bson.D{{Key: "lsn", Value: order}}), fn)
}

func (s *Service) scanDataEntries(filter bson.M, opts *options.FindOptions, fn func(*Entry) error) error {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, filter, opts)
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close(ctx) }()
	for cursor.Next(ctx) {
		entry := new(Entry)
		if err := cursor.Decode(entry); err != nil {
			return err
		}
		if !entry.IsData() {
			continue
		}
		if err := s.compressor.DecompressEntry(entry); err != nil {
			return fmt.Errorf("failed to decompress WAL entry LSN %d: %w", entry.LSN, err)
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
	return cursor.Err()
}

// ScanDocumentKeys calls fn once for every document key that the branch's own
// entries of a collection name in [startLSN, endLSN], in key order. It reads
// the (branch, collection, document, lsn) index alone and holds one key at a
// time, so the collection's size does not matter to the caller's memory. A
// key is reported whether its newest entry is a put or a delete. An older
// binary key indexed under a different ID requires a collection replay.
func (s *Service) ScanDocumentKeys(ctx context.Context, branchID, collection string, startLSN, endLSN int64, fn func(key string) error) error {
	if startLSN > endLSN {
		return nil
	}
	cursor, err := s.collection.Find(ctx, bson.M{
		"branch_id":  branchID,
		"collection": collection,
		"lsn":        bson.M{"$gte": startLSN, "$lte": endLSN},
	}, options.Find().
		SetProjection(bson.M{"_id": 0, "document_id": 1}).
		SetSort(bson.D{{Key: "branch_id", Value: 1}, {Key: "collection", Value: 1}, {Key: "document_id", Value: 1}, {Key: "lsn", Value: 1}}))
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close(context.Background()) }()
	last, first := "", true
	for cursor.Next(ctx) {
		key, ok := cursor.Current.Lookup("document_id").StringValueOK()
		if !ok || key == "" || (!first && key == last) {
			continue
		}
		last, first = key, false
		if alias, affected := oldBinaryIndexKey(key); affected && alias == key {
			// The index alone cannot show every key when older rows share an
			// envelope. A replay recovers them directly from their images.
			return ErrLegacyDocumentKeys
		}
		if err := fn(key); err != nil {
			return err
		}
	}
	return cursor.Err()
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
