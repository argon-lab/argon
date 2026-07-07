package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Config configures the S3 chunk store. Credentials and region resolve
// through the standard AWS chain (environment, shared config, IAM roles);
// Endpoint supports S3-compatible stores (MinIO, Cloudflare R2, Ceph), which
// also switches to path-style addressing.
type S3Config struct {
	Bucket   string
	Prefix   string // key prefix, default "argon/chunks"
	Endpoint string // optional custom endpoint for S3-compatible stores
}

// s3ChunkStore stores chunks as immutable objects keyed by content address.
type s3ChunkStore struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3ChunkStore creates the default cloud chunk store.
func NewS3ChunkStore(ctx context.Context, cfg S3Config) (ChunkStore, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 chunk store requires a bucket")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "argon/chunks"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			// S3-compatible stores generally don't support virtual-hosted
			// bucket addressing.
			o.UsePathStyle = true
		}
	})

	return &s3ChunkStore{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s *s3ChunkStore) key(id string) string {
	shard := "00"
	if len(id) >= 2 {
		shard = id[:2]
	}
	return s.prefix + "/" + shard + "/" + id
}

func (s *s3ChunkStore) Put(ctx context.Context, data []byte) (string, error) {
	id := chunkID(data)
	key := s.key(id)

	// Chunks are immutable and content-addressed: if the object exists,
	// its bytes are these bytes, so skip the upload entirely.
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return id, nil // Deduplicated.
	}
	var notFound interface{ ErrorCode() string }
	if !errors.As(err, &notFound) || (notFound.ErrorCode() != "NotFound" && notFound.ErrorCode() != "NoSuchKey") {
		return "", fmt.Errorf("failed to check chunk %s: %w", id, err)
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return "", fmt.Errorf("failed to store chunk %s: %w", id, err)
	}
	return id, nil
}

func (s *s3ChunkStore) Delete(ctx context.Context, ids []string) error {
	// Per-object deletes keep this simple and endpoint-compatible; GC
	// batches are small (chunks orphaned by one branch's snapshots).
	for _, id := range ids {
		_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.key(id)),
		})
		if err != nil {
			return fmt.Errorf("failed to delete chunk %s: %w", id, err)
		}
	}
	return nil
}

func (s *s3ChunkStore) Get(ctx context.Context, id string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(id)),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to load chunk %s: %w", id, err)
	}
	defer func() { _ = out.Body.Close() }()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read chunk %s: %w", id, err)
	}
	return data, nil
}
