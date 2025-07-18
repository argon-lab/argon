package storage

import (
	"bytes"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// CompressionType represents different compression algorithms
type CompressionType string

const (
	CompressionNone CompressionType = "none"
	CompressionLZ4  CompressionType = "lz4"
	CompressionZSTD CompressionType = "zstd"
)

// Compressor handles data compression and decompression
type Compressor interface {
	Compress(data []byte) ([]byte, error)
	Decompress(data []byte) ([]byte, error)
	Type() CompressionType
}

// LZ4Compressor implements LZ4 compression
type LZ4Compressor struct{}

func NewLZ4Compressor() *LZ4Compressor {
	return &LZ4Compressor{}
}

func (c *LZ4Compressor) Type() CompressionType {
	return CompressionLZ4
}

func (c *LZ4Compressor) Compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := lz4.NewWriter(&buf)
	
	if _, err := writer.Write(data); err != nil {
		return nil, fmt.Errorf("failed to compress with LZ4: %w", err)
	}
	
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close LZ4 writer: %w", err)
	}
	
	return buf.Bytes(), nil
}

func (c *LZ4Compressor) Decompress(data []byte) ([]byte, error) {
	reader := lz4.NewReader(bytes.NewReader(data))
	
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		return nil, fmt.Errorf("failed to decompress with LZ4: %w", err)
	}
	
	return buf.Bytes(), nil
}

// ZSTDCompressor implements ZSTD compression (higher compression ratio)
type ZSTDCompressor struct {
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func NewZSTDCompressor() (*ZSTDCompressor, error) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("failed to create ZSTD encoder: %w", err)
	}
	
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create ZSTD decoder: %w", err)
	}
	
	return &ZSTDCompressor{
		encoder: encoder,
		decoder: decoder,
	}, nil
}

func (c *ZSTDCompressor) Type() CompressionType {
	return CompressionZSTD
}

func (c *ZSTDCompressor) Compress(data []byte) ([]byte, error) {
	return c.encoder.EncodeAll(data, make([]byte, 0, len(data))), nil
}

func (c *ZSTDCompressor) Decompress(data []byte) ([]byte, error) {
	return c.decoder.DecodeAll(data, nil)
}

func (c *ZSTDCompressor) Close() {
	if c.encoder != nil {
		c.encoder.Close()
	}
	if c.decoder != nil {
		c.decoder.Close()
	}
}

// NoCompression implements no compression (passthrough)
type NoCompression struct{}

func NewNoCompression() *NoCompression {
	return &NoCompression{}
}

func (c *NoCompression) Type() CompressionType {
	return CompressionNone
}

func (c *NoCompression) Compress(data []byte) ([]byte, error) {
	return data, nil
}

func (c *NoCompression) Decompress(data []byte) ([]byte, error) {
	return data, nil
}

// NewCompressor creates a compressor based on the specified type
func NewCompressor(compressionType CompressionType) (Compressor, error) {
	switch compressionType {
	case CompressionLZ4:
		return NewLZ4Compressor(), nil
	case CompressionZSTD:
		return NewZSTDCompressor()
	case CompressionNone:
		return NewNoCompression(), nil
	default:
		return nil, fmt.Errorf("unsupported compression type: %s", compressionType)
	}
}