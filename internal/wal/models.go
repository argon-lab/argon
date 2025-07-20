package wal

import (
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// OperationType represents the type of WAL operation
type OperationType string

const (
	OpInsert        OperationType = "insert"
	OpUpdate        OperationType = "update"
	OpDelete        OperationType = "delete"
	OpCreateBranch  OperationType = "create_branch"
	OpDeleteBranch  OperationType = "delete_branch"
	OpCreateProject OperationType = "create_project"
	OpDeleteProject OperationType = "delete_project"
)

// Entry represents a single WAL entry
type Entry struct {
	ID                  primitive.ObjectID     `bson:"_id,omitempty" json:"id,omitempty"`
	LSN                 int64                  `bson:"lsn" json:"lsn"`
	Timestamp           time.Time              `bson:"timestamp" json:"timestamp"`
	ProjectID           string                 `bson:"project_id" json:"project_id"`
	BranchID            string                 `bson:"branch_id" json:"branch_id"`
	Operation           OperationType          `bson:"operation" json:"operation"`
	Collection          string                 `bson:"collection,omitempty" json:"collection,omitempty"`
	DocumentID          string                 `bson:"document_id,omitempty" json:"document_id,omitempty"`
	Document            bson.Raw               `bson:"-" json:"-"`
	OldDocument         bson.Raw               `bson:"-" json:"-"`
	CompressedDocument  []byte                 `bson:"compressed_document,omitempty" json:"-"`
	CompressedOldDocument []byte               `bson:"compressed_old_document,omitempty" json:"-"`
	Metadata            map[string]interface{} `bson:"metadata,omitempty" json:"metadata,omitempty"`
}

// Branch represents a WAL-based branch
type Branch struct {
	ID         string    `bson:"_id" json:"id"`
	ProjectID  string    `bson:"project_id" json:"project_id"`
	Name       string    `bson:"name" json:"name"`
	ParentID   string    `bson:"parent_id,omitempty" json:"parent_id,omitempty"`
	HeadLSN    int64     `bson:"head_lsn" json:"head_lsn"`
	BaseLSN    int64     `bson:"base_lsn" json:"base_lsn"`
	CreatedAt  time.Time `bson:"created_at" json:"created_at"`
	CreatedLSN int64     `bson:"created_lsn" json:"created_lsn"`
	IsDeleted  bool      `bson:"is_deleted" json:"is_deleted"`
}

// Project represents a WAL-enabled project
type Project struct {
	ID           string    `bson:"_id" json:"id"`
	Name         string    `bson:"name" json:"name"`
	MainBranchID string    `bson:"main_branch_id" json:"main_branch_id"`
	CreatedAt    time.Time `bson:"created_at" json:"created_at"`
	UseWAL       bool      `bson:"use_wal" json:"use_wal"`
}
