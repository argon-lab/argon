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
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/mongoexpr"
	"github.com/argon-lab/argon/v2/internal/wal"
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

// ValidateStrategy checks an explicit resolution choice before loading a plan.
func ValidateStrategy(strategy string) error {
	switch strategy {
	case "", StrategyOurs, StrategyTheirs:
		return nil
	default:
		return fmt.Errorf("unknown strategy %q (want theirs or ours)", strategy)
	}
}

// Change is one document the merge would adopt from the source branch.
type Change struct {
	Collection string `bson:"collection" json:"collection"`
	DocumentID string `bson:"document_id" json:"document_id"`
	// Delete marks a removal; otherwise Document is the adopted state.
	Delete   bool        `bson:"delete,omitempty" json:"delete,omitempty"`
	Document bson.M      `bson:"-" json:"document,omitempty"`
	ID       interface{} `bson:"document_key,omitempty" json:"document_key,omitempty"`
	Before   bson.M      `bson:"-" json:"before,omitempty"`

	DocumentRaw  bson.Raw `bson:"document,omitempty" json:"-"`
	BeforeRaw    bson.Raw `bson:"before,omitempty" json:"-"`
	SortedImages []string `bson:"sorted_images,omitempty" json:"sorted_images,omitempty"`
	// The driver's decoded ID can normalize an unusual BSON encoding.
	// Retain the stored auxiliary key for exact approval validation.
	storedKey bson.RawValue
}

// Conflict is a document both sides changed differently since the fork.
type Conflict struct {
	Collection string `bson:"collection" json:"collection"`
	DocumentID string `bson:"document_id" json:"document_id"`
	Base       bson.M `bson:"-" json:"base,omitempty"`
	Ours       bson.M `bson:"-" json:"ours,omitempty"`
	Theirs     bson.M `bson:"-" json:"theirs,omitempty"`

	BaseRaw      bson.Raw `bson:"base,omitempty" json:"-"`
	OursRaw      bson.Raw `bson:"ours,omitempty" json:"-"`
	TheirsRaw    bson.Raw `bson:"theirs,omitempty" json:"-"`
	SortedImages []string `bson:"sorted_images,omitempty" json:"sorted_images,omitempty"`
}

// Plan is a persisted, reviewable merge proposal.
type Plan struct {
	FormatVersion  int                `bson:"format_version,omitempty" json:"format_version,omitempty"`
	SourceVersion  int64              `bson:"source_version,omitempty" json:"source_version,omitempty"`
	TargetVersion  int64              `bson:"target_version,omitempty" json:"target_version,omitempty"`
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
	// WriteBack is set once the plan was claimed for writing to the external
	// database its target follows; see writeback.go.
	WriteBack *WriteBackRecord `bson:"write_back,omitempty" json:"write_back,omitempty"`
	// WriteBackLastError is why the latest write-back attempt wrote nothing.
	WriteBackLastError string `bson:"write_back_last_error,omitempty" json:"write_back_last_error,omitempty"`

	// ProvenanceVersion identifies the writer that verified or marked every image.
	ProvenanceVersion    int    `bson:"provenance_version,omitempty" json:"provenance_version,omitempty"`
	FollowedDatabase     string `bson:"followed_database,omitempty" json:"followed_database,omitempty"`
	FollowedReplicaSetID string `bson:"followed_replica_set_id,omitempty" json:"followed_replica_set_id,omitempty"`
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

// ErrFullReplayRequired reports that the changed-documents path cannot
// produce a plan for these branches and the states must be rebuilt in full.
var ErrFullReplayRequired = errors.New("merge plan needs a full replay of the branch states")

// Compute builds a merge plan for merging a branch into its parent,
// without persisting it (the diff view).
//
// Only a document the source branch wrote since its fork can end up in a
// plan, so the plan is computed from those documents alone and its cost
// follows the size of the change, not the size of the database (see
// ComputeChanged). When that path cannot answer, the three states are
// rebuilt in full instead (ComputeFull). Both produce the same plan.
func (s *Service) Compute(sourceBranchID string) (*Plan, error) {
	source, target, err := s.endpoints(sourceBranchID)
	if err != nil {
		return nil, err
	}
	if plan, err := s.computeChanged(source, target); err == nil {
		return plan, nil
	}
	// The full path is the reference: it either builds the plan or returns
	// the error that describes what is wrong with the history.
	return s.computeFull(source, target)
}

// ComputeChanged builds the plan from the documents the source branch wrote
// since its fork, and fails with ErrFullReplayRequired (or the underlying
// read error) instead of falling back to a full replay.
func (s *Service) ComputeChanged(sourceBranchID string) (*Plan, error) {
	source, target, err := s.endpoints(sourceBranchID)
	if err != nil {
		return nil, err
	}
	return s.computeChanged(source, target)
}

// ComputeFull builds the plan by materializing the fork state, the source
// and the target in full and comparing every document. It is the reference
// the changed-documents path is tested against.
func (s *Service) ComputeFull(sourceBranchID string) (*Plan, error) {
	source, target, err := s.endpoints(sourceBranchID)
	if err != nil {
		return nil, err
	}
	return s.computeFull(source, target)
}

func (s *Service) endpoints(sourceBranchID string) (source, target *wal.Branch, err error) {
	source, err = s.branches.GetBranchByID(sourceBranchID)
	if err != nil {
		return nil, nil, fmt.Errorf("source branch not found: %w", err)
	}
	if source.ParentID == "" {
		return nil, nil, fmt.Errorf("branch %s has no parent to merge into", source.Name)
	}
	target, err = s.branches.GetBranchByID(source.ParentID)
	if err != nil {
		return nil, nil, fmt.Errorf("target branch not found: %w", err)
	}
	return source, target, nil
}

func newPlan(source, target *wal.Branch) *Plan {
	return &Plan{
		FormatVersion:  2,
		SourceVersion:  source.MutationVersion,
		TargetVersion:  target.MutationVersion,
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
}

// consider classifies one document from its three states — base (at the
// fork), ours (target) and theirs (source), nil meaning absent — and records
// the change or conflict it amounts to, if any.
func (p *Plan) consider(collection, docID string, b, o, th bson.M) error {
	theirsChanged, err := docsUnequal(b, th)
	if err != nil {
		return err
	}
	if !theirsChanged {
		return nil // Source did nothing; ours stands either way.
	}
	oursChanged, err := docsUnequal(b, o)
	if err != nil {
		return err
	}
	if oursChanged {
		differ, err := docsUnequal(o, th)
		if err != nil {
			return err
		}
		if !differ {
			return nil // Both sides made the same change.
		}
		p.Conflicts = append(p.Conflicts, Conflict{
			Collection: collection,
			DocumentID: docID,
			Base:       b,
			Ours:       o,
			Theirs:     th,
		})
		return nil
	}
	id, err := wal.DocumentIDValue(docID)
	if err != nil {
		return err
	}
	p.Changes = append(p.Changes, Change{
		Collection: collection,
		DocumentID: docID,
		Delete:     th == nil,
		Document:   th,
		ID:         id,
		Before:     o,
	})
	return nil
}

func (p *Plan) sort() {
	sortChanges(p.Changes)
	sort.Slice(p.Conflicts, func(i, j int) bool {
		a, b := p.Conflicts[i], p.Conflicts[j]
		if a.Collection != b.Collection {
			return a.Collection < b.Collection
		}
		return a.DocumentID < b.DocumentID
	})
}

func (s *Service) computeFull(source, target *wal.Branch) (*Plan, error) {
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

	plan := newPlan(source, target)
	for _, collection := range unionKeys3(base, ours, theirs) {
		baseDocs := base[collection]
		ourDocs := ours[collection]
		theirDocs := theirs[collection]
		for _, docID := range unionKeys3(baseDocs, ourDocs, theirDocs) {
			if err := plan.consider(collection, docID, baseDocs[docID], ourDocs[docID], theirDocs[docID]); err != nil {
				return nil, err
			}
		}
	}
	plan.sort()
	return plan, nil
}

// MaxChangedEntries is the most entries a source branch may hold in its own
// log for a plan to be computed from them. Beyond it the three states are
// materialized instead, which reads each document once however often it was
// rewritten and starts from the branch's snapshots.
var MaxChangedEntries int64 = 200000

// sourceLogComplete reports, as ErrFullReplayRequired, that the source's own
// log may no longer name every document the branch changed: garbage
// collection deleted entries of it, with a recorded floor or, in releases up
// to 2.1.5, without one (bounded by the snapshot manifests of that time).
func (s *Service) sourceLogComplete(source *wal.Branch) error {
	floors, err := s.wal.ReclaimFloors(source.ID)
	if err != nil {
		return err
	}
	for collection, floor := range floors {
		if floor > source.BaseLSN {
			return fmt.Errorf("%w: garbage collection deleted entries of %q on the source branch", ErrFullReplayRequired, collection)
		}
	}
	unrecorded, err := s.materializer.UnrecordedReclaimBound(source)
	if err != nil {
		return err
	}
	if unrecorded > source.BaseLSN {
		return fmt.Errorf("%w: entries of the source branch may have been deleted by a release that kept no record of it", ErrFullReplayRequired)
	}
	return nil
}

// computeChanged rests on one observation. The source's state is the fork
// state with the source's own entries folded on top, so a document differs
// between the two only if one of those entries is about it — and a document
// that does not differ there is never part of a plan, whatever the target
// did to it. The candidates are therefore the documents named by the
// source's own visible entries, and for each of them the three states are
// read from the document's own history:
//
//   - theirs: the newest of the source's own entries for it;
//   - base:   the document at the fork point, read along the source's
//     inherited chain;
//   - ours:   the document at the target's head.
//
// Everything is read through the same visibility rules as a full replay
// (ancestry bounds, discarded ranges, reclaim floors), so a plan built here
// equals the one ComputeFull builds.
func (s *Service) computeChanged(source, target *wal.Branch) (*Plan, error) {
	// Newest visible entry of the source's own log, per collection and
	// document. Folding the source's entries in LSN order leaves exactly
	// this entry's outcome for each key.
	if err := s.sourceLogComplete(source); err != nil {
		return nil, err
	}
	// A branch that rewrote more than this is cheaper to read as states.
	count, err := s.wal.CountBranchEntries(source.ID, source.BaseLSN+1, source.HeadLSN, MaxChangedEntries+1)
	if err != nil {
		return nil, err
	}
	if count > MaxChangedEntries {
		return nil, fmt.Errorf("%w: the source branch holds more than %d entries of its own", ErrFullReplayRequired, MaxChangedEntries)
	}

	latest := make(map[string]map[string]*wal.Entry)
	err = s.wal.ScanBranchDataEntries(source.ID, source.BaseLSN+1, source.HeadLSN, func(entry *wal.Entry) error {
		if source.IsDiscardedForRead(entry.LSN, source.HeadLSN) {
			return nil
		}
		key, ok := materializer.EntryStateKey(entry)
		if entry.DocumentKeyVersion < 1 {
			ok = false // Legacy keys: see wal.ErrLegacyDocumentKeys.
		}
		// Only the outcome is needed from here on.
		entry.PreImage, entry.CompressedPreImage = nil, nil
		if !ok {
			return fmt.Errorf("%w: entry at LSN %d does not identify its document on its own", ErrFullReplayRequired, entry.LSN)
		}
		docs := latest[entry.Collection]
		if docs == nil {
			docs = make(map[string]*wal.Entry)
			latest[entry.Collection] = docs
		}
		if current := docs[key]; current == nil || entry.LSN > current.LSN {
			docs[key] = entry
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Checked again after the scan: GC records a floor before it deletes.
	if err := s.sourceLogComplete(source); err != nil {
		return nil, err
	}

	plan := newPlan(source, target)
	for collection, entries := range latest {
		keys := make([]string, 0, len(entries))
		for key := range entries {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		base, err := s.materializer.MaterializeDocumentsAtLSN(source, collection, keys, source.BaseLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to read fork state: %w", err)
		}
		ours, err := s.materializer.MaterializeDocumentsAtLSN(target, collection, keys, target.HeadLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to read target: %w", err)
		}
		for _, key := range keys {
			state := make(map[string]bson.M, 1)
			if err := s.materializer.ApplyEntry(state, entries[key]); err != nil {
				return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entries[key].LSN, err)
			}
			if err := plan.consider(collection, key, base[key], ours[key], state[key]); err != nil {
				return nil, err
			}
		}
	}
	plan.sort()
	return plan, nil
}

// Preview computes a plan and persists it pending review — the data PR.
func (s *Service) Preview(ctx context.Context, sourceBranchID string) (*Plan, error) {
	plan, err := s.PreparePlan(ctx, sourceBranchID)
	if err != nil {
		return nil, err
	}
	if err := s.SavePlan(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// SavePlan persists a computed plan. Callers can assign its ID to prepare a
// printable response before persistence.
func (s *Service) SavePlan(ctx context.Context, proposal *Plan) error {
	plan := *proposal
	checkProvenance := plan.FormatVersion == orderedPlanVersion && plan.ProvenanceVersion == 0
	if checkProvenance {
		// A programmatic format-3 proposal can carry raw images with no map
		// views, or maps with no evidence. Establish its provenance from the
		// branches before certifying it under a new ID.
		for i := range plan.Changes {
			if err := plan.Changes[i].decodeStoredImages(); err != nil {
				return err
			}
		}
		for i := range plan.Conflicts {
			if err := plan.Conflicts[i].decodeStoredImages(); err != nil {
				return err
			}
		}
	}
	if plan.FormatVersion == 2 || checkProvenance {
		if err := s.preparePlanImages(ctx, &plan); err != nil {
			return err
		}
	}
	ordinary := plan.approvalEligible() && plan.FollowedDatabase == ""
	for i := range plan.Changes {
		if err := plan.Changes[i].encodeImages(ordinary); err != nil {
			return err
		}
	}
	for i := range plan.Conflicts {
		if err := plan.Conflicts[i].encodeImages(ordinary); err != nil {
			return err
		}
	}
	if plan.ID.IsZero() {
		plan.ID = primitive.NewObjectID()
	}
	raw, err := encodePendingPlan(&plan)
	if err != nil {
		return err
	}
	_, err = s.plans.InsertOne(ctx, raw)
	if err != nil {
		return fmt.Errorf("failed to persist merge plan: %w", err)
	}
	*proposal = plan
	return nil
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
	entries, err := s.ListPlanEntries(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var plans []*Plan
	for _, entry := range entries {
		if entry.Err != nil {
			return nil, entry.Err
		}
		plans = append(plans, entry.Plan)
	}
	return plans, nil
}

// PlanEntry identifies an individual plan even if its contents cannot be decoded.
type PlanEntry struct {
	ID   string
	Plan *Plan
	Err  error
}

// ListPlanEntries keeps a damaged record's error with its ID, so other plans
// for the project can still be displayed.
func (s *Service) ListPlanEntries(ctx context.Context, projectID string) ([]PlanEntry, error) {
	cursor, err := s.plans.Find(ctx, bson.M{"project_id": projectID},
		options.Find().SetSort(bson.M{"created_at": -1}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	var entries []PlanEntry
	for cursor.Next(ctx) {
		var plan Plan
		if err := cursor.Decode(&plan); err != nil {
			key := cursor.Current.Lookup("_id")
			id := key.String()
			if oid, ok := key.ObjectIDOK(); ok {
				id = oid.Hex()
			}
			entries = append(entries, PlanEntry{ID: id, Err: err})
		} else {
			entries = append(entries, PlanEntry{ID: plan.ID.Hex(), Plan: &plan})
		}
	}
	return entries, cursor.Err()
}

// ApplyResult summarizes an executed merge.
type ApplyResult struct {
	Applied           int
	ConflictsResolved int
}

// RequirePending reports why a plan is unavailable for apply.
func (plan *Plan) RequirePending() error {
	switch plan.Status {
	case StatusPending:
		return nil
	case StatusWritingBack:
		return fmt.Errorf("merge plan %s is %s. Following marks it applied if it sees the transaction. %s", plan.ID.Hex(), plan.Status, writingBackAdvice)
	default:
		return fmt.Errorf("merge plan is %s, not pending", plan.Status)
	}
}

// Apply executes a pending plan against the exact heads it was computed
// for. Conflicts require an explicit strategy; without one, a conflicted
// plan refuses to apply.
func (s *Service) Apply(ctx context.Context, planID primitive.ObjectID, strategy string) (*ApplyResult, error) {
	if err := ValidateStrategy(strategy); err != nil {
		return nil, err
	}
	plan, err := s.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if err := plan.RequirePending(); err != nil {
		return nil, err
	}

	target, err := s.branches.GetBranchByID(plan.TargetBranchID)
	if err != nil {
		return nil, fmt.Errorf("target branch not found: %w", err)
	}
	source, err := s.branches.GetBranchByID(plan.SourceBranchID)
	if err != nil {
		return nil, fmt.Errorf("source branch not found: %w", err)
	}
	if target.IsFollower() {
		// Appending here would leave this branch holding documents its source
		// does not. The only way a followed branch changes is its source.
		return nil, target.RefuseFollowerWrite("merge apply")
	}
	if target.HeadLSN != plan.TargetHead {
		return nil, fmt.Errorf("stale plan: target advanced from LSN %d to %d since preview; run preview again", plan.TargetHead, target.HeadLSN)
	}
	if source.HeadLSN != plan.SourceHead {
		return nil, fmt.Errorf("stale plan: source advanced from LSN %d to %d since preview; run preview again", plan.SourceHead, source.HeadLSN)
	}
	if plan.FormatVersion >= 2 && (source.MutationVersion != plan.SourceVersion || target.MutationVersion != plan.TargetVersion) {
		return nil, fmt.Errorf("stale plan: branch state changed since preview; run preview again")
	}

	changes, resolved, err := resolveChanges(plan, strategy)
	if err != nil {
		return nil, err
	}

	// Older plans stored ambiguous text keys. Recover the typed state keys
	// from the original heads, including inserts that exist only on source.
	if plan.FormatVersion < 2 {
		ours, err := s.materializer.MaterializeBranch(target)
		if err != nil {
			return nil, err
		}
		theirs, err := s.materializer.MaterializeBranch(source)
		if err != nil {
			return nil, err
		}
		for i := range changes {
			c := &changes[i]
			targetKey, err := legacyPlanKey(ours[c.Collection], c.DocumentID)
			if err != nil {
				return nil, err
			}
			sourceKey, err := legacyPlanKey(theirs[c.Collection], c.DocumentID)
			if err != nil {
				return nil, err
			}
			if targetKey != "" && sourceKey != "" && targetKey != sourceKey {
				return nil, fmt.Errorf("legacy merge plan has ambiguous ID %q; run preview again", c.DocumentID)
			}
			c.Before = ours[c.Collection][targetKey]
			key := targetKey
			if key == "" {
				key = sourceKey
			}
			if key == "" {
				return nil, fmt.Errorf("legacy merge plan lacks BSON ID for %q; run preview again", c.DocumentID)
			}
			c.DocumentID = key
		}
	}
	if plan.FormatVersion < orderedPlanVersion {
		if err := s.prepareApplyImages(source, changes); err != nil {
			return nil, err
		}
	}
	for i := range changes {
		c := &changes[i]
		c.ID, err = documentIDValue(*c)
		if err != nil {
			return nil, err
		}
		if !c.Delete && c.DocumentRaw == nil {
			return nil, fmt.Errorf("merge plan has no replacement for %s/%s; run preview again", c.Collection, c.DocumentID)
		}
		c.DocumentRaw, err = ordinaryImage(c.DocumentRaw, c.Document, c.ID)
		if err != nil {
			return nil, err
		}
		c.BeforeRaw, err = ordinaryImage(c.BeforeRaw, c.Before, c.ID)
		if err != nil {
			return nil, err
		}
	}

	_, err = s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		// This claim is committed together with all effects. Crashes or
		// transaction retries cannot leave a partly applied/spent plan.
		claimed, err := s.plans.UpdateOne(sc, bson.M{"_id": plan.ID, "status": StatusPending},
			bson.M{"$set": bson.M{"status": "applying"}, "$unset": bson.M{"write_back_last_error": ""}})
		if err != nil {
			return nil, err
		}
		if claimed.MatchedCount != 1 {
			return nil, fmt.Errorf("merge plan is no longer pending")
		}
		if err := s.branches.FenceBranch(sc, target); err != nil {
			return nil, err
		}
		if err := s.branches.FenceBranch(sc, source); err != nil {
			return nil, err
		}

		if target.IsLive() {
			if err := s.applyPhysical(sc, target, changes); err != nil {
				return nil, err
			}
		} else {
			if err := s.applyWAL(sc, target, source, changes, "merge:"+plan.ID.Hex()); err != nil {
				return nil, err
			}
		}
		mergeRecord := &wal.Entry{
			ProjectID: target.ProjectID, BranchID: target.ID,
			Operation: wal.OpMerge, Actor: "merge", TxnID: "merge:" + plan.ID.Hex(),
			Metadata: map[string]interface{}{
				"plan_id": plan.ID.Hex(), "source_branch_id": plan.SourceBranchID,
				"source_branch": plan.SourceBranch, "source_head": plan.SourceHead,
				"changes": len(changes), "conflicts_resolved": resolved, "strategy": strategy,
			},
		}
		lsn, err := s.wal.AppendContext(sc, mergeRecord)
		if err != nil {
			return nil, fmt.Errorf("failed to record merge: %w", err)
		}
		if !target.IsLive() {
			if err := s.branches.CompareAndSetHead(sc, target.ID, target.HeadLSN, lsn); err != nil {
				return nil, err
			}
		}
		_, err = s.plans.UpdateOne(sc, bson.M{"_id": plan.ID, "status": "applying"},
			bson.M{"$set": bson.M{"status": StatusApplied, "strategy": strategy, "applied_at": time.Now()}})
		return nil, err
	})
	if err != nil {
		return nil, err
	}
	return &ApplyResult{Applied: len(changes), ConflictsResolved: resolved}, nil
}

func (s *Service) applyWAL(ctx context.Context, target, source *wal.Branch, changes []Change, txnID string) error {
	entries := make([]*wal.Entry, 0, len(changes))
	for _, c := range changes {
		e := &wal.Entry{ProjectID: target.ProjectID, BranchID: target.ID,
			Collection: c.Collection, DocumentID: c.DocumentID, Actor: "merge:" + source.Name,
			Operation: wal.OpPut, TxnID: txnID}
		e.PreImage = c.BeforeRaw
		if c.Delete {
			e.Operation = wal.OpDelete
		} else {
			e.PostImage = c.DocumentRaw
		}
		entries = append(entries, e)
	}
	_, err := s.wal.AppendBatchContext(ctx, entries)
	return err
}

func (s *Service) applyPhysical(ctx context.Context, target *wal.Branch, changes []Change) error {
	own, err := target.OwnDatabase()
	if err != nil {
		return err
	}
	physical := s.client.Database(own)
	for _, c := range changes {
		coll := physical.Collection(c.Collection)
		currentRaw, err := coll.FindOne(ctx, bson.M{"_id": c.ID}).Raw()
		if err != nil && err != mongo.ErrNoDocuments {
			return err
		}
		var current bson.M
		if err := decodeImage(currentRaw, &current); err != nil {
			return err
		}
		changed, err := docsUnequal(current, c.Before)
		if err != nil {
			return err
		}
		if changed && current != nil && slices.Contains(c.SortedImages, "before") {
			// In particular, a stored code scope can still contain duplicate
			// fields. Compare it under the same encoding used at preview.
			// MongoDB can match numerically equal keys with different BSON
			// types. Preserve the live key's type and compound field order.
			sorted, err := sortedImage(current, currentRaw.Lookup("_id"), false)
			if err != nil {
				return err
			}
			changed = !bytes.Equal(sorted, c.BeforeRaw)
		}
		if changed {
			return fmt.Errorf("stale merge plan: physical document %s/%s changed since preview; wait for capture and preview again", c.Collection, c.DocumentID)
		}
		if c.Delete {
			if _, err := coll.DeleteOne(ctx, bson.M{"_id": c.ID}); err != nil {
				return err
			}
		} else if _, err := coll.ReplaceOne(ctx, bson.M{"_id": c.ID}, c.DocumentRaw, options.Replace().SetUpsert(true)); err != nil {
			return err
		}
	}
	return nil
}

func legacyPlanKey(state map[string]bson.M, stored string) (string, error) {
	var matched string
	for key, doc := range state {
		if key == stored || wal.LegacyDocumentIDString(doc["_id"]) == stored {
			if matched != "" {
				return "", fmt.Errorf("legacy merge plan has ambiguous ID %q; run preview again", stored)
			}
			matched = key
		}
	}
	return matched, nil
}

func documentIDValue(c Change) (interface{}, error) {
	// A saved image may decode its compound _id into an unordered map.
	// The WAL key identifies the document with its original BSON field order.
	return wal.DocumentIDValue(c.DocumentID)
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
