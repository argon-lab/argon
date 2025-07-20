package branch

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// BranchDatabase provides branch-aware MongoDB operations with collection isolation
type BranchDatabase struct {
	client   *mongo.Client
	dbName   string
	branchID string
	prefix   string
}

// NewBranchDatabase creates a new branch-aware database wrapper
func NewBranchDatabase(client *mongo.Client, dbName, branchID string) *BranchDatabase {
	// Generate branch prefix from branch ID
	prefix := generateBranchPrefix(branchID)
	
	return &BranchDatabase{
		client:   client,
		dbName:   dbName,
		branchID: branchID,
		prefix:   prefix,
	}
}

// Collection returns a MongoDB collection with branch-aware naming
func (bd *BranchDatabase) Collection(name string) *mongo.Collection {
	collectionName := bd.getCollectionName(name)
	return bd.client.Database(bd.dbName).Collection(collectionName)
}

// Database returns the underlying MongoDB database
func (bd *BranchDatabase) Database() *mongo.Database {
	return bd.client.Database(bd.dbName)
}

// ListBranchCollections returns all collections belonging to this branch
func (bd *BranchDatabase) ListBranchCollections(ctx context.Context) ([]string, error) {
	db := bd.client.Database(bd.dbName)
	
	// List all collections
	cursor, err := db.ListCollections(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	
	var collections []string
	for cursor.Next(ctx) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			continue
		}
		
		if name, ok := result["name"].(string); ok {
			// Only include collections that belong to this branch
			if bd.belongsToBranch(name) {
				collections = append(collections, name)
			}
		}
	}
	
	return collections, nil
}

// CreateBranchCollections creates collections for a new branch by copying from parent
func (bd *BranchDatabase) CreateBranchCollections(ctx context.Context, parentBranchDB *BranchDatabase, collections []string) error {
	db := bd.client.Database(bd.dbName)
	
	for _, baseCollection := range collections {
		// Skip metadata collections
		if isMetadataCollection(baseCollection) {
			continue
		}
		
		parentCollection := parentBranchDB.Collection(baseCollection)
		targetCollectionName := bd.getCollectionName(baseCollection)
		
		// Copy data using aggregation pipeline with $out
		pipeline := []bson.M{
			{"$out": targetCollectionName},
		}
		
		_, err := parentCollection.Aggregate(ctx, pipeline)
		if err != nil {
			return fmt.Errorf("failed to copy collection %s: %w", baseCollection, err)
		}
		
		// Copy indexes
		if err := bd.copyIndexes(ctx, parentCollection, db.Collection(targetCollectionName)); err != nil {
			return fmt.Errorf("failed to copy indexes for %s: %w", baseCollection, err)
		}
	}
	
	return nil
}

// DeleteBranchCollections removes all collections belonging to this branch
func (bd *BranchDatabase) DeleteBranchCollections(ctx context.Context) error {
	collections, err := bd.ListBranchCollections(ctx)
	if err != nil {
		return err
	}
	
	db := bd.client.Database(bd.dbName)
	
	for _, collection := range collections {
		if err := db.Collection(collection).Drop(ctx); err != nil {
			return fmt.Errorf("failed to drop collection %s: %w", collection, err)
		}
	}
	
	return nil
}

// GetBranchInfo returns information about this branch's database state
func (bd *BranchDatabase) GetBranchInfo(ctx context.Context) (*BranchDatabaseInfo, error) {
	collections, err := bd.ListBranchCollections(ctx)
	if err != nil {
		return nil, err
	}
	
	info := &BranchDatabaseInfo{
		BranchID:    bd.branchID,
		Prefix:      bd.prefix,
		Collections: make(map[string]*CollectionInfo),
	}
	
	// Get stats for each collection
	for _, collectionName := range collections {
		// Get collection stats
		var stats bson.M
		err := bd.client.Database(bd.dbName).RunCommand(ctx, bson.M{
			"collStats": collectionName,
		}).Decode(&stats)
		
		if err == nil {
			info.Collections[collectionName] = &CollectionInfo{
				Name:      collectionName,
				Count:     getInt64FromBSON(stats, "count"),
				Size:      getInt64FromBSON(stats, "size"),
				IndexSize: getInt64FromBSON(stats, "totalIndexSize"),
			}
		}
	}
	
	return info, nil
}

// Helper methods

func (bd *BranchDatabase) getCollectionName(baseName string) string {
	// Don't prefix metadata collections
	if isMetadataCollection(baseName) {
		return baseName
	}
	
	// For main branch (empty prefix), use base name
	if bd.prefix == "" {
		return baseName
	}
	
	// For feature branches, use prefixed name
	return fmt.Sprintf("%s_%s", bd.prefix, baseName)
}

func (bd *BranchDatabase) belongsToBranch(collectionName string) bool {
	// Metadata collections don't belong to any specific branch
	if isMetadataCollection(collectionName) {
		return false
	}
	
	// For main branch (empty prefix), collection belongs if it's not prefixed
	if bd.prefix == "" {
		return !strings.Contains(collectionName, "_")
	}
	
	// For feature branches, collection belongs if it has the right prefix
	return strings.HasPrefix(collectionName, bd.prefix+"_")
}

func (bd *BranchDatabase) copyIndexes(ctx context.Context, srcCollection, destCollection *mongo.Collection) error {
	// Get indexes from source collection
	cursor, err := srcCollection.Indexes().List(ctx)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)
	
	var indexes []mongo.IndexModel
	for cursor.Next(ctx) {
		var index bson.M
		if err := cursor.Decode(&index); err != nil {
			continue
		}
		
		// Skip the default _id index
		if name, ok := index["name"].(string); ok && name == "_id_" {
			continue
		}
		
		// Extract key and options
		if key, ok := index["key"].(bson.M); ok {
			indexModel := mongo.IndexModel{Keys: key}
			
			// Add index options if present
			opts := options.Index()
			if name, ok := index["name"].(string); ok {
				opts.SetName(name)
			}
			if unique, ok := index["unique"].(bool); ok && unique {
				opts.SetUnique(true)
			}
			if sparse, ok := index["sparse"].(bool); ok && sparse {
				opts.SetSparse(true)
			}
			
			indexModel.Options = opts
			indexes = append(indexes, indexModel)
		}
	}
	
	// Create indexes on destination collection
	if len(indexes) > 0 {
		_, err := destCollection.Indexes().CreateMany(ctx, indexes)
		return err
	}
	
	return nil
}

// generateBranchPrefix creates a safe prefix from branch ID
func generateBranchPrefix(branchID string) string {
	if branchID == "" {
		return ""
	}
	
	// For main branch, use empty prefix
	if branchID == "main" || branchID == "master" {
		return ""
	}
	
	// For other branches, create a safe prefix
	// Take first 8 characters of branch ID to keep collection names reasonable
	prefix := branchID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	
	// Ensure prefix is MongoDB collection name safe
	prefix = strings.ToLower(prefix)
	prefix = strings.ReplaceAll(prefix, "-", "")
	prefix = strings.ReplaceAll(prefix, ".", "")
	
	return prefix
}

// isMetadataCollection checks if a collection is a metadata collection that shouldn't be branched
func isMetadataCollection(name string) bool {
	metadataCollections := []string{
		"branches",
		"projects", 
		"users",
		"change_events",
		"jobs",
		"sessions",
		"auth_tokens",
	}
	
	for _, metaCollection := range metadataCollections {
		if name == metaCollection {
			return true
		}
	}
	
	return false
}

// getInt64FromBSON safely extracts int64 value from BSON document
func getInt64FromBSON(doc bson.M, key string) int64 {
	if val, ok := doc[key]; ok {
		switch v := val.(type) {
		case int64:
			return v
		case int32:
			return int64(v)
		case int:
			return int64(v)
		case float64:
			return int64(v)
		}
	}
	return 0
}

// BranchDatabaseInfo contains information about a branch's database state
type BranchDatabaseInfo struct {
	BranchID    string                    `json:"branch_id"`
	Prefix      string                    `json:"prefix"`
	Collections map[string]*CollectionInfo `json:"collections"`
}

// CollectionInfo contains information about a collection
type CollectionInfo struct {
	Name      string `json:"name"`
	Count     int64  `json:"count"`
	Size      int64  `json:"size"`
	IndexSize int64  `json:"index_size"`
}