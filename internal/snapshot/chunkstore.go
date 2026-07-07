package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// ChunkStore stores immutable, content-addressed blobs. The ID of a chunk
// is the hex SHA-256 of its (compressed) bytes, so writing the same content
// twice is free — snapshots of slowly-changing collections share most of
// their chunks.
//
// The MongoDB implementation keeps chunks in a collection, which works on
// any deployment with zero extra infrastructure; object-storage backends
// (S3/GCS) can implement the same interface when WAL segments move there.
type ChunkStore interface {
	// Put stores data and returns its content address.
	Put(ctx context.Context, data []byte) (string, error)
	// Get retrieves a chunk by content address.
	Get(ctx context.Context, id string) ([]byte, error)
	// Delete removes chunks by content address. Missing chunks are not an
	// error — deletion must be idempotent for GC retries.
	Delete(ctx context.Context, ids []string) error
}

// mongoChunkStore stores chunks in the wal_snapshot_chunks collection.
type mongoChunkStore struct {
	chunks *mongo.Collection
}

// NewMongoChunkStore creates a chunk store backed by a MongoDB collection.
func NewMongoChunkStore(db *mongo.Database) ChunkStore {
	return &mongoChunkStore{chunks: db.Collection("wal_snapshot_chunks")}
}

func chunkID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *mongoChunkStore) Put(ctx context.Context, data []byte) (string, error) {
	id := chunkID(data)
	_, err := s.chunks.InsertOne(ctx, bson.M{
		"_id":        id,
		"data":       primitive.Binary{Data: data},
		"size":       len(data),
		"created_at": time.Now(),
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return id, nil // Content already stored: deduplicated.
		}
		return "", fmt.Errorf("failed to store chunk %s: %w", id, err)
	}
	return id, nil
}

func (s *mongoChunkStore) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.chunks.DeleteMany(ctx, bson.M{"_id": bson.M{"$in": ids}})
	return err
}

func (s *mongoChunkStore) Get(ctx context.Context, id string) ([]byte, error) {
	var doc struct {
		Data primitive.Binary `bson:"data"`
	}
	err := s.chunks.FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if err != nil {
		return nil, fmt.Errorf("failed to load chunk %s: %w", id, err)
	}
	return doc.Data.Data, nil
}
