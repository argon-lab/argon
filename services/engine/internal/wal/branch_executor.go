package wal

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"github.com/hashicorp/golang-lru/v2"
)

// Branch represents a database branch with WAL-based state
type Branch struct {
	ID             string    `bson:"_id"`
	Name           string    `bson:"name"`
	HeadLSN        int64     `bson:"head_lsn"`
	BaseLSN        int64     `bson:"base_lsn"`
	ParentBranchID string    `bson:"parent_branch_id,omitempty"`
	Created        time.Time `bson:"created"`
}

// MaterializedState represents the state of a collection at a specific LSN
type MaterializedState struct {
	Collection   string
	LSN          int64
	Documents    map[string]interface{}
	LastModified time.Time
}

// BranchExecutor executes queries against a branch's materialized state
type BranchExecutor struct {
	branch          *Branch
	wal             *WALService
	db              *mongo.Database
	stateCache      *lru.Cache[string, *MaterializedState]
	snapshotColl    *mongo.Collection
	cacheMu         sync.RWMutex
	snapshotService *SnapshotService
}

// NewBranchExecutor creates a new branch executor
func NewBranchExecutor(branch *Branch, wal *WALService, db *mongo.Database) (*BranchExecutor, error) {
	cache, err := lru.New[string, *MaterializedState](100)
	if err != nil {
		return nil, err
	}
	
	executor := &BranchExecutor{
		branch:       branch,
		wal:          wal,
		db:           db,
		stateCache:   cache,
		snapshotColl: db.Collection("branch_snapshots"),
	}
	
	// Initialize snapshot service
	executor.snapshotService = NewSnapshotService(executor, db)
	
	return executor, nil
}

// Find executes a find query against the materialized state
func (be *BranchExecutor) Find(ctx context.Context, collectionName string, filter bson.M, opts ...*options.FindOptions) ([]bson.M, error) {
	// Get materialized state
	state, err := be.getMaterializedState(ctx, collectionName)
	if err != nil {
		return nil, err
	}
	
	// Convert documents map to slice
	documents := make([]bson.M, 0, len(state.Documents))
	for _, doc := range state.Documents {
		if docMap, ok := doc.(bson.M); ok {
			documents = append(documents, docMap)
		}
	}
	
	// Apply filter
	filtered := be.applyFilter(documents, filter)
	
	// Apply options
	if len(opts) > 0 {
		opt := opts[0]
		
		// Apply sort
		if opt.Sort != nil {
			filtered = be.applySort(filtered, opt.Sort)
		}
		
		// Apply skip
		if opt.Skip != nil && *opt.Skip > 0 {
			if int(*opt.Skip) < len(filtered) {
				filtered = filtered[*opt.Skip:]
			} else {
				filtered = []bson.M{}
			}
		}
		
		// Apply limit
		if opt.Limit != nil && *opt.Limit > 0 {
			if int(*opt.Limit) < len(filtered) {
				filtered = filtered[:*opt.Limit]
			}
		}
	}
	
	return filtered, nil
}

// FindOne executes a findOne query against the materialized state
func (be *BranchExecutor) FindOne(ctx context.Context, collectionName string, filter bson.M) (bson.M, error) {
	results, err := be.Find(ctx, collectionName, filter, options.Find().SetLimit(1))
	if err != nil {
		return nil, err
	}
	
	if len(results) == 0 {
		return nil, mongo.ErrNoDocuments
	}
	
	return results[0], nil
}

// Count returns the number of documents matching the filter
func (be *BranchExecutor) Count(ctx context.Context, collectionName string, filter bson.M) (int64, error) {
	state, err := be.getMaterializedState(ctx, collectionName)
	if err != nil {
		return 0, err
	}
	
	// Convert documents map to slice for filtering
	documents := make([]bson.M, 0, len(state.Documents))
	for _, doc := range state.Documents {
		if docMap, ok := doc.(bson.M); ok {
			documents = append(documents, docMap)
		}
	}
	
	filtered := be.applyFilter(documents, filter)
	return int64(len(filtered)), nil
}

// getMaterializedState returns the materialized state of a collection at branch HEAD
func (be *BranchExecutor) getMaterializedState(ctx context.Context, collectionName string) (*MaterializedState, error) {
	cacheKey := fmt.Sprintf("%s:%s:%d", be.branch.ID, collectionName, be.branch.HeadLSN)
	
	// Check cache
	be.cacheMu.RLock()
	if cached, ok := be.stateCache.Get(cacheKey); ok {
		be.cacheMu.RUnlock()
		return cached, nil
	}
	be.cacheMu.RUnlock()
	
	// Find nearest snapshot
	snapshot, err := be.findNearestSnapshot(ctx, collectionName)
	if err != nil {
		return nil, err
	}
	
	// Initialize state
	documents := make(map[string]interface{})
	fromLSN := int64(0)
	
	if snapshot != nil {
		// Restore from snapshot
		for _, doc := range snapshot.Documents {
			if docMap, ok := doc.(bson.M); ok {
				if id, ok := docMap["_id"]; ok {
					documents[fmt.Sprintf("%v", id)] = docMap
				}
			}
		}
		fromLSN = snapshot.LSN
	}
	
	// Apply WAL entries from snapshot to HEAD
	entries, err := be.wal.GetEntriesRange(ctx, fromLSN+1, be.branch.HeadLSN)
	if err != nil {
		return nil, err
	}
	
	// Filter entries for this collection and branch lineage
	for _, entry := range entries {
		if entry.Collection != collectionName {
			continue
		}
		
		if !be.isInBranchLineage(entry.Metadata.BranchID) {
			continue
		}
		
		be.applyWALEntry(documents, &entry)
	}
	
	// Create materialized state
	state := &MaterializedState{
		Collection:   collectionName,
		LSN:          be.branch.HeadLSN,
		Documents:    documents,
		LastModified: time.Now(),
	}
	
	// Cache the state
	be.cacheMu.Lock()
	be.stateCache.Add(cacheKey, state)
	be.cacheMu.Unlock()
	
	// Maybe create snapshot
	go be.maybeCreateSnapshot(ctx, collectionName, state)
	
	return state, nil
}

// applyWALEntry applies a WAL entry to the document map
func (be *BranchExecutor) applyWALEntry(documents map[string]interface{}, entry *WALEntry) {
	if entry.DocumentID == nil {
		return
	}
	
	docID := fmt.Sprintf("%v", entry.DocumentID)
	
	switch entry.Operation {
	case "insert":
		if entry.Changes.After != nil {
			documents[docID] = entry.Changes.After
		}
	case "update":
		if entry.Changes.After != nil {
			documents[docID] = entry.Changes.After
		}
	case "delete":
		delete(documents, docID)
	}
}

// isInBranchLineage checks if a branch is in the current branch's lineage
func (be *BranchExecutor) isInBranchLineage(branchID string) bool {
	// Simple check for now - in production, traverse parent chain
	return branchID == be.branch.ID || branchID == be.branch.ParentBranchID
}

// findNearestSnapshot finds the most recent snapshot before HEAD
func (be *BranchExecutor) findNearestSnapshot(ctx context.Context, collectionName string) (*Snapshot, error) {
	var snapshot Snapshot
	err := be.snapshotColl.FindOne(ctx, bson.M{
		"branch_id":  be.branch.ID,
		"collection": collectionName,
		"lsn":        bson.M{"$lte": be.branch.HeadLSN},
	}, options.FindOne().SetSort(bson.D{{Key: "lsn", Value: -1}})).Decode(&snapshot)
	
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	
	return &snapshot, nil
}

// maybeCreateSnapshot creates a snapshot if enough entries have been processed
func (be *BranchExecutor) maybeCreateSnapshot(ctx context.Context, collectionName string, state *MaterializedState) {
	snapshot, _ := be.findNearestSnapshot(ctx, collectionName)
	
	lastSnapshotLSN := int64(0)
	if snapshot != nil {
		lastSnapshotLSN = snapshot.LSN
	}
	
	entriesSinceSnapshot := state.LSN - lastSnapshotLSN
	if entriesSinceSnapshot > 1000 { // Snapshot every 1000 entries
		be.snapshotService.CreateSnapshot(ctx, collectionName, state)
	}
}

// applyFilter applies a MongoDB filter to documents
func (be *BranchExecutor) applyFilter(documents []bson.M, filter bson.M) []bson.M {
	if len(filter) == 0 {
		return documents
	}
	
	var filtered []bson.M
	for _, doc := range documents {
		if be.matchesFilter(doc, filter) {
			filtered = append(filtered, doc)
		}
	}
	
	return filtered
}

// matchesFilter checks if a document matches a filter
func (be *BranchExecutor) matchesFilter(doc bson.M, filter bson.M) bool {
	for key, value := range filter {
		docValue, exists := doc[key]
		
		// Handle special operators
		switch key {
		case "$and":
			if conditions, ok := value.([]interface{}); ok {
				for _, cond := range conditions {
					if condMap, ok := cond.(bson.M); ok {
						if !be.matchesFilter(doc, condMap) {
							return false
						}
					}
				}
				continue
			}
		case "$or":
			if conditions, ok := value.([]interface{}); ok {
				matched := false
				for _, cond := range conditions {
					if condMap, ok := cond.(bson.M); ok {
						if be.matchesFilter(doc, condMap) {
							matched = true
							break
						}
					}
				}
				if !matched {
					return false
				}
				continue
			}
		}
		
		// Handle field-level comparison
		if !exists {
			return false
		}
		
		// Handle operators in value
		if valueMap, ok := value.(bson.M); ok {
			if !be.evaluateOperators(docValue, valueMap) {
				return false
			}
		} else {
			// Simple equality
			if !be.valuesEqual(docValue, value) {
				return false
			}
		}
	}
	
	return true
}

// evaluateOperators evaluates MongoDB query operators
func (be *BranchExecutor) evaluateOperators(fieldValue interface{}, operators bson.M) bool {
	for op, value := range operators {
		switch op {
		case "$eq":
			if !be.valuesEqual(fieldValue, value) {
				return false
			}
		case "$ne":
			if be.valuesEqual(fieldValue, value) {
				return false
			}
		case "$gt":
			if !be.compareValues(fieldValue, value, ">") {
				return false
			}
		case "$gte":
			if !be.compareValues(fieldValue, value, ">=") {
				return false
			}
		case "$lt":
			if !be.compareValues(fieldValue, value, "<") {
				return false
			}
		case "$lte":
			if !be.compareValues(fieldValue, value, "<=") {
				return false
			}
		case "$in":
			if arr, ok := value.([]interface{}); ok {
				found := false
				for _, v := range arr {
					if be.valuesEqual(fieldValue, v) {
						found = true
						break
					}
				}
				if !found {
					return false
				}
			}
		case "$nin":
			if arr, ok := value.([]interface{}); ok {
				for _, v := range arr {
					if be.valuesEqual(fieldValue, v) {
						return false
					}
				}
			}
		case "$exists":
			// This is handled at the field level
			if boolVal, ok := value.(bool); ok {
				return boolVal
			}
		}
	}
	
	return true
}

// valuesEqual compares two values for equality
func (be *BranchExecutor) valuesEqual(a, b interface{}) bool {
	// Handle ObjectID comparison
	if aID, ok := a.(primitive.ObjectID); ok {
		if bID, ok := b.(primitive.ObjectID); ok {
			return aID == bID
		}
		if bStr, ok := b.(string); ok {
			return aID.Hex() == bStr
		}
	}
	
	// Default comparison
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// compareValues compares two values based on operator
func (be *BranchExecutor) compareValues(a, b interface{}, op string) bool {
	// Simple numeric comparison for now
	aFloat, aOK := toFloat64(a)
	bFloat, bOK := toFloat64(b)
	
	if !aOK || !bOK {
		return false
	}
	
	switch op {
	case ">":
		return aFloat > bFloat
	case ">=":
		return aFloat >= bFloat
	case "<":
		return aFloat < bFloat
	case "<=":
		return aFloat <= bFloat
	}
	
	return false
}

// applySort sorts documents based on sort specification
func (be *BranchExecutor) applySort(documents []bson.M, sortSpec interface{}) []bson.M {
	// Convert sort spec to bson.D
	var sortFields bson.D
	
	switch s := sortSpec.(type) {
	case bson.D:
		sortFields = s
	case bson.M:
		for k, v := range s {
			if dir, ok := v.(int); ok {
				sortFields = append(sortFields, bson.E{Key: k, Value: dir})
			}
		}
	}
	
	if len(sortFields) == 0 {
		return documents
	}
	
	// Create a copy to avoid modifying original
	sorted := make([]bson.M, len(documents))
	copy(sorted, documents)
	
	// Simple bubble sort - replace with better algorithm in production
	for i := 0; i < len(sorted)-1; i++ {
		for j := 0; j < len(sorted)-i-1; j++ {
			if be.shouldSwap(sorted[j], sorted[j+1], sortFields) {
				sorted[j], sorted[j+1] = sorted[j+1], sorted[j]
			}
		}
	}
	
	return sorted
}

// shouldSwap determines if two documents should be swapped based on sort order
func (be *BranchExecutor) shouldSwap(a, b bson.M, sortFields bson.D) bool {
	for _, field := range sortFields {
		aVal := a[field.Key]
		bVal := b[field.Key]
		
		direction := 1
		if dir, ok := field.Value.(int); ok {
			direction = dir
		}
		
		cmp := be.compareForSort(aVal, bVal)
		if cmp != 0 {
			return (direction == 1 && cmp > 0) || (direction == -1 && cmp < 0)
		}
	}
	
	return false
}

// compareForSort compares two values for sorting
func (be *BranchExecutor) compareForSort(a, b interface{}) int {
	// Handle nil values
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	
	// Try numeric comparison
	aFloat, aOK := toFloat64(a)
	bFloat, bOK := toFloat64(b)
	
	if aOK && bOK {
		if aFloat < bFloat {
			return -1
		}
		if aFloat > bFloat {
			return 1
		}
		return 0
	}
	
	// Fall back to string comparison
	aStr := fmt.Sprintf("%v", a)
	bStr := fmt.Sprintf("%v", b)
	
	if aStr < bStr {
		return -1
	}
	if aStr > bStr {
		return 1
	}
	return 0
}

// toFloat64 converts a value to float64 if possible
func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int32:
		return float64(val), true
	case int64:
		return float64(val), true
	}
	return 0, false
}

// Snapshot represents a materialized state snapshot
type Snapshot struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	BranchID   string             `bson:"branch_id"`
	Collection string             `bson:"collection"`
	LSN        int64              `bson:"lsn"`
	Documents  []interface{}      `bson:"documents"`
	Count      int                `bson:"count"`
	Created    time.Time          `bson:"created"`
}