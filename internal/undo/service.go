// Package undo reverts a range of WAL history by writing compensating
// operations. History stays append-only: an undo produces new entries (or
// new physical writes on a live branch) rather than deleting anything, so
// undos are themselves audited and can be undone.
//
// For every document touched in [fromLSN, toLSN], the state to restore is
// the pre-image of the *oldest* in-range entry for that document — the
// document as it stood just before the range began. An oldest entry without
// a pre-image is an insert, so the compensation is a delete.
package undo

import (
	"context"
	"fmt"
	"sort"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service plans and applies range/actor undos.
type Service struct {
	wal      *wal.Service
	branches *branchwal.BranchService
	client   *mongo.Client
}

// NewService creates an undo service. The client reaches physical branch
// databases for live-branch application.
func NewService(walService *wal.Service, branches *branchwal.BranchService, client *mongo.Client) *Service {
	return &Service{wal: walService, branches: branches, client: client}
}

// Compensation is one document's planned restoration.
type Compensation struct {
	Collection string
	DocumentID string
	// ID is the document's real BSON _id (recovered from the images), used
	// to address the document in a physical database.
	ID interface{}
	// Restore is the document to put back; nil means the document did not
	// exist before the range (the compensation is a delete).
	Restore bson.M
}

// Conflict marks a document that cannot be undone safely under an actor
// filter: another actor modified it after the filtered writes, so blindly
// restoring would discard their change.
type Conflict struct {
	Collection string
	DocumentID string
	OtherActor string
	AtLSN      int64
}

// Plan describes what an undo would do.
type Plan struct {
	BranchID      string
	FromLSN       int64
	ToLSN         int64
	Actor         string // empty: all actors
	Compensations []Compensation
	Conflicts     []Conflict
	// Unrecoverable documents were touched in-range but their oldest entry
	// carries no pre-image (degraded capture); they cannot be restored.
	Unrecoverable []string
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

	entries, err := s.wal.GetBranchEntries(branch.ID, "", fromLSN, toLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to load range: %w", err)
	}

	plan := &Plan{BranchID: branch.ID, FromLSN: fromLSN, ToLSN: toLSN, Actor: actor}

	type docKey struct{ collection, id string }
	oldest := make(map[docKey]*wal.Entry)
	conflicted := make(map[docKey]bool)

	for _, entry := range entries {
		if !entry.IsData() || branch.IsDiscardedForRead(entry.LSN, toLSN) {
			continue
		}
		key := docKey{entry.Collection, entry.DocumentID}

		if actor != "" && entry.Actor != actor {
			// Another actor touched this document. If our actor wrote it
			// earlier in the range, restoring would clobber this write.
			if _, ours := oldest[key]; ours && !conflicted[key] {
				conflicted[key] = true
				plan.Conflicts = append(plan.Conflicts, Conflict{
					Collection: entry.Collection,
					DocumentID: entry.DocumentID,
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
		switch {
		case len(entry.PreImage) > 0:
			var pre bson.M
			if err := bson.Unmarshal(entry.PreImage, &pre); err != nil {
				return nil, fmt.Errorf("failed to decode pre-image for %s/%s: %w", key.collection, key.id, err)
			}
			plan.Compensations = append(plan.Compensations, Compensation{
				Collection: key.collection,
				DocumentID: key.id,
				ID:         pre["_id"],
				Restore:    pre,
			})
		case entry.Operation == wal.OpPut:
			// No pre-image on a put: the document was created in-range;
			// its real _id comes from the post-image.
			var post bson.M
			if err := bson.Unmarshal(entry.PostImage, &post); err != nil {
				return nil, fmt.Errorf("failed to decode post-image for %s/%s: %w", key.collection, key.id, err)
			}
			plan.Compensations = append(plan.Compensations, Compensation{
				Collection: key.collection,
				DocumentID: key.id,
				ID:         post["_id"],
			})
		default:
			// A delete without a pre-image cannot be restored.
			plan.Unrecoverable = append(plan.Unrecoverable,
				fmt.Sprintf("%s/%s", key.collection, key.id))
		}
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

// Apply executes a plan. On a live branch the compensations are written to
// the physical database (the ingester records them as new history); on a
// metadata-only branch they append directly to the WAL. Returns how many
// documents were restored and how many deleted.
func (s *Service) Apply(ctx context.Context, branch *wal.Branch, plan *Plan) (restored, deleted int, err error) {
	if branch.ID != plan.BranchID {
		return 0, 0, fmt.Errorf("plan belongs to branch %s, not %s", plan.BranchID, branch.ID)
	}
	if branch.IsLive() {
		return s.applyPhysical(ctx, branch, plan)
	}
	return s.applyWAL(branch, plan)
}

func (s *Service) applyPhysical(ctx context.Context, branch *wal.Branch, plan *Plan) (restored, deleted int, err error) {
	physical := s.client.Database(branch.PhysicalDB)
	for _, c := range plan.Compensations {
		coll := physical.Collection(c.Collection)
		if c.Restore == nil {
			if _, err := coll.DeleteOne(ctx, bson.M{"_id": c.ID}); err != nil {
				return restored, deleted, fmt.Errorf("failed to delete %s/%s: %w", c.Collection, c.DocumentID, err)
			}
			deleted++
			continue
		}
		if _, err := coll.ReplaceOne(ctx,
			bson.M{"_id": c.ID},
			c.Restore,
			options.Replace().SetUpsert(true),
		); err != nil {
			return restored, deleted, fmt.Errorf("failed to restore %s/%s: %w", c.Collection, c.DocumentID, err)
		}
		restored++
	}
	return restored, deleted, nil
}

func (s *Service) applyWAL(branch *wal.Branch, plan *Plan) (restored, deleted int, err error) {
	entries := make([]*wal.Entry, 0, len(plan.Compensations))
	for _, c := range plan.Compensations {
		entry := &wal.Entry{
			ProjectID:  branch.ProjectID,
			BranchID:   branch.ID,
			Collection: c.Collection,
			DocumentID: c.DocumentID,
			Actor:      "undo",
		}
		if c.Restore == nil {
			entry.Operation = wal.OpDelete
			deleted++
		} else {
			entry.Operation = wal.OpPut
			post, err := bson.Marshal(c.Restore)
			if err != nil {
				return 0, 0, fmt.Errorf("failed to encode restoration for %s/%s: %w", c.Collection, c.DocumentID, err)
			}
			entry.PostImage = post
			restored++
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return 0, 0, nil
	}
	lsns, err := s.wal.AppendBatch(entries)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to append compensations: %w", err)
	}
	if err := s.branches.UpdateBranchHead(branch.ID, lsns[len(lsns)-1]); err != nil {
		return 0, 0, fmt.Errorf("failed to advance branch head: %w", err)
	}
	return restored, deleted, nil
}
