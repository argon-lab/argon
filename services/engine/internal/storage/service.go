package storage

import (
	"argon/engine/internal/config"
)

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
}

type Metadata struct {
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type"`
	Compressed   bool   `json:"compressed"`
	Checksum     string `json:"checksum"`
	LastModified string `json:"last_modified"`
}

type service struct {
	config *config.Config
}

func NewService(cfg *config.Config) Service {
	return &service{config: cfg}
}

// Placeholder implementations - will be fully implemented in Day 2-3

func (s *service) Upload(path string, data []byte) error {
	// TODO: Implement cloud storage upload
	return nil
}

func (s *service) Download(path string) ([]byte, error) {
	// TODO: Implement cloud storage download
	return nil, nil
}

func (s *service) Delete(path string) error {
	// TODO: Implement cloud storage delete
	return nil
}

func (s *service) Compress(data []byte) ([]byte, error) {
	// TODO: Implement compression (LZ4/ZSTD)
	return data, nil
}

func (s *service) Decompress(data []byte) ([]byte, error) {
	// TODO: Implement decompression
	return data, nil
}

func (s *service) GetMetadata(path string) (*Metadata, error) {
	// TODO: Implement metadata retrieval
	return &Metadata{}, nil
}

func (s *service) SetMetadata(path string, metadata *Metadata) error {
	// TODO: Implement metadata storage
	return nil
}