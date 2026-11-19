// Package undo reverts a range of WAL history by writing compensating
// operations. History stays append-only: an undo produces new entries (or
// new physical writes on a live branch) rather than deleting anything, so
// undos are themselves audited and can be undone.
//
// For every document touched in [fromLSN, toLSN], the state to restore is
// the pre-image of the *oldest* in-range entry for that document — the
// document as it stood just before the range began. A missing pre-image
// requires proof of insertion or recoverable earlier history. Later writes
// outside the selected range are conflicts and are preserved.
package undo

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/mongoexpr"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service plans and applies range/actor undos.
type Service struct {
	wal      *wal.Service
	branches *branchwal.BranchService
	client   *mongo.Client
	mat      *materializer.Service
}

// NewService creates an undo service. The client reaches physical branch
// databases for live-branch application.
func NewService(walService *wal.Service, branches *branchwal.BranchService, client *mongo.Client, mats ...*materializer.Service) *Service {
	mat := materializer.NewService(walService, branches)
	if len(mats) > 0 {
		mat = mats[0]
	}
	return &Service{wal: walService, branches: branches, client: client, mat: mat}
}

// Compensation is one document's planned restoration.
type Compensation struct {
	Collection string `json:"collection"`
	DocumentID string `json:"document_id"`
	// ID is the document's real BSON _id (recovered from the images), used
	// to address the document in a physical database.
	ID interface{} `json:"id"`
	// Restore is the document to put back; nil means the document did not
	// exist before the range (the compensation is a delete).
	Restore bson.M `json:"restore"`
	Before  bson.M `json:"before"`

	restoreRaw bson.Raw
	beforeRaw  bson.Raw
}

// Conflict marks a document changed after the selected range, or by another
// actor after filtered writes. Restoring it would discard an unselected change.
type Conflict struct {
	Collection string `json:"collection"`
	DocumentID string `json:"document_id"`
	OtherActor string `json:"other_actor"`
	AtLSN      int64  `json:"at_lsn"`
}

// Plan describes what an undo would do.
type Plan struct {
	BranchID      string         `json:"branch_id"`
	FromLSN       int64          `json:"from_lsn"`
	ToLSN         int64          `json:"to_lsn"`
	Actor         string         `json:"actor"` // empty: all actors
	HeadLSN       int64          `json:"head_lsn"`
	Compensations []Compensation `json:"compensations"`
	Conflicts     []Conflict     `json:"conflicts"`
	// Unrecoverable documents were touched in-range but their oldest entry
	// carries no pre-image (degraded capture); they cannot be restored.
	Unrecoverable []string `json:"unrecoverable"`
}

// BuildPlan computes the compensations for undoing [fromLSN, toLSN] on a
// branch, optionally restricted to one actor's writes.
func (s *Service) BuildPlan(branch *wal.Branch, fromLSN, toLSN int64, actor string) (*Plan, error) {
	if toLSN == 0 {
		toLSN = branch.HeadLSN
	}
	if fromLSN <= 0 || fromLSN > toLSN {
		return nil, fmt.Errorf("invalid undo range [%d, %d]", fromLSN, toLSN)
	}
	if toLSN > branch.HeadLSN {
		return nil, fmt.Errorf("undo range end %d is beyond branch head %d", toLSN, branch.HeadLSN)
	}
	if fromLSN <= branch.BaseLSN {
		return nil, fmt.Errorf("undo range start %d is at or below the branch fork point %d: undo operates on the branch's own history", fromLSN, branch.BaseLSN)
	}

	// An undo restores each document to its state just before the range,
	// which it learns from the oldest in-range entry. If GC has deleted
	// entries inside the range, the oldest survivor is not the oldest
	// write, and the plan would restore the wrong state.
	floors, err := s.wal.ReclaimFloors(branch.ID)
	if err != nil {
		return nil, err
	}
	for collection, floor := range floors {
		if floor >= fromLSN {
			return nil, fmt.Errorf("%w: undo from LSN %d reaches entries of collection %q at or below LSN %d that were deleted; start above LSN %d",
				wal.ErrHistoryReclaimed, fromLSN, collection, floor, floor)
		}
	}

	entries, err := s.wal.GetBranchEntries(branch.ID, "", fromLSN, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to load range: %w", err)
	}

	plan := &Plan{BranchID: branch.ID, FromLSN: fromLSN, ToLSN: toLSN, Actor: actor, HeadLSN: branch.HeadLSN}

	type docKey struct{ collection, id string }
	oldest := make(map[docKey]*wal.Entry)
	conflicted := make(map[docKey]bool)

	for _, entry := range entries {
		if !entry.IsData() || branch.IsDiscardedForRead(entry.LSN, branch.HeadLSN) {
			continue
		}
		id := entry.DocumentID
		image := entry.PostImage
		if len(image) == 0 {
			image = entry.PreImage
		}
		if len(image) > 0 {
			docID, err := wal.DocumentIDFromImage(image)
			if err != nil {
				return nil, err
			}
			id = wal.DocumentIDString(docID)
		}
		key := docKey{entry.Collection, id}

		if entry.LSN > toLSN || (actor != "" && entry.Actor != actor) {
			// A write not selected for undo must survive, including later
			// writes by the same actor when the selected range ends earlier.
			if _, ours := oldest[key]; ours && !conflicted[key] {
				conflicted[key] = true
				plan.Conflicts = append(plan.Conflicts, Conflict{
					Collection: entry.Collection,
					DocumentID: key.id,
					OtherActor: entry.Actor,
					AtLSN:      entry.LSN,
				})
			}
			continue
		}
		if _, seen := oldest[key]; !seen {
			oldest[key] = entry
		}
	}

	for key, entry := range oldest {
		if conflicted[key] {
			continue
		}
		c := Compensation{Collection: key.collection, DocumentID: key.id}
		switch {
		case len(entry.PreImage) > 0:
			c.restoreRaw = entry.PreImage
			c.ID, err = wal.DocumentIDFromImage(entry.PreImage)
		case entry.Operation == wal.OpPut && safeCreation(entry):
			// Older metadata writers did not always include pre-images. Recover
			// the preceding state instead of guessing that every put is an insert.
			c.restoreRaw, err = s.priorImage(branch, entry, key.id)
			if err != nil {
				plan.Unrecoverable = append(plan.Unrecoverable, fmt.Sprintf("%s/%s", key.collection, key.id))
				continue
			}
			var post bson.M
			if err := wal.UnmarshalDocument(entry.PostImage, &post); err != nil {
				return nil, fmt.Errorf("failed to decode post-image for %s/%s: %w", key.collection, key.id, err)
			}
			c.ID, err = wal.DocumentIDFromImage(entry.PostImage)
		default:
			// A delete without a pre-image cannot be restored.
			plan.Unrecoverable = append(plan.Unrecoverable,
				fmt.Sprintf("%s/%s", key.collection, key.id))
			continue
		}
		if err != nil {
			return nil, err
		}
		if c.restoreRaw != nil {
			if err := wal.UnmarshalDocument(c.restoreRaw, &c.Restore); err != nil {
				return nil, fmt.Errorf("failed to decode restoration for %s/%s: %w", key.collection, key.id, err)
			}
			c.Restore["_id"] = c.ID
			if len(entry.PreImage) > 0 {
				c.restoreRaw, err = s.restorationImage(branch, entry, c)
				if err != nil {
					return nil, err
				}
			}
		}
		c.beforeRaw, err = s.mat.EditDocumentAtLSN(branch, c.Collection, c.DocumentID, branch.HeadLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to load current state for undo: %w", err)
		}
		if c.beforeRaw != nil {
			if err := wal.UnmarshalDocument(c.beforeRaw, &c.Before); err != nil {
				return nil, err
			}
			c.Before["_id"] = c.ID
		}
		plan.Compensations = append(plan.Compensations, c)
	}

	// Deterministic order for application and display.
	sort.Slice(plan.Compensations, func(i, j int) bool {
		a, b := plan.Compensations[i], plan.Compensations[j]
		if a.Collection != b.Collection {
			return a.Collection < b.Collection
		}
		return a.DocumentID < b.DocumentID
	})
	sort.Strings(plan.Unrecoverable)
	return plan, nil
}

// Earlier programmatic writers saved a name-sorted view of the preceding
// state. Recover its order only if the prior image accounts for every value
// in that recorded pre-image. A native capture supplies its own pre-image.
func (s *Service) restorationImage(branch *wal.Branch, entry *wal.Entry, c Compensation) (bson.Raw, error) {
	image, err := materializer.EditImage(c.Restore, entry.PreImage, wal.PreImage, entry)
	if err != nil || !bytes.Equal(image, entry.PreImage) || len(entry.Metadata) != 0 || entry.TxnID != "" {
		return image, err
	}
	sorted, err := wal.MarshalDocument(c.Restore)
	if err != nil || !bytes.Equal(image, sorted) {
		return image, err
	}
	prior, err := s.priorImage(branch, entry, c.DocumentID)
	if err != nil || prior == nil {
		return image, nil
	}
	return materializer.EditImage(c.Restore, prior, wal.PreImage, entry)
}

func (s *Service) priorImage(branch *wal.Branch, entry *wal.Entry, key string) (bson.Raw, error) {
	at := entry.LSN - 1
	// The first write after a reset can be adjacent to the discarded history.
	// Step back to the position visible when this entry was written.
	ranges := branch.EffectiveDiscardedRanges()
	for i := len(ranges) - 1; i >= 0; i-- {
		r := ranges[i]
		if r.To < entry.LSN && r.Contains(at) {
			at = r.From - 1
		}
	}
	return s.mat.EditDocumentAtLSN(branch, entry.Collection, key, at)
}

// A missing pre-image is only evidence of insertion on a writer path that
// knows the prior state. Legacy ingested puts may have been degraded updates.
func safeCreation(entry *wal.Entry) bool {
	if op, ok := entry.Metadata["capture_operation"].(string); ok {
		return op == "insert"
	}
	if existed, ok := entry.Metadata["previous_exists"].(bool); ok {
		return !existed
	}
	return entry.Actor != "ingest"
}

// Apply executes a plan. On a live branch the compensations are written to
// the physical database (the ingester records them as new history); on a
// metadata-only branch they append directly to the WAL. Returns how many
// documents were restored and how many deleted.
func (s *Service) Apply(ctx context.Context, branch *wal.Branch, plan *Plan) (restored, deleted int, err error) {
	if branch.ID != plan.BranchID {
		return 0, 0, fmt.Errorf("plan belongs to branch %s, not %s", plan.BranchID, branch.ID)
	}
	if branch.IsFollower() {
		return 0, 0, branch.RefuseFollowerWrite("undo")
	}
	if branch.HeadLSN != plan.HeadLSN {
		return 0, 0, fmt.Errorf("undo plan is stale; branch head changed")
	}
	// The decoded compensation is authoritative even for a plan reconstructed
	// or edited by a caller. These candidates were vetted at capture or replay.
	for i := range plan.Compensations {
		c := &plan.Compensations[i]
		c.restoreRaw, err = materializer.EditImage(c.Restore, c.restoreRaw, wal.PreImage, nil)
		if err != nil {
			return 0, 0, err
		}
		c.beforeRaw, err = materializer.EditImage(c.Before, c.beforeRaw, wal.PreImage, nil)
		if err != nil {
			return 0, 0, err
		}
	}
	if branch.IsLive() {
		return s.applyPhysical(ctx, branch, plan)
	}
	return s.applyWAL(ctx, branch, plan)
}

func (s *Service) applyPhysical(ctx context.Context, branch *wal.Branch, plan *Plan) (restored, deleted int, err error) {
	own, err := branch.OwnDatabase()
	if err != nil {
		return 0, 0, err
	}
	physical := s.client.Database(own)
	_, err = s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		restored, deleted = 0, 0
		if err := s.branches.FenceBranch(sc, branch); err != nil {
			return nil, err
		}
		for _, c := range plan.Compensations {
			coll := physical.Collection(c.Collection)
			var current bson.M
			raw, err := coll.FindOne(sc, bson.M{"_id": c.ID}).Raw()
			if err == nil {
				if err := wal.UnmarshalDocument(raw, &current); err != nil {
					return nil, err
				}
			} else if err != mongo.ErrNoDocuments {
				return nil, err
			}
			equal, err := mongoexpr.CanonicalEqual(current, c.Before)
			if err != nil {
				return nil, err
			}
			if !equal {
				return nil, fmt.Errorf("undo plan is stale: %s/%s changed before capture caught up", c.Collection, c.DocumentID)
			}
			if c.Restore == nil {
				if _, err := coll.DeleteOne(sc, bson.M{"_id": c.ID}); err != nil {
					return nil, fmt.Errorf("failed to delete %s/%s: %w", c.Collection, c.DocumentID, err)
				}
				deleted++
				continue
			}
			if _, err := coll.ReplaceOne(sc,
				bson.M{"_id": c.ID},
				c.restoreRaw,
				options.Replace().SetUpsert(true),
			); err != nil {
				return nil, fmt.Errorf("failed to restore %s/%s: %w", c.Collection, c.DocumentID, err)
			}
			restored++
		}
		return nil, nil
	})
	if err != nil {
		return 0, 0, err
	}
	return restored, deleted, err
}

func (s *Service) applyWAL(ctx context.Context, branch *wal.Branch, plan *Plan) (restored, deleted int, err error) {
	entries := make([]*wal.Entry, 0, len(plan.Compensations))
	for _, c := range plan.Compensations {
		entry := &wal.Entry{
			ProjectID:  branch.ProjectID,
			BranchID:   branch.ID,
			Collection: c.Collection,
			DocumentID: c.DocumentID,
			Actor:      "undo",
			Metadata:   map[string]interface{}{"previous_exists": c.Before != nil},
			PreImage:   c.beforeRaw,
		}
		if c.Restore == nil {
			entry.Operation = wal.OpDelete
			deleted++
		} else {
			entry.Operation = wal.OpPut
			entry.PostImage = c.restoreRaw
			restored++
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return 0, 0, nil
	}
	_, err = s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if err := s.branches.FenceBranch(sc, branch); err != nil {
			return nil, err
		}
		lsns, err := s.wal.AppendBatchContext(sc, entries)
		if err != nil {
			return nil, err
		}
		return nil, s.branches.CompareAndSetHead(sc, branch.ID, plan.HeadLSN, lsns[len(lsns)-1])
	})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to append compensations: %w", err)
	}
	return restored, deleted, nil
}
