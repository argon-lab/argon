// Package walwriter is the programmatic write path for branches that are
// not checked out: imports, merges, undos, seeding and tests. It appends
// explicit document states — callers say exactly what a document becomes —
// and never evaluates filters or update operators. Applications work with
// checked-out branches through real MongoDB drivers instead; the change
// stream feeds their WAL (see the ingest package).
package walwriter

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// AutoSnapshotter is notified after writes; see the snapshot package.
type AutoSnapshotter interface {
	MaybeSnapshot(branch *wal.Branch)
}

// Writer appends puts and deletes to one branch's WAL.
type Writer struct {
	mu           sync.Mutex
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
func (w *Writer) SetActor(actor string) { w.mu.Lock(); defer w.mu.Unlock(); w.actor = actor }

// SetAutoSnapshotter enables threshold-based automatic snapshotting.
func (w *Writer) SetAutoSnapshotter(a AutoSnapshotter) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.autoSnapshot = a
}

// guardNotLive rejects writes to checked-out branches: their WAL is fed
// from the physical database's change stream, and writing through both
// paths would double-log.
func (w *Writer) guardNotLive() error {
	fresh, err := w.branches.GetBranchByID(w.branch.ID)
	if err != nil {
		return err
	}
	w.branch = fresh
	if w.branch.IsFollower() {
		return w.branch.RefuseFollowerWrite("write")
	}
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
	w.mu.Lock()
	defer w.mu.Unlock()
	for attempt := 0; ; attempt++ {
		lsns, err := w.putMany(ctx, collection, docs)
		if !errors.Is(err, branchwal.ErrBranchChanged) || attempt >= 31 {
			return lsns, err
		}
		if err := retryPause(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

func (w *Writer) putMany(ctx context.Context, collection string, docs []bson.M) ([]int64, error) {
	if err := w.guardNotLive(); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("PutMany requires at least one document")
	}

	entries := make([]*wal.Entry, 0, len(docs))
	previousPosts := make(map[string]bson.Raw)
	for i, doc := range docs {
		post, id, err := normalizeDoc(doc)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		docID := wal.DocumentIDString(id)
		pre, err := w.preImage(collection, docID, previousPosts[docID])
		if err != nil {
			return nil, err
		}

		entries = append(entries, &wal.Entry{
			ProjectID:  w.branch.ProjectID,
			BranchID:   w.branch.ID,
			Operation:  wal.OpPut,
			Collection: collection,
			DocumentID: docID,
			PreImage:   pre,
			PostImage:  post,
			Actor:      w.actor,
		})
		previousPosts[docID] = post
	}

	return w.commit(ctx, entries)
}

// Delete removes a document by its _id value. Returns (lsn, true) when the
// document existed, (0, false) when there was nothing to delete.
func (w *Writer) Delete(ctx context.Context, collection string, id interface{}) (int64, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for attempt := 0; ; attempt++ {
		lsn, deleted, err := w.delete(ctx, collection, id)
		if !errors.Is(err, branchwal.ErrBranchChanged) || attempt >= 31 {
			return lsn, deleted, err
		}
		if err := retryPause(ctx, attempt); err != nil {
			return 0, false, err
		}
	}
}

func retryPause(ctx context.Context, attempt int) error {
	delay := time.Duration(attempt+1) * time.Millisecond
	if delay > 20*time.Millisecond {
		delay = 20 * time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (w *Writer) delete(ctx context.Context, collection string, id interface{}) (int64, bool, error) {
	if err := w.guardNotLive(); err != nil {
		return 0, false, err
	}
	docID := wal.DocumentIDString(id)

	pre, err := w.preImage(collection, docID, nil)
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
	lsns, err := w.commit(ctx, []*wal.Entry{entry})
	if err != nil {
		return 0, false, err
	}
	return lsns[0], true, nil
}

func (w *Writer) commit(ctx context.Context, entries []*wal.Entry) ([]int64, error) {
	var lsns []int64
	expected := w.branch.HeadLSN
	_, err := w.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		// Acquire the branch document before appending. A racing checkout,
		// reset, merge or writer makes the whole append fail atomically.
		if err := w.branches.FenceBranch(sc, w.branch); err != nil {
			return nil, err
		}
		var err error
		lsns, err = w.wal.AppendBatchContext(sc, entries)
		if err != nil {
			return nil, err
		}
		return nil, w.branches.CompareAndSetHead(sc, w.branch.ID, expected, lsns[len(lsns)-1])
	})
	if err != nil {
		return nil, err
	}
	w.branch.HeadLSN = lsns[len(lsns)-1]
	if w.autoSnapshot != nil {
		w.autoSnapshot.MaybeSnapshot(w.branch)
	}
	return lsns, nil
}

// preImage marshals the value a reader sees after the previous put in the
// batch, or at the branch head if this is the first write to the document.
func (w *Writer) preImage(collection, docID string, previousPost bson.Raw) (bson.Raw, error) {
	var current bson.M
	if previousPost != nil {
		decoded, err := wal.DecodeImage(previousPost)
		if err != nil {
			return nil, fmt.Errorf("failed to decode previous post-image for %s/%s: %w", collection, docID, err)
		}
		current = decoded.TakeValue()
	} else {
		var err error
		current, err = w.materializer.MaterializeDocument(w.branch, collection, docID)
		if err != nil {
			return nil, fmt.Errorf("failed to look up pre-image for %s/%s: %w", collection, docID, err)
		}
	}
	if current == nil {
		return nil, nil
	}
	raw, err := wal.MarshalDocument(current)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pre-image for %s/%s: %w", collection, docID, err)
	}
	return raw, nil
}

// normalizeDoc assigns a missing _id without mutating the caller's map.
// Both the image and its WAL key come from the same encoded document.
func normalizeDoc(doc bson.M) (bson.Raw, interface{}, error) {
	if _, ok := doc["_id"]; !ok {
		withID := make(bson.M, len(doc)+1)
		maps.Copy(withID, doc)
		withID["_id"] = primitive.NewObjectID()
		doc = withID
	}
	raw, err := wal.MarshalDocument(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("document must be BSON-marshallable: %w", err)
	}
	id, err := wal.DocumentIDFromImage(raw)
	return raw, id, err
}
