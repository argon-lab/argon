// Package walwriter is the programmatic write path for branches that are
// not checked out: imports, merges, undos, seeding and tests. It appends
// explicit document states — callers say exactly what a document becomes —
// and never evaluates filters or update operators. Applications work with
// checked-out branches through real MongoDB drivers instead; the change
// stream feeds their WAL (see the ingest package).
package walwriter

import (
	"context"
	"fmt"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// AutoSnapshotter is notified after writes; see the snapshot package.
type AutoSnapshotter interface {
	MaybeSnapshot(branch *wal.Branch)
}

// Writer appends puts and deletes to one branch's WAL.
type Writer struct {
	wal          *wal.Service
	branches     *branchwal.BranchService
	materializer *materializer.Service
	branch       *wal.Branch
	actor        string
	autoSnapshot AutoSnapshotter
}

// New creates a writer for a branch. The materializer supplies pre-images
// (point lookups), which power diff, undo and audit.
func New(walService *wal.Service, branches *branchwal.BranchService, mat *materializer.Service, branch *wal.Branch) *Writer {
	return &Writer{
		wal:          walService,
		branches:     branches,
		materializer: mat,
		branch:       branch,
	}
}

// SetActor tags subsequent writes with an actor identity (e.g. "user:jake",
// "agent:session-42") for audit and per-actor undo.
func (w *Writer) SetActor(actor string) { w.actor = actor }

// SetAutoSnapshotter enables threshold-based automatic snapshotting.
func (w *Writer) SetAutoSnapshotter(a AutoSnapshotter) { w.autoSnapshot = a }

// guardNotLive rejects writes to checked-out branches: their WAL is fed
// from the physical database's change stream, and writing through both
// paths would double-log.
func (w *Writer) guardNotLive() error {
	if w.branch.IsLive() {
		return fmt.Errorf("branch %s is checked out as %s: write through a MongoDB driver instead", w.branch.Name, w.branch.PhysicalDB)
	}
	return nil
}

// Put sets a document to exactly the given state (an upsert by post-image).
// A missing _id is generated. Returns the entry's LSN.
func (w *Writer) Put(ctx context.Context, collection string, doc bson.M) (int64, error) {
	lsns, err := w.PutMany(ctx, collection, []bson.M{doc})
	if err != nil {
		return 0, err
	}
	return lsns[0], nil
}

// PutMany sets several documents in one batched append (one contiguous LSN
// range).
func (w *Writer) PutMany(ctx context.Context, collection string, docs []bson.M) ([]int64, error) {
	if err := w.guardNotLive(); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("PutMany requires at least one document")
	}

	entries := make([]*wal.Entry, 0, len(docs))
	for i, doc := range docs {
		normalized, id, err := normalizeDoc(doc)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		docID := wal.DocumentIDString(id)

		post, err := bson.Marshal(normalized)
		if err != nil {
			return nil, fmt.Errorf("document %d: failed to marshal: %w", i, err)
		}
		entry := &wal.Entry{
			ProjectID:  w.branch.ProjectID,
			BranchID:   w.branch.ID,
			Operation:  wal.OpPut,
			Collection: collection,
			DocumentID: docID,
			PostImage:  post,
			Actor:      w.actor,
		}
		if pre, err := w.preImage(collection, docID); err != nil {
			return nil, err
		} else if pre != nil {
			entry.PreImage = pre
		}
		entries = append(entries, entry)
	}

	lsns, err := w.wal.AppendBatch(entries)
	if err != nil {
		return nil, err
	}
	if err := w.advanceHead(lsns[len(lsns)-1]); err != nil {
		return nil, err
	}
	return lsns, nil
}

// Delete removes a document by its _id value. Returns (lsn, true) when the
// document existed, (0, false) when there was nothing to delete.
func (w *Writer) Delete(ctx context.Context, collection string, id interface{}) (int64, bool, error) {
	if err := w.guardNotLive(); err != nil {
		return 0, false, err
	}
	docID := wal.DocumentIDString(id)

	pre, err := w.preImage(collection, docID)
	if err != nil {
		return 0, false, err
	}
	if pre == nil {
		return 0, false, nil
	}

	entry := &wal.Entry{
		ProjectID:  w.branch.ProjectID,
		BranchID:   w.branch.ID,
		Operation:  wal.OpDelete,
		Collection: collection,
		DocumentID: docID,
		PreImage:   pre,
		Actor:      w.actor,
	}
	lsn, err := w.wal.Append(entry)
	if err != nil {
		return 0, false, err
	}
	if err := w.advanceHead(lsn); err != nil {
		return 0, false, err
	}
	return lsn, true, nil
}

// preImage point-looks-up the document's current state, marshalled, or nil.
func (w *Writer) preImage(collection, docID string) (bson.Raw, error) {
	current, err := w.materializer.MaterializeDocument(w.branch, collection, docID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up pre-image for %s/%s: %w", collection, docID, err)
	}
	if current == nil {
		return nil, nil
	}
	raw, err := bson.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pre-image for %s/%s: %w", collection, docID, err)
	}
	return raw, nil
}

func (w *Writer) advanceHead(lsn int64) error {
	if err := w.branches.UpdateBranchHead(w.branch.ID, lsn); err != nil {
		return fmt.Errorf("failed to advance branch head: %w", err)
	}
	if lsn > w.branch.HeadLSN {
		w.branch.HeadLSN = lsn
	}
	if w.autoSnapshot != nil {
		w.autoSnapshot.MaybeSnapshot(w.branch)
	}
	return nil
}

// normalizeDoc round-trips the document through BSON (a private, normalized
// copy) and ensures it has an _id.
func normalizeDoc(doc bson.M) (bson.M, interface{}, error) {
	raw, err := bson.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("document must be BSON-marshallable: %w", err)
	}
	var normalized bson.M
	if err := bson.Unmarshal(raw, &normalized); err != nil {
		return nil, nil, fmt.Errorf("document must decode to a BSON document: %w", err)
	}
	id, exists := normalized["_id"]
	if !exists || id == nil {
		id = primitive.NewObjectID()
		normalized["_id"] = id
	}
	return normalized, id, nil
}
