package snapshot

import (
	"context"
	"fmt"
	"os"

	"go.mongodb.org/mongo-driver/mongo"
)

// Environment configuration for the snapshot chunk store backend:
//
//	ARGON_SNAPSHOT_STORE   mongodb (default) | s3 | filesystem
//	ARGON_S3_BUCKET        bucket name (required for s3)
//	ARGON_S3_PREFIX        key prefix (default "argon/chunks")
//	ARGON_S3_ENDPOINT      custom endpoint for MinIO/R2/Ceph (optional)
//	ARGON_SNAPSHOT_DIR     directory (required for filesystem)
//
// S3 credentials and region resolve through the standard AWS chain
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_REGION, shared config,
// IAM roles). mongodb is the zero-configuration default so Argon works out
// of the box; cloud deployments should set ARGON_SNAPSHOT_STORE=s3.

// NewChunkStoreFromEnv builds the chunk store selected by the environment,
// returning the store and a short description for logs.
func NewChunkStoreFromEnv(ctx context.Context, db *mongo.Database) (ChunkStore, string, error) {
	backend := os.Getenv("ARGON_SNAPSHOT_STORE")
	switch backend {
	case "", "mongodb":
		return NewMongoChunkStore(db), "mongodb", nil

	case "filesystem":
		dir := os.Getenv("ARGON_SNAPSHOT_DIR")
		if dir == "" {
			return nil, "", fmt.Errorf("ARGON_SNAPSHOT_STORE=filesystem requires ARGON_SNAPSHOT_DIR")
		}
		store, err := NewFilesystemChunkStore(dir)
		if err != nil {
			return nil, "", err
		}
		return store, "filesystem:" + dir, nil

	case "s3":
		bucket := os.Getenv("ARGON_S3_BUCKET")
		if bucket == "" {
			return nil, "", fmt.Errorf("ARGON_SNAPSHOT_STORE=s3 requires ARGON_S3_BUCKET")
		}
		store, err := NewS3ChunkStore(ctx, S3Config{
			Bucket:   bucket,
			Prefix:   os.Getenv("ARGON_S3_PREFIX"),
			Endpoint: os.Getenv("ARGON_S3_ENDPOINT"),
		})
		if err != nil {
			return nil, "", err
		}
		return store, "s3://" + bucket, nil

	default:
		return nil, "", fmt.Errorf("unknown ARGON_SNAPSHOT_STORE %q (want mongodb, s3 or filesystem)", backend)
	}
}
