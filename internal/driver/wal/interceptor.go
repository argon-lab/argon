package wal

import (
	"context"
	"fmt"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Interceptor intercepts MongoDB operations and redirects them to WAL
type Interceptor struct {
	wal      *wal.Service
	branch   *wal.Branch
	branches *branchwal.BranchService
}

// NewInterceptor creates a new WAL interceptor
func NewInterceptor(walService *wal.Service, branch *wal.Branch, branchService *branchwal.BranchService) *Interceptor {
	return &Interceptor{
		wal:      walService,
		branch:   branch,
		branches: branchService,
	}
}

// InsertResult represents the result of an insert operation
type InsertResult struct {
	InsertedID interface{}
}

// UpdateResult represents the result of an update operation
type UpdateResult struct {
	MatchedCount  int64
	ModifiedCount int64
	UpsertedID    interface{}
}

// DeleteResult represents the result of a delete operation
type DeleteResult struct {
	DeletedCount int64
}

// InsertOne intercepts an insert operation and writes to WAL
func (i *Interceptor) InsertOne(ctx context.Context, collection string, document interface{}) (*InsertResult, error) {
	// Generate or extract document ID
	docID, doc := i.ensureDocumentID(document)

	// Marshal document to BSON
	docBytes, err := bson.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal document: %w", err)
	}

	// Convert ID to string for storage
	docIDStr := ""
	switch id := docID.(type) {
	case primitive.ObjectID:
		docIDStr = id.Hex()
	case string:
		docIDStr = id
	default:
		docIDStr = fmt.Sprintf("%v", id)
	}

	// Create WAL entry
	entry := &wal.Entry{
		ProjectID:  i.branch.ProjectID,
		BranchID:   i.branch.ID,
		Operation:  wal.OpInsert,
		Collection: collection,
		DocumentID: docIDStr,
		Document:   docBytes,
	}

	// Append to WAL
	lsn, err := i.wal.Append(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to append to WAL: %w", err)
	}

	// Update branch HEAD
	if err := i.branches.UpdateBranchHead(i.branch.ID, lsn); err != nil {
		return nil, fmt.Errorf("failed to update branch head: %w", err)
	}

	return &InsertResult{InsertedID: docID}, nil
}

// UpdateOne intercepts an update operation and writes to WAL
func (i *Interceptor) UpdateOne(ctx context.Context, collection string, filter, update interface{}) (*UpdateResult, error) {
	// For WAL, we need to know what document we're updating
	// In a real implementation, we'd materialize and find the document first
	// For MVP, we'll store the filter and update operation

	filterBytes, err := bson.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal filter: %w", err)
	}

	updateBytes, err := bson.Marshal(update)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal update: %w", err)
	}

	// Try to extract document ID from filter if it's a simple _id filter
	var documentID string
	if filterMap, ok := filter.(bson.M); ok {
		if id, exists := filterMap["_id"]; exists {
			documentID = convertIDToString(id)
		}
	}

	// Create a combined document with filter and update
	updateDoc := bson.M{
		"filter": bson.Raw(filterBytes),
		"update": bson.Raw(updateBytes),
	}

	docBytes, err := bson.Marshal(updateDoc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal update document: %w", err)
	}

	// Create WAL entry
	entry := &wal.Entry{
		ProjectID:  i.branch.ProjectID,
		BranchID:   i.branch.ID,
		Operation:  wal.OpUpdate,
		Collection: collection,
		DocumentID: documentID,
		Document:   docBytes,
		Metadata: map[string]interface{}{
			"filter_bytes": len(filterBytes),
			"update_bytes": len(updateBytes),
		},
	}

	// Append to WAL
	lsn, err := i.wal.Append(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to append to WAL: %w", err)
	}

	// Update branch HEAD
	if err := i.branches.UpdateBranchHead(i.branch.ID, lsn); err != nil {
		return nil, fmt.Errorf("failed to update branch head: %w", err)
	}

	// For MVP, assume one document matched and modified
	return &UpdateResult{
		MatchedCount:  1,
		ModifiedCount: 1,
	}, nil
}

// DeleteOne intercepts a delete operation and writes to WAL
func (i *Interceptor) DeleteOne(ctx context.Context, collection string, filter interface{}) (*DeleteResult, error) {
	// Marshal filter
	filterBytes, err := bson.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal filter: %w", err)
	}

	// Try to extract document ID from filter if it's a simple _id filter
	var documentID string
	if filterMap, ok := filter.(bson.M); ok {
		if id, exists := filterMap["_id"]; exists {
			documentID = convertIDToString(id)
		}
	}

	// Create WAL entry
	entry := &wal.Entry{
		ProjectID:  i.branch.ProjectID,
		BranchID:   i.branch.ID,
		Operation:  wal.OpDelete,
		Collection: collection,
		DocumentID: documentID,
		Document:   filterBytes, // Store filter as document
		Metadata: map[string]interface{}{
			"is_filter": true,
		},
	}

	// Append to WAL
	lsn, err := i.wal.Append(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to append to WAL: %w", err)
	}

	// Update branch HEAD
	if err := i.branches.UpdateBranchHead(i.branch.ID, lsn); err != nil {
		return nil, fmt.Errorf("failed to update branch head: %w", err)
	}

	// For MVP, assume one document deleted
	return &DeleteResult{DeletedCount: 1}, nil
}

// InsertMany intercepts a bulk insert operation
func (i *Interceptor) InsertMany(ctx context.Context, collection string, documents []interface{}) ([]interface{}, error) {
	insertedIDs := make([]interface{}, 0, len(documents))

	for _, doc := range documents {
		result, err := i.InsertOne(ctx, collection, doc)
		if err != nil {
			return nil, err
		}
		insertedIDs = append(insertedIDs, result.InsertedID)
	}

	return insertedIDs, nil
}

// ensureDocumentID ensures the document has an _id field
func (i *Interceptor) ensureDocumentID(document interface{}) (interface{}, interface{}) {
	// Convert to bson.M for manipulation
	var doc bson.M

	switch d := document.(type) {
	case bson.M:
		doc = d
	case map[string]interface{}:
		doc = bson.M(d)
	default:
		// For other types, marshal and unmarshal to bson.M
		bytes, _ := bson.Marshal(document)
		_ = bson.Unmarshal(bytes, &doc)
	}

	// Check if _id exists
	if id, exists := doc["_id"]; exists && id != nil {
		return id, doc
	}

	// Generate new ObjectID
	id := primitive.NewObjectID()
	doc["_id"] = id

	return id, doc
}

// convertIDToString converts various ID types to string
func convertIDToString(id interface{}) string {
	switch v := id.(type) {
	case primitive.ObjectID:
		return v.Hex()
	case string:
		return v
	default:
		return fmt.Sprintf("%v", id)
	}
}
