package storage

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStorageManager(t *testing.T) {
	tests := []struct {
		name        string
		storageType string
		config      map[string]interface{}
		expectError bool
	}{
		{
			name:        "Local storage",
			storageType: "local",
			config: map[string]interface{}{
				"path": "/tmp/argon-test",
			},
			expectError: false,
		},
		{
			name:        "Invalid storage type",
			storageType: "invalid",
			config:      map[string]interface{}{},
			expectError: true,
		},
		{
			name:        "S3 storage without credentials",
			storageType: "s3",
			config: map[string]interface{}{
				"bucket": "test-bucket",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, err := NewStorageManager(tt.storageType, tt.config)
			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, manager)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, manager)
			}
		})
	}
}

func TestLocalStorage(t *testing.T) {
	// Create temporary directory
	tempDir, err := ioutil.TempDir("", "argon-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	storage, err := NewLocalStorage(tempDir)
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("Store and Retrieve", func(t *testing.T) {
		key := "test/key"
		data := []byte("test data")

		// Store data
		err := storage.Put(ctx, key, data)
		assert.NoError(t, err)

		// Retrieve data
		retrieved, err := storage.Get(ctx, key)
		assert.NoError(t, err)
		assert.Equal(t, data, retrieved)
	})

	t.Run("Exists", func(t *testing.T) {
		key := "test/exists"
		data := []byte("test data")

		// Check non-existent key
		exists, err := storage.Exists(ctx, key)
		assert.NoError(t, err)
		assert.False(t, exists)

		// Store data
		err = storage.Put(ctx, key, data)
		assert.NoError(t, err)

		// Check existing key
		exists, err = storage.Exists(ctx, key)
		assert.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("Delete", func(t *testing.T) {
		key := "test/delete"
		data := []byte("test data")

		// Store data
		err := storage.Put(ctx, key, data)
		assert.NoError(t, err)

		// Delete data
		err = storage.Delete(ctx, key)
		assert.NoError(t, err)

		// Verify deletion
		exists, err := storage.Exists(ctx, key)
		assert.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("List", func(t *testing.T) {
		prefix := "test/list/"
		keys := []string{
			prefix + "file1",
			prefix + "file2",
			prefix + "subdir/file3",
		}

		// Store multiple files
		for _, key := range keys {
			err := storage.Put(ctx, key, []byte("data"))
			assert.NoError(t, err)
		}

		// List with prefix
		listed, err := storage.List(ctx, prefix)
		assert.NoError(t, err)
		assert.Len(t, listed, 3)

		// List with more specific prefix
		listed, err = storage.List(ctx, prefix+"subdir/")
		assert.NoError(t, err)
		assert.Len(t, listed, 1)
	})
}

func TestCompression(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "argon-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	storage, err := NewStorageManager("local", map[string]interface{}{
		"path": tempDir,
	})
	require.NoError(t, err)

	// Enable compression
	storage.EnableCompression("zstd", 3)

	ctx := context.Background()

	t.Run("Compress and Decompress", func(t *testing.T) {
		key := "compressed/data"
		// Create compressible data
		data := bytes.Repeat([]byte("test data "), 1000)

		// Store with compression
		err := storage.Store(ctx, key, data)
		assert.NoError(t, err)

		// Retrieve and verify
		retrieved, err := storage.Retrieve(ctx, key)
		assert.NoError(t, err)
		assert.Equal(t, data, retrieved)

		// Verify actual file is compressed (smaller)
		filePath := filepath.Join(tempDir, key)
		fileInfo, err := os.Stat(filePath)
		assert.NoError(t, err)
		assert.Less(t, fileInfo.Size(), int64(len(data)))
	})

	t.Run("Compression Levels", func(t *testing.T) {
		data := bytes.Repeat([]byte("test data "), 1000)
		sizes := make(map[int]int64)

		for level := 1; level <= 5; level++ {
			storage.EnableCompression("zstd", level)
			key := fmt.Sprintf("compression/level-%d", level)
			
			err := storage.Store(ctx, key, data)
			assert.NoError(t, err)

			filePath := filepath.Join(tempDir, key)
			fileInfo, err := os.Stat(filePath)
			assert.NoError(t, err)
			sizes[level] = fileInfo.Size()
		}

		// Higher compression levels should generally produce smaller files
		assert.LessOrEqual(t, sizes[5], sizes[1])
	})
}

func TestContentAddressableStorage(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "argon-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	cas, err := NewContentAddressableStorage(tempDir)
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("Store and Retrieve by Hash", func(t *testing.T) {
		data := []byte("test content")

		// Store data
		hash, err := cas.Store(ctx, data)
		assert.NoError(t, err)
		assert.NotEmpty(t, hash)

		// Retrieve by hash
		retrieved, err := cas.Retrieve(ctx, hash)
		assert.NoError(t, err)
		assert.Equal(t, data, retrieved)
	})

	t.Run("Deduplication", func(t *testing.T) {
		data := []byte("duplicate content")

		// Store same data multiple times
		hash1, err := cas.Store(ctx, data)
		assert.NoError(t, err)

		hash2, err := cas.Store(ctx, data)
		assert.NoError(t, err)

		// Should get same hash
		assert.Equal(t, hash1, hash2)

		// Should only store once
		files, err := ioutil.ReadDir(tempDir)
		assert.NoError(t, err)
		assert.Len(t, files, 1)
	})

	t.Run("Different Content Different Hash", func(t *testing.T) {
		data1 := []byte("content 1")
		data2 := []byte("content 2")

		hash1, err := cas.Store(ctx, data1)
		assert.NoError(t, err)

		hash2, err := cas.Store(ctx, data2)
		assert.NoError(t, err)

		assert.NotEqual(t, hash1, hash2)
	})
}

func TestStorageMetrics(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "argon-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	storage, err := NewStorageManager("local", map[string]interface{}{
		"path": tempDir,
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Perform operations
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("metrics/file-%d", i)
		data := []byte(fmt.Sprintf("data %d", i))
		err := storage.Store(ctx, key, data)
		assert.NoError(t, err)
	}

	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("metrics/file-%d", i)
		_, err := storage.Retrieve(ctx, key)
		assert.NoError(t, err)
	}

	// Get metrics
	metrics := storage.GetMetrics()
	assert.Equal(t, int64(10), metrics.Writes)
	assert.Equal(t, int64(5), metrics.Reads)
	assert.Greater(t, metrics.BytesWritten, int64(0))
	assert.Greater(t, metrics.BytesRead, int64(0))
}

func TestConcurrentAccess(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "argon-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	storage, err := NewStorageManager("local", map[string]interface{}{
		"path": tempDir,
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Concurrent writes
	done := make(chan bool, 100)
	errors := make(chan error, 100)

	for i := 0; i < 100; i++ {
		go func(idx int) {
			key := fmt.Sprintf("concurrent/file-%d", idx)
			data := []byte(fmt.Sprintf("data %d", idx))
			
			if err := storage.Store(ctx, key, data); err != nil {
				errors <- err
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 100; i++ {
		<-done
	}

	close(errors)
	
	// Check for errors
	for err := range errors {
		t.Errorf("Concurrent write failed: %v", err)
	}

	// Verify all files were written
	files, err := storage.List(ctx, "concurrent/")
	assert.NoError(t, err)
	assert.Len(t, files, 100)
}

func BenchmarkLocalStorageWrite(b *testing.B) {
	tempDir, err := ioutil.TempDir("", "argon-bench-*")
	require.NoError(b, err)
	defer os.RemoveAll(tempDir)

	storage, err := NewLocalStorage(tempDir)
	require.NoError(b, err)

	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 1024) // 1KB

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench/%d", i)
		storage.Put(ctx, key, data)
	}
}

func BenchmarkLocalStorageRead(b *testing.B) {
	tempDir, err := ioutil.TempDir("", "argon-bench-*")
	require.NoError(b, err)
	defer os.RemoveAll(tempDir)

	storage, err := NewLocalStorage(tempDir)
	require.NoError(b, err)

	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 1024) // 1KB

	// Pre-populate data
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("bench/%d", i)
		storage.Put(ctx, key, data)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench/%d", i%1000)
		storage.Get(ctx, key)
	}
}

func BenchmarkCompression(b *testing.B) {
	tempDir, err := ioutil.TempDir("", "argon-bench-*")
	require.NoError(b, err)
	defer os.RemoveAll(tempDir)

	storage, err := NewStorageManager("local", map[string]interface{}{
		"path": tempDir,
	})
	require.NoError(b, err)

	storage.EnableCompression("zstd", 3)

	ctx := context.Background()
	data := bytes.Repeat([]byte("test data "), 1000) // ~9KB of compressible data

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench/%d", i)
		storage.Store(ctx, key, data)
	}
}