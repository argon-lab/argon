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
	// OpMerge records that another branch's changes were merged in; the
	// data itself arrives as ordinary puts/deletes, this entry is the
	// audit marker carrying the plan metadata.
	OpMerge OperationType = "merge"
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
	ID                 primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	SchemaVersion      int                `bson:"v,omitempty" json:"v,omitempty"`
	LSN                int64              `bson:"lsn" json:"lsn"`
	Timestamp          time.Time          `bson:"timestamp" json:"timestamp"`
	ProjectID          string             `bson:"project_id" json:"project_id"`
	BranchID           string             `bson:"branch_id" json:"branch_id"`
	Operation          OperationType      `bson:"operation" json:"operation"`
	Collection         string             `bson:"collection,omitempty" json:"collection,omitempty"`
	DocumentID         string             `bson:"document_id,omitempty" json:"document_id,omitempty"`
	DocumentKeyVersion int                `bson:"key_v,omitempty" json:"key_v,omitempty"`

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
	case OpCreateBranch, OpDeleteBranch, OpCreateProject, OpDeleteProject, OpMerge:
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
	ID              string    `bson:"_id" json:"id"`
	ProjectID       string    `bson:"project_id" json:"project_id"`
	Name            string    `bson:"name" json:"name"`
	ParentID        string    `bson:"parent_id,omitempty" json:"parent_id,omitempty"`
	HeadLSN         int64     `bson:"head_lsn" json:"head_lsn"`
	BaseLSN         int64     `bson:"base_lsn" json:"base_lsn"`
	CreatedAt       time.Time `bson:"created_at" json:"created_at"`
	CreatedLSN      int64     `bson:"created_lsn" json:"created_lsn"`
	IsDeleted       bool      `bson:"is_deleted" json:"is_deleted"`
	MutationVersion int64     `bson:"mutation_version,omitempty" json:"mutation_version,omitempty"`

	// DiscardedRanges records LSN windows abandoned by restore/reset
	// operations. The entries stay in the WAL for audit, but materialization
	// skips them; without this, the first write after a reset (whose LSN is
	// necessarily higher than the discarded entries') would advance the head
	// past the discarded window and resurrect it.
	//
	// Readers must not interpret the stored ends directly: use
	// IsDiscardedForRead or EffectiveDiscardedRanges, which apply the
	// high-water rule described there.
	DiscardedRanges []LSNRange `bson:"discarded_ranges,omitempty" json:"discarded_ranges,omitempty"`

	// Checkout state (mongod-as-compute). A checked-out ("live") branch is
	// materialized into a real MongoDB database that applications connect
	// to directly; the WAL is fed from its change stream instead of the
	// SDK write path.
	PhysicalDB    string `bson:"physical_db,omitempty" json:"physical_db,omitempty"`
	State         string `bson:"state,omitempty" json:"state,omitempty"` // "" | BranchStateLive
	CheckedOutLSN int64  `bson:"checked_out_lsn,omitempty" json:"checked_out_lsn,omitempty"`

	// ExpiresAt marks an ephemeral sandbox branch: past this instant, a
	// sweep releases and deletes it (storage reclaimed through the delete
	// hook). Merge or discard it before then — or extend it.
	ExpiresAt *time.Time `bson:"expires_at,omitempty" json:"expires_at,omitempty"`
}

// IsExpired reports whether a sandbox branch has passed its TTL.
func (b *Branch) IsExpired(now time.Time) bool {
	return b.ExpiresAt != nil && now.After(*b.ExpiresAt)
}

// BranchStateLive marks a branch as checked out into a physical database.
const BranchStateLive = "live"

// IsLive reports whether the branch is checked out into a physical
// database (writes flow through mongod and the change-stream ingester, not
// the SDK interceptor).
func (b *Branch) IsLive() bool {
	return b.State == BranchStateLive
}

// IsDiscardedForRead reports whether an LSN must be skipped when reading
// this branch's entries up to upperBound (the segment's inclusive end: the
// head for a self-read, the fork LSN for a child's read, the pinned LSN for
// a pin).
//
// A discarded range only applies when the read extends past it. The
// sequencer never rewinds, so after a reset to T that discards (T, M] —
// where M is the highest LSN the branch's history had reached — any later
// write or fork point lands strictly above M, while every view captured
// before the reset (a child's fork point, a pin, a backup branch) sits at or
// below M. That makes "upperBound > M" exactly the "this reader's timeline
// includes the reset" test: post-reset readers skip the window, and views
// that legitimately captured the pre-reset history keep all of it.
//
// M is the high-water mark, not the head at the time of the reset. The two
// differ when a branch is reset twice with no write in between: the head is
// then already below the first window. A range therefore always ends at the
// highest end recorded so far, which is what EffectiveDiscardedRanges
// computes; releases up to 2.1.5 stored the head instead, and those ranges
// are read here with their corrected end.
func (b *Branch) IsDiscardedForRead(lsn, upperBound int64) bool {
	var highWater int64
	for _, r := range b.DiscardedRanges {
		if r.To > highWater {
			highWater = r.To
		}
		if lsn >= r.From && lsn <= highWater && upperBound > highWater {
			return true
		}
	}
	return false
}

// EffectiveDiscardedRanges returns the discarded ranges as readers interpret
// them, in recording order: each range ends at the highest end recorded up
// to and including it (see IsDiscardedForRead). The i-th element describes
// the i-th stored range, and never changes once recorded, because ranges
// are append-only.
func (b *Branch) EffectiveDiscardedRanges() []LSNRange {
	if len(b.DiscardedRanges) == 0 {
		return nil
	}
	ranges := make([]LSNRange, len(b.DiscardedRanges))
	var highWater int64
	for i, r := range b.DiscardedRanges {
		if r.To > highWater {
			highWater = r.To
		}
		ranges[i] = LSNRange{From: r.From, To: highWater}
	}
	return ranges
}

// HistoryHighWater is the highest LSN this branch's own history has ever
// reached: the head, or the end of a discarded window above it. LSNs in
// (HeadLSN, HistoryHighWater] were abandoned by a reset but remain readable
// as history — forks and pins captured there keep working, and they can be
// forked again to recover what a reset discarded.
func (b *Branch) HistoryHighWater() int64 {
	highWater := b.HeadLSN
	for _, r := range b.DiscardedRanges {
		if r.To > highWater {
			highWater = r.To
		}
	}
	return highWater
}

// ValidateResetTarget refuses to rewind the head into history that an
// earlier reset already discarded. Such a position is not on the branch's
// current timeline: making it the head would show the discarded writes
// again until the next write, which lands above the old window and hides
// them once more. The history is still there to be forked.
func (b *Branch) ValidateResetTarget(target int64) error {
	ranges := b.EffectiveDiscardedRanges()
	inside := false
	valid := target
	for moved := true; moved; {
		moved = false
		for _, r := range ranges {
			if r.Contains(valid) {
				valid = r.From - 1
				inside, moved = true, true
			}
		}
	}
	if !inside {
		return nil
	}
	return fmt.Errorf("LSN %d is in history that an earlier reset discarded; reset to LSN %d or earlier, or fork that history into a new branch (restore branch at LSN %d) to recover it", target, valid, target)
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
	Importing    bool      `bson:"importing,omitempty" json:"importing,omitempty"`
}
