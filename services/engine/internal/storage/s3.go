package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Backend implements cloud storage using AWS S3
type S3Backend struct {
	client *s3.Client
	bucket string
	region string
}

// NewS3Backend creates a new S3 storage backend
func NewS3Backend(bucket, region string) (*S3Backend, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	
	client := s3.NewFromConfig(cfg)
	
	return &S3Backend{
		client: client,
		bucket: bucket,
		region: region,
	}, nil
}

// Upload uploads data to S3
func (s *S3Backend) Upload(ctx context.Context, path string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
		Body:   bytes.NewReader(data),
		Metadata: map[string]string{
			"uploaded-at": time.Now().UTC().Format(time.RFC3339),
			"size":        fmt.Sprintf("%d", len(data)),
		},
	})
	
	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}
	
	return nil
}

// Download downloads data from S3
func (s *S3Backend) Download(ctx context.Context, path string) ([]byte, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	
	if err != nil {
		return nil, fmt.Errorf("failed to download from S3: %w", err)
	}
	defer result.Body.Close()
	
	data, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read S3 object body: %w", err)
	}
	
	return data, nil
}

// Delete deletes an object from S3
func (s *S3Backend) Delete(ctx context.Context, path string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	
	if err != nil {
		return fmt.Errorf("failed to delete from S3: %w", err)
	}
	
	return nil
}

// List lists objects with a given prefix
func (s *S3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	result, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	
	if err != nil {
		return nil, fmt.Errorf("failed to list S3 objects: %w", err)
	}
	
	var keys []string
	for _, obj := range result.Contents {
		if obj.Key != nil {
			keys = append(keys, *obj.Key)
		}
	}
	
	return keys, nil
}

// GetMetadata retrieves object metadata from S3
func (s *S3Backend) GetMetadata(ctx context.Context, path string) (*Metadata, error) {
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	
	if err != nil {
		return nil, fmt.Errorf("failed to get S3 object metadata: %w", err)
	}
	
	metadata := &Metadata{
		Size:        *result.ContentLength,
		ContentType: aws.ToString(result.ContentType),
		Compressed:  false, // Will be determined by file extension or metadata
	}
	
	if result.LastModified != nil {
		metadata.LastModified = result.LastModified.Format(time.RFC3339)
	}
	
	// Check for compression in metadata
	if compType, exists := result.Metadata["compression"]; exists {
		metadata.Compressed = compType != "none"
	}
	
	return metadata, nil
}

// Exists checks if an object exists in S3
func (s *S3Backend) Exists(ctx context.Context, path string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check S3 object existence: %w", err)
	}
	
	return true, nil
}

// Copy copies an object within S3
func (s *S3Backend) Copy(ctx context.Context, srcPath, destPath string) error {
	copySource := fmt.Sprintf("%s/%s", s.bucket, srcPath)
	
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(destPath),
		CopySource: aws.String(copySource),
	})
	
	if err != nil {
		return fmt.Errorf("failed to copy S3 object: %w", err)
	}
	
	return nil
}