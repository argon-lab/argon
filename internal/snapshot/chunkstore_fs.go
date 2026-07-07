package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// fsChunkStore stores chunks as files under a directory, sharded by the
// first two hex characters of the content address to keep directories
// small. Writes are atomic (temp file + rename), and because chunks are
// content-addressed and immutable, an existing file never needs rewriting.
type fsChunkStore struct {
	dir string
}

// NewFilesystemChunkStore creates a chunk store rooted at dir, creating it
// if needed. Suited to self-hosted deployments that want snapshot data out
// of MongoDB without running an object store.
func NewFilesystemChunkStore(dir string) (ChunkStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("filesystem chunk store requires a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create chunk directory %s: %w", dir, err)
	}
	return &fsChunkStore{dir: dir}, nil
}

func (s *fsChunkStore) path(id string) string {
	shard := "00"
	if len(id) >= 2 {
		shard = id[:2]
	}
	return filepath.Join(s.dir, shard, id)
}

func (s *fsChunkStore) Put(ctx context.Context, data []byte) (string, error) {
	id := chunkID(data)
	target := s.path(id)

	if _, err := os.Stat(target); err == nil {
		return id, nil // Content already stored: deduplicated.
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("failed to create chunk shard directory: %w", err)
	}

	// Write-then-rename keeps concurrent writers of the same content safe:
	// both write identical bytes and the rename is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(target), "chunk-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp chunk file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("failed to write chunk %s: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("failed to close chunk %s: %w", id, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("failed to store chunk %s: %w", id, err)
	}
	return id, nil
}

func (s *fsChunkStore) Delete(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete chunk %s: %w", id, err)
		}
	}
	return nil
}

func (s *fsChunkStore) Get(ctx context.Context, id string) ([]byte, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("failed to load chunk %s: %w", id, err)
	}
	return data, nil
}
