package wal

import (
	"context"
	"fmt"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Collection is a WAL-backed collection handle with a mongo.Collection-like
// surface. Writes are resolved once and logged as physical entries; reads
// materialize branch state and evaluate the filter in-process.
//
// M1 limitations (until reads route through a real mongod in M3): Find
// options (sort/skip/limit/projection) are not applied, aggregation is not
// supported, and results are returned in canonical document-ID order.
type Collection struct {
	name        string
	branch      *wal.Branch
	interceptor *Interceptor
}

// NewCollection creates a new WAL-enabled collection
func NewCollection(
	name string,
	branch *wal.Branch,
	walService *wal.Service,
	branchService *branchwal.BranchService,
	materializer Materializer,
) *Collection {
	return &Collection{
		name:        name,
		branch:      branch,
		interceptor: NewInterceptor(walService, branch, branchService, materializer),
	}
}

// InsertOne inserts a single document
func (c *Collection) InsertOne(ctx context.Context, document interface{}, opts ...*options.InsertOneOptions) (*mongo.InsertOneResult, error) {
	result, err := c.interceptor.InsertOne(ctx, c.name, document)
	if err != nil {
		return nil, err
	}
	return &mongo.InsertOneResult{InsertedID: result.InsertedID}, nil
}

// InsertMany inserts multiple documents
func (c *Collection) InsertMany(ctx context.Context, documents []interface{}, opts ...*options.InsertManyOptions) (*mongo.InsertManyResult, error) {
	insertedIDs, err := c.interceptor.InsertMany(ctx, c.name, documents)
	if err != nil {
		return nil, err
	}
	return &mongo.InsertManyResult{InsertedIDs: insertedIDs}, nil
}

// UpdateOne updates a single document
func (c *Collection) UpdateOne(ctx context.Context, filter, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	result, err := c.interceptor.UpdateOne(ctx, c.name, filter, update, upsertFromUpdateOptions(opts))
	if err != nil {
		return nil, err
	}
	return toMongoUpdateResult(result), nil
}

// UpdateMany updates every matching document
func (c *Collection) UpdateMany(ctx context.Context, filter, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	result, err := c.interceptor.UpdateMany(ctx, c.name, filter, update, upsertFromUpdateOptions(opts))
	if err != nil {
		return nil, err
	}
	return toMongoUpdateResult(result), nil
}

// ReplaceOne replaces a single document
func (c *Collection) ReplaceOne(ctx context.Context, filter, replacement interface{}, opts ...*options.ReplaceOptions) (*mongo.UpdateResult, error) {
	upsert := false
	for _, o := range opts {
		if o != nil && o.Upsert != nil {
			upsert = *o.Upsert
		}
	}
	result, err := c.interceptor.ReplaceOne(ctx, c.name, filter, replacement, upsert)
	if err != nil {
		return nil, err
	}
	return toMongoUpdateResult(result), nil
}

// DeleteOne deletes a single document
func (c *Collection) DeleteOne(ctx context.Context, filter interface{}, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error) {
	result, err := c.interceptor.DeleteOne(ctx, c.name, filter)
	if err != nil {
		return nil, err
	}
	return &mongo.DeleteResult{DeletedCount: result.DeletedCount}, nil
}

// DeleteMany deletes every matching document
func (c *Collection) DeleteMany(ctx context.Context, filter interface{}, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error) {
	result, err := c.interceptor.DeleteMany(ctx, c.name, filter)
	if err != nil {
		return nil, err
	}
	return &mongo.DeleteResult{DeletedCount: result.DeletedCount}, nil
}

// Find executes a query and returns a real cursor over the matching
// documents in canonical document-ID order.
func (c *Collection) Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (*mongo.Cursor, error) {
	docs, err := c.interceptor.FindMatches(c.name, filter, false)
	if err != nil {
		return nil, err
	}
	asInterfaces := make([]interface{}, len(docs))
	for i, doc := range docs {
		asInterfaces[i] = doc
	}
	cursor, err := mongo.NewCursorFromDocuments(asInterfaces, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build cursor: %w", err)
	}
	return cursor, nil
}

// FindOne executes a query and returns the first match in canonical order.
func (c *Collection) FindOne(ctx context.Context, filter interface{}, opts ...*options.FindOneOptions) *mongo.SingleResult {
	docs, err := c.interceptor.FindMatches(c.name, filter, true)
	if err != nil {
		return mongo.NewSingleResultFromDocument(bson.M{}, err, nil)
	}
	if len(docs) == 0 {
		return mongo.NewSingleResultFromDocument(bson.M{}, mongo.ErrNoDocuments, nil)
	}
	return mongo.NewSingleResultFromDocument(docs[0], nil, nil)
}

// CountDocuments counts documents matching the filter
func (c *Collection) CountDocuments(ctx context.Context, filter interface{}, opts ...*options.CountOptions) (int64, error) {
	docs, err := c.interceptor.FindMatches(c.name, filter, false)
	if err != nil {
		return 0, err
	}
	return int64(len(docs)), nil
}

// Name returns the collection name
func (c *Collection) Name() string {
	return c.name
}

func upsertFromUpdateOptions(opts []*options.UpdateOptions) bool {
	for _, o := range opts {
		if o != nil && o.Upsert != nil {
			return *o.Upsert
		}
	}
	return false
}

func toMongoUpdateResult(result *UpdateResult) *mongo.UpdateResult {
	return &mongo.UpdateResult{
		MatchedCount:  result.MatchedCount,
		ModifiedCount: result.ModifiedCount,
		UpsertedCount: result.UpsertedCount,
		UpsertedID:    result.UpsertedID,
	}
}
