package wal

import (
	"context"
	"fmt"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Collection represents a WAL-enabled collection
type Collection struct {
	name         string
	branch       *wal.Branch
	interceptor  *Interceptor
	materializer Materializer
	underlying   *mongo.Collection
}

// Materializer interface for building state from WAL
type Materializer interface {
	MaterializeCollection(branch *wal.Branch, collection string) (map[string]bson.M, error)
	MaterializeDocument(branch *wal.Branch, collection, documentID string) (bson.M, error)
	MaterializeBranch(branch *wal.Branch) (map[string]map[string]bson.M, error)
}

// NewCollection creates a new WAL-enabled collection
func NewCollection(
	name string,
	branch *wal.Branch,
	walService *wal.Service,
	branchService *branchwal.BranchService,
	materializer Materializer,
	underlying *mongo.Collection,
) *Collection {
	interceptor := NewInterceptor(walService, branch, branchService)

	return &Collection{
		name:         name,
		branch:       branch,
		interceptor:  interceptor,
		materializer: materializer,
		underlying:   underlying,
	}
}

// InsertOne inserts a single document
func (c *Collection) InsertOne(ctx context.Context, document interface{}, opts ...*options.InsertOneOptions) (*mongo.InsertOneResult, error) {
	result, err := c.interceptor.InsertOne(ctx, c.name, document)
	if err != nil {
		return nil, err
	}

	return &mongo.InsertOneResult{
		InsertedID: result.InsertedID,
	}, nil
}

// InsertMany inserts multiple documents
func (c *Collection) InsertMany(ctx context.Context, documents []interface{}, opts ...*options.InsertManyOptions) (*mongo.InsertManyResult, error) {
	insertedIDs, err := c.interceptor.InsertMany(ctx, c.name, documents)
	if err != nil {
		return nil, err
	}

	return &mongo.InsertManyResult{
		InsertedIDs: insertedIDs,
	}, nil
}

// UpdateOne updates a single document
func (c *Collection) UpdateOne(ctx context.Context, filter, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	result, err := c.interceptor.UpdateOne(ctx, c.name, filter, update)
	if err != nil {
		return nil, err
	}

	return &mongo.UpdateResult{
		MatchedCount:  result.MatchedCount,
		ModifiedCount: result.ModifiedCount,
		UpsertedCount: 0,
		UpsertedID:    result.UpsertedID,
	}, nil
}

// DeleteOne deletes a single document
func (c *Collection) DeleteOne(ctx context.Context, filter interface{}, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error) {
	result, err := c.interceptor.DeleteOne(ctx, c.name, filter)
	if err != nil {
		return nil, err
	}

	return &mongo.DeleteResult{
		DeletedCount: result.DeletedCount,
	}, nil
}

// Find executes a query and returns a cursor
func (c *Collection) Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (*mongo.Cursor, error) {
	// For MVP, we'll materialize and filter in memory
	// Future: implement proper cursor support

	// Convert filter to bson.M
	var filterDoc bson.M
	if filter == nil {
		filterDoc = bson.M{}
	} else {
		switch f := filter.(type) {
		case bson.M:
			filterDoc = f
		case map[string]interface{}:
			filterDoc = bson.M(f)
		default:
			bytes, _ := bson.Marshal(filter)
			_ = bson.Unmarshal(bytes, &filterDoc)
		}
	}

	// Materialize collection state
	state, err := c.materializer.MaterializeCollection(c.branch, c.name)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize collection: %w", err)
	}

	// Apply filter
	var results []interface{}
	for _, doc := range state {
		if matchesFilter(doc, filterDoc) {
			results = append(results, doc)
		}
	}

	// Create a mock cursor with results
	// In a real implementation, we'd return a proper cursor
	return createMockCursor(results), nil
}

// FindOne executes a query and returns a single result
func (c *Collection) FindOne(ctx context.Context, filter interface{}, opts ...*options.FindOneOptions) *mongo.SingleResult {
	// Convert filter to bson.M
	var filterDoc bson.M
	if filter == nil {
		filterDoc = bson.M{}
	} else {
		switch f := filter.(type) {
		case bson.M:
			filterDoc = f
		case map[string]interface{}:
			filterDoc = bson.M(f)
		default:
			bytes, _ := bson.Marshal(filter)
			_ = bson.Unmarshal(bytes, &filterDoc)
		}
	}

	// Materialize collection state
	state, err := c.materializer.MaterializeCollection(c.branch, c.name)
	if err != nil {
		return mongo.NewSingleResultFromDocument(nil, err, nil)
	}

	// Find first matching document
	for _, doc := range state {
		if matchesFilter(doc, filterDoc) {
			return mongo.NewSingleResultFromDocument(doc, nil, nil)
		}
	}

	// No match found
	return mongo.NewSingleResultFromDocument(nil, mongo.ErrNoDocuments, nil)
}

// CountDocuments counts documents matching the filter
func (c *Collection) CountDocuments(ctx context.Context, filter interface{}, opts ...*options.CountOptions) (int64, error) {
	// Convert filter to bson.M
	var filterDoc bson.M
	if filter == nil {
		filterDoc = bson.M{}
	} else {
		switch f := filter.(type) {
		case bson.M:
			filterDoc = f
		case map[string]interface{}:
			filterDoc = bson.M(f)
		default:
			bytes, _ := bson.Marshal(filter)
			_ = bson.Unmarshal(bytes, &filterDoc)
		}
	}

	// Materialize collection state
	state, err := c.materializer.MaterializeCollection(c.branch, c.name)
	if err != nil {
		return 0, fmt.Errorf("failed to materialize collection: %w", err)
	}

	// Count matching documents
	var count int64
	for _, doc := range state {
		if matchesFilter(doc, filterDoc) {
			count++
		}
	}

	return count, nil
}

// Name returns the collection name
func (c *Collection) Name() string {
	return c.name
}

// matchesFilter checks if a document matches a filter with MongoDB operators support
func matchesFilter(doc, filter bson.M) bool {
	// Empty filter matches all
	if len(filter) == 0 {
		return true
	}

	// Check each filter field
	for key, expected := range filter {
		actual, exists := doc[key]
		if !exists {
			return false
		}

		// Handle different operator types
		switch exp := expected.(type) {
		case bson.M:
			// Handle operators like $gt, $lt, etc.
			if !matchesOperators(actual, exp) {
				return false
			}
		case map[string]interface{}:
			// Convert to bson.M and handle as operators
			if !matchesOperators(actual, bson.M(exp)) {
				return false
			}
		default:
			// Simple equality check
			if !isEqual(actual, expected) {
				return false
			}
		}
	}

	return true
}

// matchesOperators handles MongoDB query operators
func matchesOperators(value interface{}, operators bson.M) bool {
	for op, operand := range operators {
		switch op {
		case "$eq":
			if !isEqual(value, operand) {
				return false
			}
		case "$ne":
			if isEqual(value, operand) {
				return false
			}
		case "$gt":
			if !isGreaterThan(value, operand) {
				return false
			}
		case "$gte":
			if !isGreaterThanOrEqual(value, operand) {
				return false
			}
		case "$lt":
			if !isLessThan(value, operand) {
				return false
			}
		case "$lte":
			if !isLessThanOrEqual(value, operand) {
				return false
			}
		case "$in":
			if !isInArray(value, operand) {
				return false
			}
		case "$nin":
			if isInArray(value, operand) {
				return false
			}
		default:
			// Unknown operator, skip
		}
	}
	return true
}

// isEqual compares two values for equality
func isEqual(a, b interface{}) bool {
	// Handle BSON type conversions
	aBytes, _ := bson.Marshal(bson.M{"v": a})
	bBytes, _ := bson.Marshal(bson.M{"v": b})

	var aDoc, bDoc bson.M
	_ = bson.Unmarshal(aBytes, &aDoc)
	_ = bson.Unmarshal(bBytes, &bDoc)

	return fmt.Sprintf("%v", aDoc["v"]) == fmt.Sprintf("%v", bDoc["v"])
}

// createMockCursor creates a simple cursor from results
// TODO: Implement proper cursor for production
func createMockCursor(results []interface{}) *mongo.Cursor {
	// This is a placeholder - in production we'd implement a proper cursor
	// that can stream results efficiently
	return nil
}

// Comparison helper functions
func isGreaterThan(a, b interface{}) bool {
	return compareValues(a, b) > 0
}

func isGreaterThanOrEqual(a, b interface{}) bool {
	return compareValues(a, b) >= 0
}

func isLessThan(a, b interface{}) bool {
	return compareValues(a, b) < 0
}

func isLessThanOrEqual(a, b interface{}) bool {
	return compareValues(a, b) <= 0
}

func isInArray(value, array interface{}) bool {
	switch arr := array.(type) {
	case []interface{}:
		for _, item := range arr {
			if isEqual(value, item) {
				return true
			}
		}
	case primitive.A: // BSON array type
		for _, item := range arr {
			if isEqual(value, item) {
				return true
			}
		}
	case []string:
		valueStr := fmt.Sprintf("%v", value)
		for _, item := range arr {
			if item == valueStr {
				return true
			}
		}
	}
	return false
}

func compareValues(a, b interface{}) int {
	// Simple numeric comparison for MVP
	aFloat := toFloat64(a)
	bFloat := toFloat64(b)

	if aFloat > bFloat {
		return 1
	} else if aFloat < bFloat {
		return -1
	}
	return 0
}

func toFloat64(v interface{}) float64 {
	switch val := v.(type) {
	case int:
		return float64(val)
	case int32:
		return float64(val)
	case int64:
		return float64(val)
	case float32:
		return float64(val)
	case float64:
		return val
	default:
		// Try to convert via string
		if str := fmt.Sprintf("%v", v); str != "" {
			if f, err := fmt.Sscanf(str, "%f", new(float64)); err == nil && f == 1 {
				var result float64
				_, _ = fmt.Sscanf(str, "%f", &result)
				return result
			}
		}
		return 0
	}
}
