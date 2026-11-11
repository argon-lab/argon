// Package snapshot implements collection snapshots — the "image layer" that
// bounds replay depth. A snapshot captures the fully materialized state of
// one collection on one branch at one LSN; materialization then becomes
// "load nearest snapshot at or below the target, replay only the delta"
// instead of replaying from the branch root.
package snapshot

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CurrentVisibilityRules is recorded on manifests built with effective
// discarded ranges (wal.Branch.EffectiveDiscardedRanges).
const CurrentVisibilityRules = 2

// Snapshot is the manifest of one collection snapshot. The document data
// itself lives in content-addressed chunks (see ChunkStore); the manifest
// references them by hash, so identical chunks are shared across snapshots.
type Snapshot struct {
	ID         primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID  string             `bson:"project_id" json:"project_id"`
	BranchID   string             `bson:"branch_id" json:"branch_id"`
	Collection string             `bson:"collection" json:"collection"`

	// LSN is the position this snapshot reflects: the state equals a full
	// ancestry replay of the collection up to and including this LSN.
	LSN int64 `bson:"lsn" json:"lsn"`

	// RangesApplied is how many of the branch's discarded ranges existed
	// when this snapshot was built. Discarded ranges are append-only, so
	// the first RangesApplied ranges are the ones the build saw. Seeing a
	// range is not the same as excluding it: a snapshot taken at an LSN
	// inside a discarded window reads the pre-reset history, exactly like
	// any other reader at that position. Service.usable reconstructs what
	// the build excluded from this count, the snapshot LSN and the ranges.
	// Reset itself stays completely unaware of snapshots.
	RangesApplied int `bson:"ranges_applied" json:"ranges_applied"`

	// VisibilityRules identifies how the build interpreted discarded
	// ranges. Manifests without it were written by releases up to 2.1.5,
	// which took each range's stored end literally; current builds use the
	// effective (high-water) ends and record CurrentVisibilityRules.
	VisibilityRules int `bson:"visibility_rules,omitempty" json:"visibility_rules,omitempty"`

	ChunkIDs  []string  `bson:"chunk_ids" json:"chunk_ids"`
	DocCount  int64     `bson:"doc_count" json:"doc_count"`
	SizeBytes int64     `bson:"size_bytes" json:"size_bytes"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}
