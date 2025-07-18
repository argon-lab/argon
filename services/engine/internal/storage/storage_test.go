package storage

import (
	"testing"
	"time"

	"argon/engine/internal/config"
)

func TestStorageEngine(t *testing.T) {
	// Create test configuration
	cfg := &config.Config{
		StorageProvider:   "local", // Use mock backend for testing
		StorageBucket:     "test-bucket",
		CompressionLevel:  6,
	}
	
	// Initialize storage service
	service, err := NewService(cfg)
	if err != nil {
		t.Fatalf("Failed to create storage service: %v", err)
	}
	
	// Test data compression
	testData := []byte("Hello, this is test data for compression testing. It should compress well with ZSTD.")
	
	compressed, err := service.Compress(testData)
	if err != nil {
		t.Fatalf("Failed to compress data: %v", err)
	}
	
	if len(compressed) >= len(testData) {
		t.Logf("Warning: Compressed data (%d bytes) is not smaller than original (%d bytes)", 
			len(compressed), len(testData))
	}
	
	// Test decompression
	decompressed, err := service.Decompress(compressed)
	if err != nil {
		t.Fatalf("Failed to decompress data: %v", err)
	}
	
	if string(decompressed) != string(testData) {
		t.Fatalf("Decompressed data does not match original")
	}
	
	// Test storage operations
	testPath := "test/storage/file.txt"
	
	// Upload
	err = service.Upload(testPath, testData)
	if err != nil {
		t.Fatalf("Failed to upload data: %v", err)
	}
	
	// Check existence
	exists, err := service.Exists(testPath)
	if err != nil {
		t.Fatalf("Failed to check file existence: %v", err)
	}
	if !exists {
		t.Fatalf("File should exist after upload")
	}
	
	// Download
	downloadedData, err := service.Download(testPath)
	if err != nil {
		t.Fatalf("Failed to download data: %v", err)
	}
	
	if string(downloadedData) != string(testData) {
		t.Fatalf("Downloaded data does not match original")
	}
	
	// Test metadata
	metadata, err := service.GetMetadata(testPath)
	if err != nil {
		t.Fatalf("Failed to get metadata: %v", err)
	}
	
	if metadata.Size == 0 {
		t.Fatalf("Metadata should have non-zero size")
	}
	
	t.Logf("✅ Storage test passed!")
	t.Logf("   Original size: %d bytes", len(testData))
	t.Logf("   Compressed size: %d bytes", len(compressed))
	t.Logf("   Compression ratio: %.2f%%", float64(len(compressed))/float64(len(testData))*100)
	t.Logf("   Metadata size: %d bytes", metadata.Size)
}

func TestDeltaStorage(t *testing.T) {
	// Create test configuration
	cfg := &config.Config{
		StorageProvider:   "local",
		StorageBucket:     "test-bucket",
		CompressionLevel:  6,
	}
	
	// Initialize storage service
	service, err := NewService(cfg)
	if err != nil {
		t.Fatalf("Failed to create storage service: %v", err)
	}
	
	// Create test delta operations
	operations := []DeltaOperation{
		{
			ID:            "op1",
			OperationType: "insert",
			Collection:    "users",
			DocumentID:    "user123",
			FullDocument: map[string]interface{}{
				"name":  "John Doe",
				"email": "john@example.com",
				"age":   30,
			},
			Timestamp: time.Now(),
		},
		{
			ID:            "op2",
			OperationType: "update",
			Collection:    "users",
			DocumentID:    "user456",
			UpdatedFields: map[string]interface{}{
				"lastLogin": time.Now(),
				"status":    "active",
			},
			Timestamp: time.Now(),
		},
		{
			ID:            "op3",
			OperationType: "delete",
			Collection:    "users",
			DocumentID:    "user789",
			Timestamp:     time.Now(),
		},
	}
	
	// Store delta
	branchID := "branch_test"
	projectID := "project_test"
	
	deltaPath, err := service.StoreDelta(branchID, projectID, operations)
	if err != nil {
		t.Fatalf("Failed to store delta: %v", err)
	}
	
	t.Logf("Delta stored at: %s", deltaPath)
	
	// Load delta
	loadedDelta, err := service.LoadDelta(deltaPath)
	if err != nil {
		t.Fatalf("Failed to load delta: %v", err)
	}
	
	// Verify delta content
	if loadedDelta.BranchID != branchID {
		t.Fatalf("Branch ID mismatch: expected %s, got %s", branchID, loadedDelta.BranchID)
	}
	
	if loadedDelta.ProjectID != projectID {
		t.Fatalf("Project ID mismatch: expected %s, got %s", projectID, loadedDelta.ProjectID)
	}
	
	if len(loadedDelta.Operations) != len(operations) {
		t.Fatalf("Operation count mismatch: expected %d, got %d", 
			len(operations), len(loadedDelta.Operations))
	}
	
	// Verify metadata
	if loadedDelta.Metadata.OperationCount != len(operations) {
		t.Fatalf("Metadata operation count mismatch: expected %d, got %d",
			len(operations), loadedDelta.Metadata.OperationCount)
	}
	
	if loadedDelta.Metadata.CompressionRatio <= 0 || loadedDelta.Metadata.CompressionRatio > 1 {
		t.Fatalf("Invalid compression ratio: %f", loadedDelta.Metadata.CompressionRatio)
	}
	
	// List deltas
	deltas, err := service.ListDeltas(projectID, branchID)
	if err != nil {
		t.Fatalf("Failed to list deltas: %v", err)
	}
	
	if len(deltas) == 0 {
		t.Fatalf("Expected at least one delta file")
	}
	
	t.Logf("✅ Delta storage test passed!")
	t.Logf("   Operations: %d", len(operations))
	t.Logf("   Uncompressed size: %d bytes", loadedDelta.Metadata.UncompressedSize)
	t.Logf("   Compressed size: %d bytes", loadedDelta.Metadata.CompressedSize)
	t.Logf("   Compression ratio: %.2f%%", loadedDelta.Metadata.CompressionRatio*100)
	t.Logf("   Delta files found: %d", len(deltas))
}