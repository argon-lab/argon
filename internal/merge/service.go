// Package merge implements three-way, document-level merges between a
// branch and its parent, with a reviewable persisted plan in between — the
// "data pull request".
//
// The three states are: base (the source branch's inherited state at its
// fork point), ours (the target branch's current state) and theirs (the
// source branch's current state). Per document:
//
//   - theirs unchanged            → nothing to merge
//   - only theirs changed         → adopt theirs (put or delete)
//   - both changed identically    → nothing to do
//   - both changed differently    → conflict, resolved by an explicit
//     strategy (theirs/ours) or by aborting
//
// Comparison is canonical (sorted-key) BSON equality. Plans are persisted
// pending, then applied exactly once against the exact target head they
// were computed for — a moved head means the plan is stale and must be
// recomputed. Application appends ordinary puts/deletes (or writes to the
// physical database when the target is live) plus one merge control entry
// carrying the audit metadata; history stays append-only, so a merge can
// be undone like any other range.
package merge

import (
	"context"
	"fmt"
	"sort"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/mongoexpr"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/argon-lab/argon/internal/walwriter"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Plan statuses.
const (
	StatusPending = "pending"
	StatusApplied = "applied"
)

// Conflict strategies.
const (
	StrategyTheirs = "theirs"
	StrategyOurs   = "ours"
)

// Change is one document the merge would adopt from the source branch.
type Change struct {
	Collection string `bson:"collection" json:"collection"`
	DocumentID string `bson:"document_id" json:"document_id"`
	// Delete marks a removal; otherwise Document is the adopted state.
	Delete   bool   `bson:"delete,omitempty" json:"delete,omitempty"`
	Document bson.M `bson:"document,omitempty" json:"document,omitempty"`
}

// Conflict is a document both sides changed differently since the fork.
type Conflict struct {
	Collection string `bson:"collection" json:"collection"`
	DocumentID string `bson:"document_id" json:"document_id"`
	Base       bson.M `bson:"base,omitempty" json:"base,omitempty"`
	Ours       bson.M `bson:"ours,omitempty" json:"ours,omitempty"`
	Theirs     bson.M `bson:"theirs,omitempty" json:"theirs,omitempty"`
}

// Plan is a persisted, reviewable merge proposal.
type Plan struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	ProjectID      string             `bson:"project_id" json:"project_id"`
	SourceBranchID string             `bson:"source_branch_id" json:"source_branch_id"`
	SourceBranch   string             `bson:"source_branch" json:"source_branch"`
	TargetBranchID string             `bson:"target_branch_id" json:"target_branch_id"`
	TargetBranch   string             `bson:"target_branch" json:"target_branch"`
	SourceHead     int64              `bson:"source_head" json:"source_head"`
	TargetHead     int64              `bson:"target_head" json:"target_head"`
	BaseLSN        int64              `bson:"base_lsn" json:"base_lsn"`
	Changes        []Change           `bson:"changes" json:"changes"`
	Conflicts      []Conflict         `bson:"conflicts" json:"conflicts"`
	Status         string             `bson:"status" json:"status"`
	Strategy       string             `bson:"strategy,omitempty" json:"strategy,omitempty"`
	CreatedAt      time.Time          `bson:"created_at" json:"created_at"`
	AppliedAt      *time.Time         `bson:"applied_at,omitempty" json:"applied_at,omitempty"`
}

// Service computes, persists and applies merge plans.
type Service struct {
	wal          *wal.Service
	branches     *branchwal.BranchService
	materializer *materializer.Service
	client       *mongo.Client
	plans        *mongo.Collection
}

// NewService creates a merge service. The client reaches physical branch
// databases for live-target application.
func NewService(db *mongo.Database, walService *wal.Service, branches *branchwal.BranchService, mat *materializer.Service, client *mongo.Client) *Service {
	return &Service{
		wal:          walService,
		branches:     branches,
		materializer: mat,
		client:       client,
		plans:        db.Collection("wal_merge_plans"),
	}
}

// Compute builds a merge plan for merging a branch into its parent,
// without persisting it (the diff view).
func (s *Service) Compute(sourceBranchID string) (*Plan, error) {
	source, err := s.branches.GetBranchByID(sourceBranchID)
	if err != nil {
		return nil, fmt.Errorf("source branch not found: %w", err)
	}
	if source.ParentID == "" {
		return nil, fmt.Errorf("branch %s has no parent to merge into", source.Name)
	}
	target, err := s.branches.GetBranchByID(source.ParentID)
	if err != nil {
		return nil, fmt.Errorf("target branch not found: %w", err)
	}

	base, err := s.materializer.MaterializeBranchAtLSN(source, source.BaseLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize fork state: %w", err)
	}
	theirs, err := s.materializer.MaterializeBranch(source)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize source: %w", err)
	}
	ours, err := s.materializer.MaterializeBranch(target)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize target: %w", err)
	}

	plan := &Plan{
		ProjectID:      source.ProjectID,
		SourceBranchID: source.ID,
		SourceBranch:   source.Name,
		TargetBranchID: target.ID,
		TargetBranch:   target.Name,
		SourceHead:     source.HeadLSN,
		TargetHead:     target.HeadLSN,
		BaseLSN:        source.BaseLSN,
		Status:         StatusPending,
		CreatedAt:      time.Now(),
	}

	for _, collection := range unionKeys3(base, ours, theirs) {
		baseDocs := base[collection]
		ourDocs := ours[collection]
		theirDocs := theirs[collection]

		for _, docID := range unionKeys3(baseDocs, ourDocs, theirDocs) {
			b, o, th := baseDocs[docID], ourDocs[docID], theirDocs[docID]

			theirsChanged, err := docsUnequal(b, th)
			if err != nil {
				return nil, err
			}
			if !theirsChanged {
				continue // Source did nothing; ours stands either way.
			}
			oursChanged, err := docsUnequal(b, o)
			if err != nil {
				return nil, err
			}
			if oursChanged {
				identical, err := docsUnequal(o, th)
				if err != nil {
					return nil, err
				}
				if !identical {
					continue // Both sides made the same change.
				}
				plan.Conflicts = append(plan.Conflicts, Conflict{
					Collection: collection,
					DocumentID: docID,
					Base:       b,
					Ours:       o,
					Theirs:     th,
				})
				continue
			}
			plan.Changes = append(plan.Changes, Change{
				Collection: collection,
				DocumentID: docID,
				Delete:     th == nil,
				Document:   th,
			})
		}
	}

	sortChanges(plan.Changes)
	sort.Slice(plan.Conflicts, func(i, j int) bool {
		a, b := plan.Conflicts[i], plan.Conflicts[j]
		if a.Collection != b.Collection {
			return a.Collection < b.Collection
		}
		return a.DocumentID < b.DocumentID
	})
	return plan, nil
}

// Preview computes a plan and persists it pending review — the data PR.
func (s *Service) Preview(ctx context.Context, sourceBranchID string) (*Plan, error) {
	plan, err := s.Compute(sourceBranchID)
	if err != nil {
		return nil, err
	}
	result, err := s.plans.InsertOne(ctx, plan)
	if err != nil {
		return nil, fmt.Errorf("failed to persist merge plan: %w", err)
	}
	plan.ID = result.InsertedID.(primitive.ObjectID)
	return plan, nil
}

// GetPlan loads a persisted plan.
func (s *Service) GetPlan(ctx context.Context, planID primitive.ObjectID) (*Plan, error) {
	var plan Plan
	if err := s.plans.FindOne(ctx, bson.M{"_id": planID}).Decode(&plan); err != nil {
		return nil, fmt.Errorf("merge plan not found: %w", err)
	}
	return &plan, nil
}

// ListPlans returns a project's plans, newest first.
func (s *Service) ListPlans(ctx context.Context, projectID string) ([]*Plan, error) {
	cursor, err := s.plans.Find(ctx, bson.M{"project_id": projectID},
		options.Find().SetSort(bson.M{"created_at": -1}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	var plans []*Plan
	if err := cursor.All(ctx, &plans); err != nil {
		return nil, err
	}
	return plans, nil
}

// ApplyResult summarizes an executed merge.
type ApplyResult struct {
	Applied           int
	ConflictsResolved int
}

// Apply executes a pending plan against the exact heads it was computed
// for. Conflicts require an explicit strategy; without one, a conflicted
// plan refuses to apply.
func (s *Service) Apply(ctx context.Context, planID primitive.ObjectID, strategy string) (*ApplyResult, error) {
	plan, err := s.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if plan.Status != StatusPending {
		return nil, fmt.Errorf("merge plan is %s, not pending", plan.Status)
	}

	target, err := s.branches.GetBranchByID(plan.TargetBranchID)
	if err != nil {
		return nil, fmt.Errorf("target branch not found: %w", err)
	}
	source, err := s.branches.GetBranchByID(plan.SourceBranchID)
	if err != nil {
		return nil, fmt.Errorf("source branch not found: %w", err)
	}
	if target.HeadLSN != plan.TargetHead {
		return nil, fmt.Errorf("stale plan: target advanced from LSN %d to %d since preview; run preview again", plan.TargetHead, target.HeadLSN)
	}
	if source.HeadLSN != plan.SourceHead {
		return nil, fmt.Errorf("stale plan: source advanced from LSN %d to %d since preview; run preview again", plan.SourceHead, source.HeadLSN)
	}

	changes := append([]Change{}, plan.Changes...)
	resolved := 0
	if len(plan.Conflicts) > 0 {
		switch strategy {
		case StrategyTheirs:
			for _, c := range plan.Conflicts {
				changes = append(changes, Change{
					Collection: c.Collection,
					DocumentID: c.DocumentID,
					Delete:     c.Theirs == nil,
					Document:   c.Theirs,
				})
				resolved++
			}
			sortChanges(changes)
		case StrategyOurs:
			resolved = len(plan.Conflicts) // Keep ours: nothing to write.
		case "":
			return nil, fmt.Errorf("plan has %d conflict(s); pass a strategy (theirs/ours) to resolve them", len(plan.Conflicts))
		default:
			return nil, fmt.Errorf("unknown strategy %q (want theirs or ours)", strategy)
		}
	}

	if target.IsLive() {
		err = s.applyPhysical(ctx, target, changes)
	} else {
		err = s.applyWAL(ctx, target, source, changes)
	}
	if err != nil {
		return nil, err
	}

	// The audit marker: which branch merged in, under which plan.
	mergeRecord := &wal.Entry{
		ProjectID: target.ProjectID,
		BranchID:  target.ID,
		Operation: wal.OpMerge,
		Actor:     "merge",
		Metadata: map[string]interface{}{
			"plan_id":            plan.ID.Hex(),
			"source_branch_id":   plan.SourceBranchID,
			"source_branch":      plan.SourceBranch,
			"source_head":        plan.SourceHead,
			"changes":            len(changes),
			"conflicts_resolved": resolved,
			"strategy":           strategy,
		},
	}
	if lsn, err := s.wal.Append(mergeRecord); err != nil {
		return nil, fmt.Errorf("failed to record the merge: %w", err)
	} else if !target.IsLive() {
		if err := s.branches.UpdateBranchHead(target.ID, lsn); err != nil {
			return nil, fmt.Errorf("failed to advance target head: %w", err)
		}
	}

	now := time.Now()
	_, err = s.plans.UpdateOne(ctx,
		bson.M{"_id": plan.ID, "status": StatusPending},
		bson.M{"$set": bson.M{"status": StatusApplied, "strategy": strategy, "applied_at": now}},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to mark plan applied: %w", err)
	}
	return &ApplyResult{Applied: len(changes), ConflictsResolved: resolved}, nil
}

func (s *Service) applyWAL(ctx context.Context, target, source *wal.Branch, changes []Change) error {
	writer := walwriter.New(s.wal, s.branches, s.materializer, target)
	writer.SetActor("merge:" + source.Name)

	// Group puts per collection for contiguous batches; deletes go singly.
	putsByCollection := make(map[string][]bson.M)
	for _, c := range changes {
		if !c.Delete {
			putsByCollection[c.Collection] = append(putsByCollection[c.Collection], c.Document)
		}
	}
	for _, collection := range sortedKeys(putsByCollection) {
		if _, err := writer.PutMany(ctx, collection, putsByCollection[collection]); err != nil {
			return fmt.Errorf("failed to apply merge puts to %s: %w", collection, err)
		}
	}
	for _, c := range changes {
		if !c.Delete {
			continue
		}
		id, err := documentIDValue(c)
		if err != nil {
			return err
		}
		if _, _, err := writer.Delete(ctx, c.Collection, id); err != nil {
			return fmt.Errorf("failed to apply merge delete to %s/%s: %w", c.Collection, c.DocumentID, err)
		}
	}
	return nil
}

func (s *Service) applyPhysical(ctx context.Context, target *wal.Branch, changes []Change) error {
	physical := s.client.Database(target.PhysicalDB)
	for _, c := range changes {
		coll := physical.Collection(c.Collection)
		if c.Delete {
			id, err := documentIDValue(c)
			if err != nil {
				return err
			}
			if _, err := coll.DeleteOne(ctx, bson.M{"_id": id}); err != nil {
				return fmt.Errorf("failed to delete %s/%s: %w", c.Collection, c.DocumentID, err)
			}
			continue
		}
		if _, err := coll.ReplaceOne(ctx,
			bson.M{"_id": c.Document["_id"]},
			c.Document,
			options.Replace().SetUpsert(true),
		); err != nil {
			return fmt.Errorf("failed to apply %s/%s: %w", c.Collection, c.DocumentID, err)
		}
	}
	return nil
}

// documentIDValue recovers the real _id for a delete change: from the base
// or ours image if present in a conflict-resolution, otherwise the change
// document. For plain deletes theirs is nil, so the materialized target
// state supplies it.
func documentIDValue(c Change) (interface{}, error) {
	if c.Document != nil {
		if id, ok := c.Document["_id"]; ok {
			return id, nil
		}
	}
	// Deletes carry no document; the canonical string form round-trips for
	// the common _id types (ObjectID hex and strings).
	if oid, err := primitive.ObjectIDFromHex(c.DocumentID); err == nil {
		return oid, nil
	}
	return c.DocumentID, nil
}

// docsUnequal is canonical BSON inequality with nil meaning "absent".
func docsUnequal(a, b bson.M) (bool, error) {
	if a == nil || b == nil {
		return (a == nil) != (b == nil), nil
	}
	equal, err := mongoexpr.CanonicalEqual(a, b)
	if err != nil {
		return false, err
	}
	return !equal, nil
}

func sortChanges(changes []Change) {
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if a.Collection != b.Collection {
			return a.Collection < b.Collection
		}
		return a.DocumentID < b.DocumentID
	})
}

func unionKeys3[V any](maps ...map[string]V) []string {
	seen := make(map[string]bool)
	for _, m := range maps {
		for k := range m {
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
