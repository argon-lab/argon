package storage

import (
	"context"
	"fmt"

	"argon/engine/internal/config"
)

// CloudBackend defines the interface for cloud storage backends
type CloudBackend interface {
	Upload(ctx context.Context, path string, data []byte) error
	Download(ctx context.Context, path string) ([]byte, error)
	Delete(ctx context.Context, path string) error
	List(ctx context.Context, prefix string) ([]string, error)
	GetMetadata(ctx context.Context, path string) (*Metadata, error)
	Exists(ctx context.Context, path string) (bool, error)
	Copy(ctx context.Context, srcPath, destPath string) error
}

// Service interface for storage operations with real implementations
type Service interface {
	// Core storage operations
	Upload(path string, data []byte) error
	Download(path string) ([]byte, error)
	Delete(path string) error
	
	// Compression operations
	Compress(data []byte) ([]byte, error)
	Decompress(data []byte) ([]byte, error)
	
	// Metadata operations
	GetMetadata(path string) (*Metadata, error)
	SetMetadata(path string, metadata *Metadata) error
	
	// Advanced operations
	List(prefix string) ([]string, error)
	Exists(path string) (bool, error)
	Copy(srcPath, destPath string) error
	
	// Delta operations
	StoreDelta(branchID, projectID string, operations []DeltaOperation) (string, error)
	LoadDelta(deltaPath string) (*DeltaFormat, error)
	ListDeltas(projectID, branchID string) ([]string, error)
}

type Metadata struct {
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type"`
	Compressed   bool   `json:"compressed"`
	Checksum     string `json:"checksum"`
	LastModified string `json:"last_modified"`
}

type service struct {
	config       *config.Config
	backend      CloudBackend
	compressor   Compressor
	deltaManager *DeltaManager
}

func NewService(cfg *config.Config) (Service, error) {
	// Initialize backend based on configuration
	var backend CloudBackend
	var err error
	
	switch cfg.StorageProvider {
	case "s3":
		backend, err = NewS3Backend(cfg.StorageBucket, cfg.AWSRegion)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize S3 backend: %w", err)
		}
	case "local":
		// Use mock backend for development/testing
		backend = &MockBackend{}
	default:
		return nil, fmt.Errorf("unsupported storage provider: %s", cfg.StorageProvider)
	}
	
	// Initialize compressor based on configuration
	compressionType := CompressionZSTD // Default to ZSTD for best compression
	if cfg.CompressionLevel == 0 {
		compressionType = CompressionNone
	}
	
	compressor, err := NewCompressor(compressionType)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize compressor: %w", err)
	}
	
	// Initialize delta manager
	deltaManager := NewDeltaManager(backend, compressor)
	
	return &service{
		config:       cfg,
		backend:      backend,
		compressor:   compressor,
		deltaManager: deltaManager,
	}, nil
}

// Core storage operations with context
func (s *service) Upload(path string, data []byte) error {
	ctx := context.Background()
	
	// Compress data before upload
	compressedData, err := s.compressor.Compress(data)
	if err != nil {
		return fmt.Errorf("failed to compress data: %w", err)
	}
	
	return s.backend.Upload(ctx, path, compressedData)
}

func (s *service) Download(path string) ([]byte, error) {
	ctx := context.Background()
	
	// Download compressed data
	compressedData, err := s.backend.Download(ctx, path)
	if err != nil {
		return nil, err
	}
	
	// Decompress data
	return s.compressor.Decompress(compressedData)
}

func (s *service) Delete(path string) error {
	ctx := context.Background()
	return s.backend.Delete(ctx, path)
}

// Compression operations
func (s *service) Compress(data []byte) ([]byte, error) {
	return s.compressor.Compress(data)
}

func (s *service) Decompress(data []byte) ([]byte, error) {
	return s.compressor.Decompress(data)
}

// Metadata operations
func (s *service) GetMetadata(path string) (*Metadata, error) {
	ctx := context.Background()
	return s.backend.GetMetadata(ctx, path)
}

func (s *service) SetMetadata(path string, metadata *Metadata) error {
	// For cloud storage, metadata is typically set during upload
	// This could be implemented as a separate metadata storage system
	return nil
}

// Advanced operations
func (s *service) List(prefix string) ([]string, error) {
	ctx := context.Background()
	return s.backend.List(ctx, prefix)
}

func (s *service) Exists(path string) (bool, error) {
	ctx := context.Background()
	return s.backend.Exists(ctx, path)
}

func (s *service) Copy(srcPath, destPath string) error {
	ctx := context.Background()
	return s.backend.Copy(ctx, srcPath, destPath)
}

// Delta operations
func (s *service) StoreDelta(branchID, projectID string, operations []DeltaOperation) (string, error) {
	return s.deltaManager.StoreDelta(branchID, projectID, operations)
}

func (s *service) LoadDelta(deltaPath string) (*DeltaFormat, error) {
	return s.deltaManager.LoadDelta(deltaPath)
}

func (s *service) ListDeltas(projectID, branchID string) ([]string, error) {
	return s.deltaManager.ListDeltas(projectID, branchID)
}

// MockBackend for development when cloud storage is not available
type MockBackend struct {
	data map[string][]byte
}

func (m *MockBackend) Upload(ctx context.Context, path string, data []byte) error {
	if m.data == nil {
		m.data = make(map[string][]byte)
	}
	m.data[path] = data
	return nil
}

func (m *MockBackend) Download(ctx context.Context, path string) ([]byte, error) {
	if m.data == nil {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	data, exists := m.data[path]
	if !exists {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	return data, nil
}

func (m *MockBackend) Delete(ctx context.Context, path string) error {
	if m.data != nil {
		delete(m.data, path)
	}
	return nil
}

func (m *MockBackend) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	if m.data != nil {
		for key := range m.data {
			if len(prefix) == 0 || len(key) >= len(prefix) && key[:len(prefix)] == prefix {
				keys = append(keys, key)
			}
		}
	}
	return keys, nil
}

func (m *MockBackend) GetMetadata(ctx context.Context, path string) (*Metadata, error) {
	if m.data == nil {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	data, exists := m.data[path]
	if !exists {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	return &Metadata{
		Size:         int64(len(data)),
		ContentType:  "application/octet-stream",
		Compressed:   true,
		LastModified: "2025-07-17T00:00:00Z",
	}, nil
}

func (m *MockBackend) Exists(ctx context.Context, path string) (bool, error) {
	if m.data == nil {
		return false, nil
	}
	_, exists := m.data[path]
	return exists, nil
}

func (m *MockBackend) Copy(ctx context.Context, srcPath, destPath string) error {
	if m.data == nil {
		return fmt.Errorf("source file not found: %s", srcPath)
	}
	data, exists := m.data[srcPath]
	if !exists {
		return fmt.Errorf("source file not found: %s", srcPath)
	}
	m.data[destPath] = data
	return nil
}