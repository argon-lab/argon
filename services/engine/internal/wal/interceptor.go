package wal

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Interceptor wraps MongoDB operations to capture them in the WAL
type Interceptor struct {
	wal       *WALService
	database  string
	projectID string
	branchID  string
	userID    string
}

// NewInterceptor creates a new WAL interceptor
func NewInterceptor(wal *WALService, database, projectID, branchID, userID string) *Interceptor {
	return &Interceptor{
		wal:       wal,
		database:  database,
		projectID: projectID,
		branchID:  branchID,
		userID:    userID,
	}
}

// WrapCollection creates a wrapped collection that intercepts all operations
func (i *Interceptor) WrapCollection(coll *mongo.Collection) *WrappedCollection {
	return &WrappedCollection{
		Collection:  coll,
		interceptor: i,
	}
}

// WrappedCollection wraps a MongoDB collection to intercept operations
type WrappedCollection struct {
	*mongo.Collection
	interceptor *Interceptor
}

// InsertOne intercepts single document inserts
func (wc *WrappedCollection) InsertOne(ctx context.Context, document interface{}, opts ...*options.InsertOneOptions) (*mongo.InsertOneResult, error) {
	// Ensure document has _id
	doc := wc.ensureDocumentID(document)
	
	// Get document ID
	docID := wc.getDocumentID(doc)
	
	// Create WAL entry
	entry := WALEntry{
		Operation:  "insert",
		Database:   wc.interceptor.database,
		Collection: wc.Collection.Name(),
		DocumentID: docID,
		Changes: Changes{
			After: doc,
		},
		Metadata: Metadata{
			UserID:    wc.interceptor.userID,
			BranchID:  wc.interceptor.branchID,
			ProjectID: wc.interceptor.projectID,
			Size:      wc.calculateSize(doc),
		},
	}
	
	// Append to WAL
	if _, err := wc.interceptor.wal.Append(ctx, entry); err != nil {
		return nil, fmt.Errorf("failed to write to WAL: %w", err)
	}
	
	// Execute original operation
	return wc.Collection.InsertOne(ctx, doc, opts...)
}

// InsertMany intercepts multiple document inserts
func (wc *WrappedCollection) InsertMany(ctx context.Context, documents []interface{}, opts ...*options.InsertManyOptions) (*mongo.InsertManyResult, error) {
	// Ensure all documents have _id
	docs := make([]interface{}, len(documents))
	for i, doc := range documents {
		docs[i] = wc.ensureDocumentID(doc)
	}
	
	// Create WAL entries for each document
	for _, doc := range docs {
		docID := wc.getDocumentID(doc)
		
		entry := WALEntry{
			Operation:  "insert",
			Database:   wc.interceptor.database,
			Collection: wc.Collection.Name(),
			DocumentID: docID,
			Changes: Changes{
				After: doc,
			},
			Metadata: Metadata{
				UserID:    wc.interceptor.userID,
				BranchID:  wc.interceptor.branchID,
				ProjectID: wc.interceptor.projectID,
				Size:      wc.calculateSize(doc),
			},
		}
		
		if _, err := wc.interceptor.wal.Append(ctx, entry); err != nil {
			return nil, fmt.Errorf("failed to write to WAL: %w", err)
		}
	}
	
	// Execute original operation
	return wc.Collection.InsertMany(ctx, docs, opts...)
}

// UpdateOne intercepts single document updates
func (wc *WrappedCollection) UpdateOne(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	// Find document before update
	var docBefore bson.M
	err := wc.Collection.FindOne(ctx, filter).Decode(&docBefore)
	if err != nil && err != mongo.ErrNoDocuments {
		return nil, err
	}
	
	// Execute update
	result, err := wc.Collection.UpdateOne(ctx, filter, update, opts...)
	if err != nil {
		return nil, err
	}
	
	// If document was updated, capture the change
	if result.MatchedCount > 0 && docBefore != nil {
		// Find document after update
		var docAfter bson.M
		if err := wc.Collection.FindOne(ctx, bson.M{"_id": docBefore["_id"]}).Decode(&docAfter); err != nil {
			return result, err
		}
		
		// Calculate delta
		delta := wc.calculateDelta(docBefore, docAfter)
		
		// Create WAL entry
		entry := WALEntry{
			Operation:  "update",
			Database:   wc.interceptor.database,
			Collection: wc.Collection.Name(),
			DocumentID: docBefore["_id"],
			Changes: Changes{
				Before: docBefore,
				After:  docAfter,
				Delta:  delta,
			},
			Metadata: Metadata{
				UserID:    wc.interceptor.userID,
				BranchID:  wc.interceptor.branchID,
				ProjectID: wc.interceptor.projectID,
				Size:      wc.calculateSize(delta),
			},
		}
		
		if _, err := wc.interceptor.wal.Append(ctx, entry); err != nil {
			// Log error but don't fail the operation
			fmt.Printf("Failed to write update to WAL: %v\n", err)
		}
	}
	
	return result, nil
}

// UpdateMany intercepts multiple document updates
func (wc *WrappedCollection) UpdateMany(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	// Find all documents before update
	cursor, err := wc.Collection.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	
	var docsBefore []bson.M
	if err := cursor.All(ctx, &docsBefore); err != nil {
		return nil, err
	}
	
	// Map documents by ID
	beforeMap := make(map[interface{}]bson.M)
	for _, doc := range docsBefore {
		beforeMap[doc["_id"]] = doc
	}
	
	// Execute update
	result, err := wc.Collection.UpdateMany(ctx, filter, update, opts...)
	if err != nil {
		return nil, err
	}
	
	// Capture changes for each updated document
	if result.ModifiedCount > 0 {
		// Find all documents after update
		ids := make([]interface{}, 0, len(beforeMap))
		for id := range beforeMap {
			ids = append(ids, id)
		}
		
		cursor, err := wc.Collection.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
		if err != nil {
			return result, err
		}
		
		var docsAfter []bson.M
		if err := cursor.All(ctx, &docsAfter); err != nil {
			return result, err
		}
		
		// Create WAL entries for each changed document
		for _, docAfter := range docsAfter {
			docBefore := beforeMap[docAfter["_id"]]
			delta := wc.calculateDelta(docBefore, docAfter)
			
			entry := WALEntry{
				Operation:  "update",
				Database:   wc.interceptor.database,
				Collection: wc.Collection.Name(),
				DocumentID: docAfter["_id"],
				Changes: Changes{
					Before: docBefore,
					After:  docAfter,
					Delta:  delta,
				},
				Metadata: Metadata{
					UserID:    wc.interceptor.userID,
					BranchID:  wc.interceptor.branchID,
					ProjectID: wc.interceptor.projectID,
					Size:      wc.calculateSize(delta),
				},
			}
			
			if _, err := wc.interceptor.wal.Append(ctx, entry); err != nil {
				fmt.Printf("Failed to write update to WAL: %v\n", err)
			}
		}
	}
	
	return result, nil
}

// DeleteOne intercepts single document deletes
func (wc *WrappedCollection) DeleteOne(ctx context.Context, filter interface{}, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error) {
	// Find document before delete
	var docBefore bson.M
	err := wc.Collection.FindOne(ctx, filter).Decode(&docBefore)
	if err != nil && err != mongo.ErrNoDocuments {
		return nil, err
	}
	
	// Execute delete
	result, err := wc.Collection.DeleteOne(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	
	// If document was deleted, capture it
	if result.DeletedCount > 0 && docBefore != nil {
		entry := WALEntry{
			Operation:  "delete",
			Database:   wc.interceptor.database,
			Collection: wc.Collection.Name(),
			DocumentID: docBefore["_id"],
			Changes: Changes{
				Before: docBefore,
			},
			Metadata: Metadata{
				UserID:    wc.interceptor.userID,
				BranchID:  wc.interceptor.branchID,
				ProjectID: wc.interceptor.projectID,
				Size:      wc.calculateSize(docBefore),
			},
		}
		
		if _, err := wc.interceptor.wal.Append(ctx, entry); err != nil {
			fmt.Printf("Failed to write delete to WAL: %v\n", err)
		}
	}
	
	return result, nil
}

// DeleteMany intercepts multiple document deletes
func (wc *WrappedCollection) DeleteMany(ctx context.Context, filter interface{}, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error) {
	// Find all documents before delete
	cursor, err := wc.Collection.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	
	var docsBefore []bson.M
	if err := cursor.All(ctx, &docsBefore); err != nil {
		return nil, err
	}
	
	// Execute delete
	result, err := wc.Collection.DeleteMany(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	
	// Create WAL entries for each deleted document
	if result.DeletedCount > 0 {
		for _, doc := range docsBefore {
			entry := WALEntry{
				Operation:  "delete",
				Database:   wc.interceptor.database,
				Collection: wc.Collection.Name(),
				DocumentID: doc["_id"],
				Changes: Changes{
					Before: doc,
				},
				Metadata: Metadata{
					UserID:    wc.interceptor.userID,
					BranchID:  wc.interceptor.branchID,
					ProjectID: wc.interceptor.projectID,
					Size:      wc.calculateSize(doc),
				},
			}
			
			if _, err := wc.interceptor.wal.Append(ctx, entry); err != nil {
				fmt.Printf("Failed to write delete to WAL: %v\n", err)
			}
		}
	}
	
	return result, nil
}

// Helper methods

func (wc *WrappedCollection) ensureDocumentID(document interface{}) interface{} {
	// Use reflection to check if document has _id field
	v := reflect.ValueOf(document)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	
	switch v.Kind() {
	case reflect.Map:
		// Handle bson.M, bson.D, etc.
		if m, ok := document.(bson.M); ok {
			if _, hasID := m["_id"]; !hasID {
				m["_id"] = primitive.NewObjectID()
			}
			return m
		}
	case reflect.Struct:
		// Handle struct types
		// This is simplified - in production, use struct tags to find ID field
		// For now, assume the document is already properly structured
	}
	
	return document
}

func (wc *WrappedCollection) getDocumentID(document interface{}) interface{} {
	v := reflect.ValueOf(document)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	
	switch v.Kind() {
	case reflect.Map:
		if m, ok := document.(bson.M); ok {
			return m["_id"]
		}
	case reflect.Struct:
		// Handle struct types - simplified version
		if v.FieldByName("ID").IsValid() {
			return v.FieldByName("ID").Interface()
		}
	}
	
	return nil
}

func (wc *WrappedCollection) calculateDelta(before, after bson.M) map[string]interface{} {
	delta := make(map[string]interface{})
	
	// Find changed fields
	for key, afterValue := range after {
		beforeValue, exists := before[key]
		if !exists || !reflect.DeepEqual(beforeValue, afterValue) {
			delta[key] = afterValue
		}
	}
	
	// Find deleted fields
	for key := range before {
		if _, exists := after[key]; !exists {
			delta[key] = nil
		}
	}
	
	return delta
}

func (wc *WrappedCollection) calculateSize(data interface{}) int64 {
	// Simple size calculation - in production use BSON size
	if data == nil {
		return 0
	}
	
	// Convert to BSON and calculate size
	bytes, err := bson.Marshal(data)
	if err != nil {
		return 0
	}
	
	return int64(len(bytes))
}