package wal

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/compress/snappy"
)

// CompressionType represents the type of compression used
type CompressionType byte

const (
	// CompressionNone means no compression
	CompressionNone CompressionType = 0
	// CompressionGzip uses gzip compression
	CompressionGzip CompressionType = 1
	// CompressionZstd uses zstd compression
	CompressionZstd CompressionType = 2
	// CompressionSnappy uses snappy compression
	CompressionSnappy CompressionType = 3
)

// CompressionConfig contains compression settings
type CompressionConfig struct {
	// Type specifies the compression algorithm
	Type CompressionType
	// MinSize is the minimum size in bytes before compression is applied
	MinSize int
	// Level is the compression level (for algorithms that support it)
	Level int
}

// DefaultCompressionConfig returns the default compression configuration
func DefaultCompressionConfig() *CompressionConfig {
	return &CompressionConfig{
		Type:    CompressionZstd,
		MinSize: 1024, // Only compress documents larger than 1KB
		Level:   3,    // Balanced compression level
	}
}

// Compressor handles compression and decompression of WAL entries
type Compressor struct {
	config     *CompressionConfig
	zstdWriter *zstd.Encoder
	zstdReader *zstd.Decoder
}

// NewCompressor creates a new compressor with the given configuration
func NewCompressor(config *CompressionConfig) (*Compressor, error) {
	if config == nil {
		config = DefaultCompressionConfig()
	}

	c := &Compressor{
		config: config,
	}

	// Initialize zstd encoder/decoder if needed
	if config.Type == CompressionZstd {
		encoder, err := zstd.NewWriter(nil, 
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(config.Level)))
		if err != nil {
			return nil, fmt.Errorf("failed to create zstd encoder: %w", err)
		}
		c.zstdWriter = encoder

		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			return nil, fmt.Errorf("failed to create zstd decoder: %w", err)
		}
		c.zstdReader = decoder
	}

	return c, nil
}

// Compress compresses the given data if it meets the size threshold
func (c *Compressor) Compress(data []byte) ([]byte, error) {
	// Skip compression for small data
	if len(data) < c.config.MinSize {
		return c.wrapData(data, CompressionNone)
	}

	var compressed []byte
	var err error

	switch c.config.Type {
	case CompressionNone:
		return c.wrapData(data, CompressionNone)
	
	case CompressionGzip:
		compressed, err = c.compressGzip(data)
	
	case CompressionZstd:
		compressed, err = c.compressZstd(data)
	
	case CompressionSnappy:
		compressed = c.compressSnappy(data)
	
	default:
		return nil, fmt.Errorf("unknown compression type: %d", c.config.Type)
	}

	if err != nil {
		return nil, err
	}

	// Only use compression if it actually reduces size
	if len(compressed) >= len(data) {
		return c.wrapData(data, CompressionNone)
	}

	return c.wrapData(compressed, c.config.Type)
}

// Decompress decompresses the given data
func (c *Compressor) Decompress(data []byte) ([]byte, error) {
	if len(data) < 5 { // 1 byte type + 4 bytes length
		return nil, fmt.Errorf("invalid compressed data: too short")
	}

	compressionType := CompressionType(data[0])
	dataLen := binary.LittleEndian.Uint32(data[1:5])
	compressedData := data[5:]

	// Validate data length
	if uint32(len(compressedData)) != dataLen {
		return nil, fmt.Errorf("invalid compressed data: length mismatch")
	}

	switch compressionType {
	case CompressionNone:
		return compressedData, nil
	
	case CompressionGzip:
		return c.decompressGzip(compressedData)
	
	case CompressionZstd:
		return c.decompressZstd(compressedData)
	
	case CompressionSnappy:
		return c.decompressSnappy(compressedData)
	
	default:
		return nil, fmt.Errorf("unknown compression type: %d", compressionType)
	}
}

// wrapData adds compression metadata to the data
func (c *Compressor) wrapData(data []byte, compressionType CompressionType) ([]byte, error) {
	// Format: [1 byte type][4 bytes length][data]
	result := make([]byte, 5+len(data))
	result[0] = byte(compressionType)
	binary.LittleEndian.PutUint32(result[1:5], uint32(len(data)))
	copy(result[5:], data)
	return result, nil
}

// compressGzip compresses data using gzip
func (c *Compressor) compressGzip(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buf, c.config.Level)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip writer: %w", err)
	}

	if _, err := writer.Write(data); err != nil {
		writer.Close()
		return nil, fmt.Errorf("failed to write gzip data: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}

	return buf.Bytes(), nil
}

// decompressGzip decompresses gzip data
func (c *Compressor) decompressGzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer reader.Close()

	decompressed, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read gzip data: %w", err)
	}

	return decompressed, nil
}

// compressZstd compresses data using zstd
func (c *Compressor) compressZstd(data []byte) ([]byte, error) {
	if c.zstdWriter == nil {
		return nil, fmt.Errorf("zstd encoder not initialized")
	}
	return c.zstdWriter.EncodeAll(data, nil), nil
}

// decompressZstd decompresses zstd data
func (c *Compressor) decompressZstd(data []byte) ([]byte, error) {
	if c.zstdReader == nil {
		return nil, fmt.Errorf("zstd decoder not initialized")
	}
	decompressed, err := c.zstdReader.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decompression failed: %w", err)
	}
	return decompressed, nil
}

// compressSnappy compresses data using snappy
func (c *Compressor) compressSnappy(data []byte) []byte {
	return snappy.Encode(nil, data)
}

// decompressSnappy decompresses snappy data
func (c *Compressor) decompressSnappy(data []byte) ([]byte, error) {
	return snappy.Decode(nil, data)
}

// CompressEntry compresses the document fields of a WAL entry
func (c *Compressor) CompressEntry(entry *Entry) error {
	// Compress document if present
	if len(entry.Document) > 0 {
		compressed, err := c.Compress(entry.Document)
		if err != nil {
			return fmt.Errorf("failed to compress document: %w", err)
		}
		entry.CompressedDocument = compressed
		entry.Document = nil // Clear original to save space
	}

	// Compress old document if present
	if len(entry.OldDocument) > 0 {
		compressed, err := c.Compress(entry.OldDocument)
		if err != nil {
			return fmt.Errorf("failed to compress old document: %w", err)
		}
		entry.CompressedOldDocument = compressed
		entry.OldDocument = nil // Clear original to save space
	}

	return nil
}

// DecompressEntry decompresses the document fields of a WAL entry
func (c *Compressor) DecompressEntry(entry *Entry) error {
	// Decompress document if present
	if len(entry.CompressedDocument) > 0 {
		decompressed, err := c.Decompress(entry.CompressedDocument)
		if err != nil {
			return fmt.Errorf("failed to decompress document: %w", err)
		}
		entry.Document = decompressed
		entry.CompressedDocument = nil // Clear compressed to save memory
	}

	// Decompress old document if present
	if len(entry.CompressedOldDocument) > 0 {
		decompressed, err := c.Decompress(entry.CompressedOldDocument)
		if err != nil {
			return fmt.Errorf("failed to decompress old document: %w", err)
		}
		entry.OldDocument = decompressed
		entry.CompressedOldDocument = nil // Clear compressed to save memory
	}

	// For backward compatibility - if no compressed fields but document fields exist,
	// the entry was stored without compression
	// Nothing to do in this case - documents are already in the right place

	return nil
}

// GetCompressionRatio returns the compression ratio for the given data
func (c *Compressor) GetCompressionRatio(original, compressed []byte) float64 {
	if len(original) == 0 {
		return 0
	}
	return 1.0 - (float64(len(compressed)) / float64(len(original)))
}

// Close cleans up any resources used by the compressor
func (c *Compressor) Close() error {
	if c.zstdWriter != nil {
		c.zstdWriter.Close()
	}
	return nil
}