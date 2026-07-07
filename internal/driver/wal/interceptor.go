package wal

import (
	"context"
	"fmt"
	"sort"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Materializer is the state-reconstruction interface the driver needs.
type Materializer interface {
	MaterializeCollection(branch *wal.Branch, collection string) (map[string]bson.M, error)
	MaterializeDocument(branch *wal.Branch, collection, documentID string) (bson.M, error)
	MaterializeBranch(branch *wal.Branch) (map[string]map[string]bson.M, error)
}

// AutoSnapshotter is notified after writes so it can decide whether the
// branch has outgrown its newest snapshot. Implementations must be cheap
// and non-blocking; the snapshot service throttles internally.
type AutoSnapshotter interface {
	MaybeSnapshot(branch *wal.Branch)
}

// Interceptor turns MongoDB write operations into deterministic WAL entries.
//
// Filters and update operators are resolved here, exactly once, at write
// time: the interceptor materializes the affected documents, computes the
// post-images, and logs those. Replay never re-executes a filter or an
// update operator, which is what makes it deterministic.
//
// Concurrency note: resolve-then-append is not atomic, so two writers
// racing on the same branch resolve against the same state and the later
// append wins at document level (last-writer-wins). The WAL itself stays
// consistent because every entry is self-contained.
type Interceptor struct {
	wal          *wal.Service
	branch       *wal.Branch
	branches     *branchwal.BranchService
	materializer Materializer
	actor        string
	autoSnapshot AutoSnapshotter
}

// NewInterceptor creates a new WAL interceptor
func NewInterceptor(walService *wal.Service, branch *wal.Branch, branchService *branchwal.BranchService, materializer Materializer) *Interceptor {
	return &Interceptor{
		wal:          walService,
		branch:       branch,
		branches:     branchService,
		materializer: materializer,
	}
}

// SetActor tags subsequent writes with an actor identity (e.g. "user:jake",
// "agent:session-42") for audit and per-session undo.
func (i *Interceptor) SetActor(actor string) {
	i.actor = actor
}

// SetAutoSnapshotter enables threshold-based automatic snapshotting for
// this handle's branch.
func (i *Interceptor) SetAutoSnapshotter(a AutoSnapshotter) {
	i.autoSnapshot = a
}

// InsertResult represents the result of an insert operation
type InsertResult struct {
	InsertedID interface{}
}

// UpdateResult represents the result of an update operation
type UpdateResult struct {
	MatchedCount  int64
	ModifiedCount int64
	UpsertedCount int64
	UpsertedID    interface{}
}

// DeleteResult represents the result of a delete operation
type DeleteResult struct {
	DeletedCount int64
}

// match pairs a resolved document with its canonical ID.
type match struct {
	docID string
	doc   bson.M
}

// guardNotLive rejects SDK writes to checked-out branches: their WAL is
// fed from the physical database's change stream, and writing through both
// paths would double-log. Handles created before a checkout can be stale;
// recreate handles after checking a branch out.
func (i *Interceptor) guardNotLive() error {
	if i.branch.IsLive() {
		return fmt.Errorf("branch %s is checked out as %s: connect with a MongoDB driver instead of the SDK write path", i.branch.Name, i.branch.PhysicalDB)
	}
	return nil
}

// InsertOne intercepts an insert operation and writes a put entry.
func (i *Interceptor) InsertOne(ctx context.Context, collection string, document interface{}) (*InsertResult, error) {
	if err := i.guardNotLive(); err != nil {
		return nil, err
	}
	docID, doc, err := i.ensureDocumentID(document)
	if err != nil {
		return nil, err
	}
	docIDStr := wal.DocumentIDString(docID)

	existing, err := i.materializer.MaterializeDocument(i.branch, collection, docIDStr)
	if err != nil {
		return nil, fmt.Errorf("failed to check for existing document: %w", err)
	}
	if existing != nil {
		return nil, fmt.Errorf("duplicate key error: a document with _id %v already exists in %s", docID, collection)
	}

	entry, err := i.putEntry(collection, docIDStr, doc, nil)
	if err != nil {
		return nil, err
	}
	if err := i.append(entry); err != nil {
		return nil, err
	}

	return &InsertResult{InsertedID: docID}, nil
}

// InsertMany intercepts a bulk insert and writes one batched set of puts.
func (i *Interceptor) InsertMany(ctx context.Context, collection string, documents []interface{}) ([]interface{}, error) {
	if err := i.guardNotLive(); err != nil {
		return nil, err
	}
	if len(documents) == 0 {
		return nil, fmt.Errorf("insertMany requires at least one document")
	}

	insertedIDs := make([]interface{}, 0, len(documents))
	entries := make([]*wal.Entry, 0, len(documents))
	seen := make(map[string]bool, len(documents))

	for idx, document := range documents {
		docID, doc, err := i.ensureDocumentID(document)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", idx, err)
		}
		docIDStr := wal.DocumentIDString(docID)

		if seen[docIDStr] {
			return nil, fmt.Errorf("duplicate key error within batch: _id %v appears twice", docID)
		}
		seen[docIDStr] = true

		existing, err := i.materializer.MaterializeDocument(i.branch, collection, docIDStr)
		if err != nil {
			return nil, fmt.Errorf("failed to check for existing document: %w", err)
		}
		if existing != nil {
			return nil, fmt.Errorf("duplicate key error: a document with _id %v already exists in %s", docID, collection)
		}

		entry, err := i.putEntry(collection, docIDStr, doc, nil)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		insertedIDs = append(insertedIDs, docID)
	}

	if err := i.appendBatch(entries); err != nil {
		return nil, err
	}
	return insertedIDs, nil
}

// UpdateOne intercepts an update of at most one document.
func (i *Interceptor) UpdateOne(ctx context.Context, collection string, filter, update interface{}, upsert bool) (*UpdateResult, error) {
	return i.applyUpdate(collection, filter, update, upsert, true)
}

// UpdateMany intercepts an update of every matching document.
func (i *Interceptor) UpdateMany(ctx context.Context, collection string, filter, update interface{}, upsert bool) (*UpdateResult, error) {
	return i.applyUpdate(collection, filter, update, upsert, false)
}

// ReplaceOne intercepts a full-document replacement.
func (i *Interceptor) ReplaceOne(ctx context.Context, collection string, filter, replacement interface{}, upsert bool) (*UpdateResult, error) {
	repDoc, ok := toBSONM(replacement)
	if !ok {
		return nil, fmt.Errorf("replacement must be a document, got %T", replacement)
	}
	if hasUpdateOperators(repDoc) {
		return nil, fmt.Errorf("replacement documents must not contain update operators")
	}
	return i.applyUpdate(collection, filter, repDoc, upsert, true)
}

func (i *Interceptor) applyUpdate(collection string, filter, update interface{}, upsert, limitOne bool) (*UpdateResult, error) {
	if err := i.guardNotLive(); err != nil {
		return nil, err
	}
	filterDoc, err := normalizeFilter(filter)
	if err != nil {
		return nil, err
	}

	matches, err := i.resolveMatches(collection, filterDoc, limitOne)
	if err != nil {
		return nil, err
	}

	if len(matches) == 0 {
		if !upsert {
			return &UpdateResult{}, nil
		}
		upsertDoc, err := BuildUpsertDocument(filterDoc, update)
		if err != nil {
			return nil, err
		}
		upsertedID, newDoc, err := i.ensureDocumentID(upsertDoc)
		if err != nil {
			return nil, err
		}
		entry, err := i.putEntry(collection, wal.DocumentIDString(upsertedID), newDoc, nil)
		if err != nil {
			return nil, err
		}
		if err := i.append(entry); err != nil {
			return nil, err
		}
		return &UpdateResult{UpsertedCount: 1, UpsertedID: upsertedID}, nil
	}

	var entries []*wal.Entry
	var modified int64
	for _, m := range matches {
		newDoc, err := ApplyUpdate(m.doc, update, false)
		if err != nil {
			return nil, err
		}
		changed, err := docsDiffer(m.doc, newDoc)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue // Matched but unchanged: MongoDB writes nothing.
		}
		entry, err := i.putEntry(collection, m.docID, newDoc, m.doc)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		modified++
	}

	if err := i.appendBatch(entries); err != nil {
		return nil, err
	}
	return &UpdateResult{MatchedCount: int64(len(matches)), ModifiedCount: modified}, nil
}

// DeleteOne intercepts a delete of at most one document.
func (i *Interceptor) DeleteOne(ctx context.Context, collection string, filter interface{}) (*DeleteResult, error) {
	return i.applyDelete(collection, filter, true)
}

// DeleteMany intercepts a delete of every matching document.
func (i *Interceptor) DeleteMany(ctx context.Context, collection string, filter interface{}) (*DeleteResult, error) {
	return i.applyDelete(collection, filter, false)
}

func (i *Interceptor) applyDelete(collection string, filter interface{}, limitOne bool) (*DeleteResult, error) {
	if err := i.guardNotLive(); err != nil {
		return nil, err
	}
	filterDoc, err := normalizeFilter(filter)
	if err != nil {
		return nil, err
	}

	matches, err := i.resolveMatches(collection, filterDoc, limitOne)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return &DeleteResult{}, nil
	}

	entries := make([]*wal.Entry, 0, len(matches))
	for _, m := range matches {
		preImage, err := bson.Marshal(m.doc)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal pre-image: %w", err)
		}
		entries = append(entries, &wal.Entry{
			ProjectID:  i.branch.ProjectID,
			BranchID:   i.branch.ID,
			Operation:  wal.OpDelete,
			Collection: collection,
			DocumentID: m.docID,
			PreImage:   preImage,
			Actor:      i.actor,
		})
	}

	if err := i.appendBatch(entries); err != nil {
		return nil, err
	}
	return &DeleteResult{DeletedCount: int64(len(matches))}, nil
}

// FindMatches resolves a filter against current branch state, in
// deterministic (sorted by document ID) order. Used by the read path.
func (i *Interceptor) FindMatches(collection string, filter interface{}, limitOne bool) ([]bson.M, error) {
	filterDoc, err := normalizeFilter(filter)
	if err != nil {
		return nil, err
	}
	matches, err := i.resolveMatches(collection, filterDoc, limitOne)
	if err != nil {
		return nil, err
	}
	docs := make([]bson.M, 0, len(matches))
	for _, m := range matches {
		docs = append(docs, m.doc)
	}
	return docs, nil
}

// resolveMatches finds the documents a filter selects. Filters with an _id
// equality take a point-lookup fast path; everything else materializes the
// collection and scans it in sorted-ID order so that "first match"
// operations are deterministic.
func (i *Interceptor) resolveMatches(collection string, filterDoc bson.M, limitOne bool) ([]match, error) {
	if idValue, ok := extractIDEquality(filterDoc); ok {
		docID := wal.DocumentIDString(idValue)
		doc, err := i.materializer.MaterializeDocument(i.branch, collection, docID)
		if err != nil {
			return nil, err
		}
		if doc == nil {
			return nil, nil
		}
		matched, err := MatchesFilter(doc, filterDoc)
		if err != nil {
			return nil, err
		}
		if !matched {
			return nil, nil
		}
		return []match{{docID: docID, doc: doc}}, nil
	}

	state, err := i.materializer.MaterializeCollection(i.branch, collection)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize collection: %w", err)
	}

	docIDs := make([]string, 0, len(state))
	for docID := range state {
		docIDs = append(docIDs, docID)
	}
	sort.Strings(docIDs)

	var matches []match
	for _, docID := range docIDs {
		matched, err := MatchesFilter(state[docID], filterDoc)
		if err != nil {
			return nil, err
		}
		if matched {
			matches = append(matches, match{docID: docID, doc: state[docID]})
			if limitOne {
				break
			}
		}
	}
	return matches, nil
}

// putEntry builds a put entry with an optional pre-image.
func (i *Interceptor) putEntry(collection, docID string, doc bson.M, preImage bson.M) (*wal.Entry, error) {
	postBytes, err := bson.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal post-image: %w", err)
	}
	entry := &wal.Entry{
		ProjectID:  i.branch.ProjectID,
		BranchID:   i.branch.ID,
		Operation:  wal.OpPut,
		Collection: collection,
		DocumentID: docID,
		PostImage:  postBytes,
		Actor:      i.actor,
	}
	if preImage != nil {
		preBytes, err := bson.Marshal(preImage)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal pre-image: %w", err)
		}
		entry.PreImage = preBytes
	}
	return entry, nil
}

// append writes one entry and advances the branch head, both durably and on
// the in-memory branch so this handle reads its own writes.
func (i *Interceptor) append(entry *wal.Entry) error {
	lsn, err := i.wal.Append(entry)
	if err != nil {
		return fmt.Errorf("failed to append to WAL: %w", err)
	}
	return i.advanceHead(lsn)
}

func (i *Interceptor) appendBatch(entries []*wal.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	lsns, err := i.wal.AppendBatch(entries)
	if err != nil {
		return fmt.Errorf("failed to append to WAL: %w", err)
	}
	return i.advanceHead(lsns[len(lsns)-1])
}

func (i *Interceptor) advanceHead(lsn int64) error {
	if err := i.branches.UpdateBranchHead(i.branch.ID, lsn); err != nil {
		return fmt.Errorf("failed to update branch head: %w", err)
	}
	if lsn > i.branch.HeadLSN {
		i.branch.HeadLSN = lsn
	}
	if i.autoSnapshot != nil {
		i.autoSnapshot.MaybeSnapshot(i.branch)
	}
	return nil
}

// ensureDocumentID normalizes the document and guarantees it has an _id.
// The returned document is always a private copy.
func (i *Interceptor) ensureDocumentID(document interface{}) (interface{}, bson.M, error) {
	doc, ok := toBSONM(document)
	if !ok {
		raw, err := bson.Marshal(document)
		if err != nil {
			return nil, nil, fmt.Errorf("document must be BSON-marshallable: %w", err)
		}
		if err := bson.Unmarshal(raw, &doc); err != nil {
			return nil, nil, fmt.Errorf("document must decode to a BSON document: %w", err)
		}
	} else {
		cloned, err := cloneDoc(doc)
		if err != nil {
			return nil, nil, err
		}
		doc = cloned
	}

	if id, exists := doc["_id"]; exists && id != nil {
		return id, doc, nil
	}

	id := primitive.NewObjectID()
	doc["_id"] = id
	return id, doc, nil
}

// normalizeFilter converts any filter representation to bson.M.
func normalizeFilter(filter interface{}) (bson.M, error) {
	if filter == nil {
		return bson.M{}, nil
	}
	if doc, ok := toBSONM(filter); ok {
		return doc, nil
	}
	raw, err := bson.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("filter must be BSON-marshallable: %w", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("filter must decode to a BSON document: %w", err)
	}
	return doc, nil
}

// extractIDEquality returns the _id value if the filter pins _id to exactly
// one value (a literal or a lone {$eq: v}).
func extractIDEquality(filter bson.M) (interface{}, bool) {
	idCond, exists := filter["_id"]
	if !exists {
		return nil, false
	}
	if opDoc, isOp := asOperatorDoc(idCond); isOp {
		if eq, has := opDoc["$eq"]; has && len(opDoc) == 1 {
			return eq, true
		}
		return nil, false
	}
	return idCond, true
}

// docsDiffer reports whether two documents differ under the canonical
// (sorted-key) representation. Marshalling each map directly would compare
// randomized key orders and misreport identical documents as changed.
func docsDiffer(a, b bson.M) (bool, error) {
	equal, err := CanonicalEqual(a, b)
	if err != nil {
		return false, err
	}
	return !equal, nil
}
