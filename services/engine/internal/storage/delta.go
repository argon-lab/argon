package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// DeltaFormat represents the structure of delta files stored in object storage
type DeltaFormat struct {
	Version     string           `json:"version"`
	BranchID    string           `json:"branch_id"`
	ProjectID   string           `json:"project_id"`
	Timestamp   time.Time        `json:"timestamp"`
	Operations  []DeltaOperation `json:"operations"`
	Metadata    DeltaMetadata    `json:"metadata"`
	Compression string           `json:"compression"`
}

// DeltaOperation represents a single database operation in a delta
type DeltaOperation struct {
	ID            string                 `json:"id"`
	OperationType string                 `json:"operation_type"` // insert, update, delete, replace
	Collection    string                 `json:"collection"`
	DocumentID    interface{}            `json:"document_id"`
	FullDocument  map[string]interface{} `json:"full_document,omitempty"`
	UpdatedFields map[string]interface{} `json:"updated_fields,omitempty"`
	RemovedFields []string               `json:"removed_fields,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
	ResumeToken   interface{}            `json:"resume_token,omitempty"`
}

// DeltaMetadata contains metadata about the delta file
type DeltaMetadata struct {
	OperationCount   int    `json:"operation_count"`
	UncompressedSize int64  `json:"uncompressed_size"`
	CompressedSize   int64  `json:"compressed_size"`
	CompressionRatio float64 `json:"compression_ratio"`
	Checksum         string `json:"checksum"`
	ParentDelta      string `json:"parent_delta,omitempty"`
}

// DeltaManager handles delta file creation, storage, and retrieval
type DeltaManager struct {
	backend    CloudBackend
	compressor Compressor
}

// NewDeltaManager creates a new delta manager
func NewDeltaManager(backend CloudBackend, compressor Compressor) *DeltaManager {
	return &DeltaManager{
		backend:    backend,
		compressor: compressor,
	}
}

// StoreDelta stores a delta file in object storage
func (dm *DeltaManager) StoreDelta(branchID, projectID string, operations []DeltaOperation) (string, error) {
	// Create delta structure
	delta := DeltaFormat{
		Version:     "1.0",
		BranchID:    branchID,
		ProjectID:   projectID,
		Timestamp:   time.Now().UTC(),
		Operations:  operations,
		Compression: string(dm.compressor.Type()),
	}
	
	// Serialize to JSON
	jsonData, err := json.Marshal(delta)
	if err != nil {
		return "", fmt.Errorf("failed to marshal delta: %w", err)
	}
	
	uncompressedSize := int64(len(jsonData))
	
	// Compress the data
	compressedData, err := dm.compressor.Compress(jsonData)
	if err != nil {
		return "", fmt.Errorf("failed to compress delta: %w", err)
	}
	
	compressedSize := int64(len(compressedData))
	compressionRatio := float64(compressedSize) / float64(uncompressedSize)
	
	// Update metadata
	delta.Metadata = DeltaMetadata{
		OperationCount:   len(operations),
		UncompressedSize: uncompressedSize,
		CompressedSize:   compressedSize,
		CompressionRatio: compressionRatio,
		Checksum:         calculateChecksum(compressedData),
	}
	
	// Re-marshal with metadata
	jsonData, err = json.Marshal(delta)
	if err != nil {
		return "", fmt.Errorf("failed to marshal delta with metadata: %w", err)
	}
	
	// Compress again with metadata
	compressedData, err = dm.compressor.Compress(jsonData)
	if err != nil {
		return "", fmt.Errorf("failed to compress delta with metadata: %w", err)
	}
	
	// Generate storage path
	deltaPath := fmt.Sprintf("projects/%s/branches/%s/deltas/%d.delta", 
		projectID, branchID, time.Now().Unix())
	
	// Upload to storage
	ctx := context.Background()
	if err := dm.backend.Upload(ctx, deltaPath, compressedData); err != nil {
		return "", fmt.Errorf("failed to upload delta: %w", err)
	}
	
	return deltaPath, nil
}

// LoadDelta loads a delta file from object storage
func (dm *DeltaManager) LoadDelta(deltaPath string) (*DeltaFormat, error) {
	// Download from storage
	ctx := context.Background()
	compressedData, err := dm.backend.Download(ctx, deltaPath)
	if err != nil {
		return nil, fmt.Errorf("failed to download delta: %w", err)
	}
	
	// Decompress the data
	jsonData, err := dm.compressor.Decompress(compressedData)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress delta: %w", err)
	}
	
	// Unmarshal delta
	var delta DeltaFormat
	if err := json.Unmarshal(jsonData, &delta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal delta: %w", err)
	}
	
	return &delta, nil
}

// ListDeltas lists all delta files for a branch
func (dm *DeltaManager) ListDeltas(projectID, branchID string) ([]string, error) {
	prefix := fmt.Sprintf("projects/%s/branches/%s/deltas/", projectID, branchID)
	ctx := context.Background()
	return dm.backend.List(ctx, prefix)
}

// CreateDeltaFromChangeEvents converts MongoDB change events to delta operations
func CreateDeltaFromChangeEvents(events []interface{}) ([]DeltaOperation, error) {
	var operations []DeltaOperation
	
	for _, event := range events {
		changeEvent, ok := event.(map[string]interface{})
		if !ok {
			continue
		}
		
		operation := DeltaOperation{
			ID:            primitive.NewObjectID().Hex(),
			Timestamp:     time.Now(),
		}
		
		// Extract operation type
		if opType, exists := changeEvent["operationType"]; exists {
			operation.OperationType = opType.(string)
		}
		
		// Extract collection name
		if ns, exists := changeEvent["ns"].(map[string]interface{}); exists {
			if coll, exists := ns["coll"]; exists {
				operation.Collection = coll.(string)
			}
		}
		
		// Extract document ID
		if docKey, exists := changeEvent["documentKey"]; exists {
			operation.DocumentID = docKey
		}
		
		// Extract full document for inserts and replaces
		if fullDoc, exists := changeEvent["fullDocument"]; exists {
			operation.FullDocument = fullDoc.(map[string]interface{})
		}
		
		// Extract updated fields for updates
		if updateDesc, exists := changeEvent["updateDescription"].(map[string]interface{}); exists {
			if updatedFields, exists := updateDesc["updatedFields"]; exists {
				operation.UpdatedFields = updatedFields.(map[string]interface{})
			}
			if removedFields, exists := updateDesc["removedFields"]; exists {
				if fields, ok := removedFields.([]interface{}); ok {
					for _, field := range fields {
						operation.RemovedFields = append(operation.RemovedFields, field.(string))
					}
				}
			}
		}
		
		// Extract resume token
		if token, exists := changeEvent["_id"]; exists {
			operation.ResumeToken = token
		}
		
		operations = append(operations, operation)
	}
	
	return operations, nil
}

// calculateChecksum calculates a simple checksum for data integrity
func calculateChecksum(data []byte) string {
	// Simple checksum implementation - in production, use SHA256 or similar
	sum := uint32(0)
	for _, b := range data {
		sum += uint32(b)
	}
	return fmt.Sprintf("%x", sum)
}