package wal_test

import (
	"bytes"
	"testing"

	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestCompressor_Basic(t *testing.T) {
	tests := []struct {
		name string
		config *wal.CompressionConfig
	}{
		{
			name: "Gzip compression",
			config: &wal.CompressionConfig{
				Type:    wal.CompressionGzip,
				MinSize: 10,
				Level:   5,
			},
		},
		{
			name: "Zstd compression",
			config: &wal.CompressionConfig{
				Type:    wal.CompressionZstd,
				MinSize: 10,
				Level:   3,
			},
		},
		{
			name: "Snappy compression",
			config: &wal.CompressionConfig{
				Type:    wal.CompressionSnappy,
				MinSize: 10,
				Level:   0, // Snappy doesn't use levels
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compressor, err := wal.NewCompressor(tt.config)
			require.NoError(t, err)
			defer compressor.Close()

			// Test data that should be compressed
			testData := bytes.Repeat([]byte("Hello World! "), 100)
			
			// Compress
			compressed, err := compressor.Compress(testData)
			require.NoError(t, err)
			assert.NotEqual(t, testData, compressed)
			assert.Less(t, len(compressed), len(testData))

			// Decompress
			decompressed, err := compressor.Decompress(compressed)
			require.NoError(t, err)
			assert.Equal(t, testData, decompressed)

			// Check compression ratio
			ratio := compressor.GetCompressionRatio(testData, compressed)
			assert.Greater(t, ratio, 0.5) // Should achieve at least 50% compression
		})
	}
}

func TestCompressor_SmallData(t *testing.T) {
	config := &wal.CompressionConfig{
		Type:    wal.CompressionZstd,
		MinSize: 1024, // 1KB minimum
		Level:   3,
	}

	compressor, err := wal.NewCompressor(config)
	require.NoError(t, err)
	defer compressor.Close()

	// Small data should not be compressed
	smallData := []byte("Small data")
	compressed, err := compressor.Compress(smallData)
	require.NoError(t, err)

	// Should have compression type None
	assert.Equal(t, byte(wal.CompressionNone), compressed[0])

	// Decompress should still work
	decompressed, err := compressor.Decompress(compressed)
	require.NoError(t, err)
	assert.Equal(t, smallData, decompressed)
}

func TestCompressor_Entry(t *testing.T) {
	compressor, err := wal.NewCompressor(nil) // Use default config
	require.NoError(t, err)
	defer compressor.Close()

	// Create test entry with large documents
	largeDoc := bson.M{
		"data": bytes.Repeat([]byte("test data "), 200),
		"field1": "value1",
		"field2": "value2",
	}
	
	docBytes, err := bson.Marshal(largeDoc)
	require.NoError(t, err)

	entry := &wal.Entry{
		ProjectID:  "test-project",
		BranchID:   "main",
		Operation:  wal.OpUpdate,
		Collection: "test",
		DocumentID: "doc1",
		Document:   docBytes,
		OldDocument: docBytes, // Use same for old document
	}

	// Get original sizes
	origDocSize := len(entry.Document)
	origOldDocSize := len(entry.OldDocument)

	// Compress entry
	err = compressor.CompressEntry(entry)
	require.NoError(t, err)

	// Verify compression happened
	assert.Less(t, len(entry.CompressedDocument), origDocSize)
	assert.Less(t, len(entry.CompressedOldDocument), origOldDocSize)

	// Verify compression happened
	assert.NotNil(t, entry.CompressedDocument)
	assert.Nil(t, entry.Document)
	
	// Store compressed for verification
	compressedDoc := make([]byte, len(entry.CompressedDocument))
	copy(compressedDoc, entry.CompressedDocument)

	// Decompress entry
	err = compressor.DecompressEntry(entry)
	require.NoError(t, err)

	// Debug output
	t.Logf("Original doc size: %d bytes", len(docBytes))
	t.Logf("Compressed doc size: %d bytes", len(compressedDoc))
	t.Logf("Decompressed doc size: %d bytes", len(entry.Document))
	t.Logf("First few bytes of original: %x", docBytes[:20])
	if len(entry.Document) >= 20 {
		t.Logf("First few bytes of decompressed: %x", []byte(entry.Document)[:20])
	} else {
		t.Logf("Decompressed data too short: %x", []byte(entry.Document))
	}
	t.Logf("Compression type in metadata: %d", compressedDoc[0])
	
	// Verify decompression restored data
	assert.NotNil(t, entry.Document)
	assert.Nil(t, entry.CompressedDocument)

	// Verify decompression
	assert.Equal(t, docBytes, []byte(entry.Document))
	assert.Equal(t, docBytes, []byte(entry.OldDocument))
	
	// Verify it was actually compressed (should have metadata prefix)
	assert.True(t, compressedDoc[0] == byte(wal.CompressionZstd) || 
		compressedDoc[0] == byte(wal.CompressionGzip) ||
		compressedDoc[0] == byte(wal.CompressionSnappy))
}

func TestCompressor_EdgeCases(t *testing.T) {
	compressor, err := wal.NewCompressor(nil)
	require.NoError(t, err)
	defer compressor.Close()

	t.Run("Empty data", func(t *testing.T) {
		compressed, err := compressor.Compress([]byte{})
		require.NoError(t, err)
		
		decompressed, err := compressor.Decompress(compressed)
		require.NoError(t, err)
		assert.Empty(t, decompressed)
	})

	t.Run("Invalid compressed data", func(t *testing.T) {
		// Too short
		_, err := compressor.Decompress([]byte{1, 2, 3})
		assert.Error(t, err)

		// Invalid length
		invalidData := []byte{byte(wal.CompressionZstd), 0, 0, 0, 10, 1, 2, 3}
		_, err = compressor.Decompress(invalidData)
		assert.Error(t, err)
	})

	t.Run("Unknown compression type", func(t *testing.T) {
		invalidData := []byte{99, 0, 0, 0, 0} // Type 99 doesn't exist
		_, err := compressor.Decompress(invalidData)
		assert.Error(t, err)
	})
}

func TestCompressor_Performance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	configs := []struct {
		name string
		config *wal.CompressionConfig
	}{
		{
			name:   "No compression",
			config: &wal.CompressionConfig{Type: wal.CompressionNone},
		},
		{
			name:   "Gzip",
			config: &wal.CompressionConfig{Type: wal.CompressionGzip, Level: 5},
		},
		{
			name:   "Zstd",
			config: &wal.CompressionConfig{Type: wal.CompressionZstd, Level: 3},
		},
		{
			name:   "Snappy",
			config: &wal.CompressionConfig{Type: wal.CompressionSnappy},
		},
	}

	// Create test data - typical MongoDB document
	testDoc := bson.M{
		"_id":        "507f1f77bcf86cd799439011",
		"username":   "johndoe",
		"email":      "john.doe@example.com",
		"profile": bson.M{
			"firstName": "John",
			"lastName":  "Doe",
			"age":       30,
			"address": bson.M{
				"street":  "123 Main St",
				"city":    "New York",
				"state":   "NY",
				"zipCode": "10001",
			},
		},
		"preferences": bson.M{
			"newsletter": true,
			"theme":      "dark",
			"language":   "en",
		},
		"data": bytes.Repeat([]byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit. "), 50),
	}

	docBytes, err := bson.Marshal(testDoc)
	require.NoError(t, err)

	t.Logf("Original document size: %d bytes", len(docBytes))

	for _, tc := range configs {
		t.Run(tc.name, func(t *testing.T) {
			tc.config.MinSize = 0 // Always compress for testing
			
			compressor, err := wal.NewCompressor(tc.config)
			require.NoError(t, err)
			defer compressor.Close()

			// Measure compression
			compressed, err := compressor.Compress(docBytes)
			require.NoError(t, err)

			ratio := compressor.GetCompressionRatio(docBytes, compressed)
			t.Logf("Compressed size: %d bytes (%.1f%% reduction)", 
				len(compressed), ratio*100)

			// Verify decompression
			decompressed, err := compressor.Decompress(compressed)
			require.NoError(t, err)
			assert.Equal(t, docBytes, decompressed)
		})
	}
}

func BenchmarkCompression(b *testing.B) {
	// Create realistic test data
	testDoc := bson.M{
		"data": bytes.Repeat([]byte("test data "), 1000),
		"field1": "value1",
		"field2": "value2",
		"nested": bson.M{
			"subfield1": "subvalue1",
			"subfield2": 12345,
		},
	}
	
	docBytes, _ := bson.Marshal(testDoc)

	benchmarks := []struct {
		name string
		config *wal.CompressionConfig
	}{
		{"Gzip", &wal.CompressionConfig{Type: wal.CompressionGzip, Level: 5}},
		{"Zstd", &wal.CompressionConfig{Type: wal.CompressionZstd, Level: 3}},
		{"Snappy", &wal.CompressionConfig{Type: wal.CompressionSnappy}},
	}

	for _, bm := range benchmarks {
		compressor, _ := wal.NewCompressor(bm.config)
		
		b.Run(bm.name+"_Compress", func(b *testing.B) {
			b.SetBytes(int64(len(docBytes)))
			b.ResetTimer()
			
			for i := 0; i < b.N; i++ {
				_, err := compressor.Compress(docBytes)
				if err != nil {
					b.Fatal(err)
				}
			}
		})

		compressed, _ := compressor.Compress(docBytes)
		
		b.Run(bm.name+"_Decompress", func(b *testing.B) {
			b.SetBytes(int64(len(compressed)))
			b.ResetTimer()
			
			for i := 0; i < b.N; i++ {
				_, err := compressor.Decompress(compressed)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
		
		compressor.Close()
	}
}