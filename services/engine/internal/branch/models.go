package branch

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Project represents a database project
type Project struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Name        string             `bson:"name" json:"name"`
	Description string             `bson:"description" json:"description"`
	OwnerID     primitive.ObjectID `bson:"owner_id" json:"owner_id"`
	CreatedAt   time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time          `bson:"updated_at" json:"updated_at"`
}

// Branch represents a database branch
type Branch struct {
	ID           primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	ProjectID    primitive.ObjectID `bson:"project_id" json:"project_id"`
	Name         string             `bson:"name" json:"name"`
	Description  string             `bson:"description" json:"description"`
	ParentBranch *primitive.ObjectID `bson:"parent_branch,omitempty" json:"parent_branch,omitempty"`
	
	// Branch state
	Status       BranchStatus `bson:"status" json:"status"`
	IsMain       bool         `bson:"is_main" json:"is_main"`
	
	// Change tracking
	BaseRevision    string `bson:"base_revision" json:"base_revision"`
	CurrentRevision string `bson:"current_revision" json:"current_revision"`
	
	// Metadata
	CreatedBy   primitive.ObjectID `bson:"created_by" json:"created_by"`
	CreatedAt   time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time          `bson:"updated_at" json:"updated_at"`
	
	// Storage information
	StoragePath   string                 `bson:"storage_path" json:"storage_path"`
	Metadata      map[string]interface{} `bson:"metadata" json:"metadata"`
	
	// Performance tracking
	DocumentCount int64   `bson:"document_count" json:"document_count"`
	StorageSize   int64   `bson:"storage_size" json:"storage_size"`
	LastSyncAt    *time.Time `bson:"last_sync_at,omitempty" json:"last_sync_at,omitempty"`
}

type BranchStatus string

const (
	BranchStatusActive    BranchStatus = "active"
	BranchStatusSuspended BranchStatus = "suspended"
	BranchStatusMerging   BranchStatus = "merging"
	BranchStatusArchived  BranchStatus = "archived"
)

// ChangeEvent represents a change captured from change streams
type ChangeEvent struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	ProjectID   primitive.ObjectID `bson:"project_id" json:"project_id"`
	BranchID    primitive.ObjectID `bson:"branch_id" json:"branch_id"`
	
	// Change details
	OperationType string                 `bson:"operation_type" json:"operation_type"`
	Collection    string                 `bson:"collection" json:"collection"`
	DocumentID    interface{}            `bson:"document_id" json:"document_id"`
	FullDocument  map[string]interface{} `bson:"full_document,omitempty" json:"full_document,omitempty"`
	
	// Change stream metadata
	ResumeToken interface{} `bson:"resume_token" json:"resume_token"`
	Timestamp   time.Time   `bson:"timestamp" json:"timestamp"`
	
	// Storage information
	StoragePath string `bson:"storage_path" json:"storage_path"`
	Compressed  bool   `bson:"compressed" json:"compressed"`
	Size        int64  `bson:"size" json:"size"`
}

// BranchCreateRequest represents a request to create a new branch
type BranchCreateRequest struct {
	ProjectID    primitive.ObjectID  `json:"project_id" binding:"required"`
	Name         string              `json:"name" binding:"required"`
	Description  string              `json:"description"`
	ParentBranch *primitive.ObjectID `json:"parent_branch,omitempty"`
}

// BranchUpdateRequest represents a request to update a branch
type BranchUpdateRequest struct {
	Description *string       `json:"description,omitempty"`
	Status      *BranchStatus `json:"status,omitempty"`
}

// BranchStats represents statistics about a branch
type BranchStats struct {
	DocumentCount   int64     `json:"document_count"`
	StorageSize     int64     `json:"storage_size"`
	ChangeCount     int64     `json:"change_count"`
	LastChangeAt    *time.Time `json:"last_change_at,omitempty"`
	CompressionRatio float64   `json:"compression_ratio"`
}