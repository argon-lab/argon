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
	// (and were therefore honored) when this snapshot was built. Discarded
	// ranges are append-only, so a reader can tell whether a later reset
	// invalidated this snapshot by looking only at ranges with index >=
	// RangesApplied. This keeps reset itself completely unaware of
	// snapshots.
	RangesApplied int `bson:"ranges_applied" json:"ranges_applied"`

	ChunkIDs  []string  `bson:"chunk_ids" json:"chunk_ids"`
	DocCount  int64     `bson:"doc_count" json:"doc_count"`
	SizeBytes int64     `bson:"size_bytes" json:"size_bytes"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}
