package wal

import (
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// OperationType represents the type of WAL operation
type OperationType string

const (
	// Data operations. Every data entry is a self-contained physical
	// record: OpPut carries the complete post-image of one document,
	// OpDelete removes one document by ID. Replay is deterministic by
	// construction because no filters or update operators are ever
	// re-executed — the outcome of the original operation is what gets
	// logged.
	OpPut    OperationType = "put"
	OpDelete OperationType = "delete"

	// Control operations.
	OpCreateBranch  OperationType = "create_branch"
	OpDeleteBranch  OperationType = "delete_branch"
	OpCreateProject OperationType = "create_project"
	OpDeleteProject OperationType = "delete_project"
)

// Legacy schema-v1 data operations. v1 update/delete entries stored the
// original filter and update expressions and re-executed them on replay,
// which is not deterministic. They can be read for migration but are
// rejected by the materializer.
const (
	LegacyOpInsert OperationType = "insert"
	LegacyOpUpdate OperationType = "update"
	LegacyOpDelete OperationType = "delete"
)

// EntrySchemaVersion identifies entries written by the current code.
// Entries with a lower (or missing) version predate the physical-log format
// and must be migrated before they can be replayed.
const EntrySchemaVersion = 2

// Entry represents a single WAL entry
type Entry struct {
	ID            primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	SchemaVersion int                `bson:"v,omitempty" json:"v,omitempty"`
	LSN           int64              `bson:"lsn" json:"lsn"`
	Timestamp     time.Time          `bson:"timestamp" json:"timestamp"`
	ProjectID     string             `bson:"project_id" json:"project_id"`
	BranchID      string             `bson:"branch_id" json:"branch_id"`
	Operation     OperationType      `bson:"operation" json:"operation"`
	Collection    string             `bson:"collection,omitempty" json:"collection,omitempty"`
	DocumentID    string             `bson:"document_id,omitempty" json:"document_id,omitempty"`

	// PostImage is the complete document after the operation. Required on
	// every put; replaying a put is simply state[DocumentID] = PostImage.
	PostImage bson.Raw `bson:"-" json:"-"`
	// PreImage is the complete document the operation replaced (puts over
	// an existing document, and deletes). It is never needed to
	// materialize state — it exists to power diff, undo and audit.
	PreImage bson.Raw `bson:"-" json:"-"`

	// Compressed forms are what is actually stored; the raw images above
	// are populated on read and cleared on write by the compressor.
	CompressedPostImage []byte `bson:"post,omitempty" json:"-"`
	CompressedPreImage  []byte `bson:"pre,omitempty" json:"-"`

	// TxnID groups entries that must become visible atomically.
	TxnID string `bson:"txn_id,omitempty" json:"txn_id,omitempty"`
	// Actor identifies who produced the write, e.g. "user:jake" or
	// "agent:session-42". Powers per-session undo and audit trails.
	Actor string `bson:"actor,omitempty" json:"actor,omitempty"`

	Metadata map[string]interface{} `bson:"metadata,omitempty" json:"metadata,omitempty"`
}

// IsLegacy reports whether the entry predates the physical-log schema and
// therefore cannot be replayed deterministically. Note that v1 and v2 both
// use the operation name "delete"; the schema version disambiguates (v1
// deletes carried a filter, v2 deletes carry a document ID and pre-image).
func (e *Entry) IsLegacy() bool {
	switch e.Operation {
	case LegacyOpInsert, LegacyOpUpdate:
		return true
	case OpDelete:
		return e.SchemaVersion < EntrySchemaVersion
	default:
		return false
	}
}

// ValidateForAppend checks the invariants the WAL enforces at its write
// boundary. Catching malformed entries here keeps every downstream consumer
// (materializer, time travel, diff) free of defensive special cases.
func (e *Entry) ValidateForAppend() error {
	if e.ProjectID == "" {
		return fmt.Errorf("WAL entry requires a project ID")
	}
	switch e.Operation {
	case OpPut:
		if e.BranchID == "" || e.Collection == "" || e.DocumentID == "" {
			return fmt.Errorf("put entry requires branch, collection and document ID")
		}
		if len(e.PostImage) == 0 {
			return fmt.Errorf("put entry for document %s requires a post-image", e.DocumentID)
		}
	case OpDelete:
		if e.BranchID == "" || e.Collection == "" || e.DocumentID == "" {
			return fmt.Errorf("delete entry requires branch, collection and document ID")
		}
	case OpCreateBranch, OpDeleteBranch, OpCreateProject, OpDeleteProject:
		// Control entries carry their payload in Metadata.
	case LegacyOpInsert, LegacyOpUpdate:
		return fmt.Errorf("operation %q is a legacy schema-v1 operation and can no longer be written", e.Operation)
	default:
		return fmt.Errorf("unknown WAL operation %q", e.Operation)
	}
	return nil
}

// LSNRange is a closed interval of LSNs.
type LSNRange struct {
	From int64 `bson:"from" json:"from"`
	To   int64 `bson:"to" json:"to"`
}

// Contains reports whether the LSN falls inside the range.
func (r LSNRange) Contains(lsn int64) bool {
	return lsn >= r.From && lsn <= r.To
}

// Branch represents a WAL-based branch. A branch is a pointer into the WAL:
// BaseLSN is the fork point (the parent's LSN the branch started from) and
// HeadLSN is the newest entry that belongs to the branch. Entries at or
// below BaseLSN are inherited from the ancestry chain via ParentID.
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

	// DiscardedRanges records LSN windows abandoned by restore/reset
	// operations. The entries stay in the WAL for audit, but materialization
	// skips them; without this, the first write after a reset (whose LSN is
	// necessarily higher than the discarded entries') would advance the head
	// past the discarded window and resurrect it.
	DiscardedRanges []LSNRange `bson:"discarded_ranges,omitempty" json:"discarded_ranges,omitempty"`
}

// IsDiscardedForRead reports whether an LSN must be skipped when reading
// this branch's entries up to upperBound (the segment's inclusive end: the
// head for a self-read, the fork LSN for a child's read).
//
// A discarded range only applies when the read extends past it. The
// sequencer never rewinds, so after a reset to T (discarding [from, to],
// with T < from) any later write or fork point lands strictly above `to` —
// while a child forked before the reset has its fork LSN at or below `to`.
// That makes "upperBound > to" exactly the "this reader's timeline includes
// the reset" test: post-reset readers skip the window, and children that
// legitimately captured the pre-reset history keep it.
func (b *Branch) IsDiscardedForRead(lsn, upperBound int64) bool {
	for _, r := range b.DiscardedRanges {
		if r.Contains(lsn) && upperBound > r.To {
			return true
		}
	}
	return false
}

// IsData reports whether the entry carries document state (as opposed to
// branch/project control records).
func (e *Entry) IsData() bool {
	switch e.Operation {
	case OpPut, OpDelete, LegacyOpInsert, LegacyOpUpdate:
		return true
	default:
		return false
	}
}

// Project represents a WAL-enabled project
type Project struct {
	ID           string    `bson:"_id" json:"id"`
	Name         string    `bson:"name" json:"name"`
	MainBranchID string    `bson:"main_branch_id" json:"main_branch_id"`
	CreatedAt    time.Time `bson:"created_at" json:"created_at"`
	UseWAL       bool      `bson:"use_wal" json:"use_wal"`
}
