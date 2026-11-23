package wal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// BranchService manages WAL-based branches
type BranchService struct {
	db         *mongo.Database
	collection *mongo.Collection
	wal        *wal.Service

	// onDelete runs after a branch is successfully soft-deleted through
	// DeleteBranch (never after ForceDeleteBranch: force-deleted branches
	// may still anchor descendants' history). Used to reclaim snapshots
	// without this package depending on the snapshot package.
	onDelete func(branchID string)

	// deleteGuard runs before DeleteBranch commits; a non-nil error
	// refuses the deletion. Used to keep pinned branches alive without
	// this package depending on the pin package.
	deleteGuard func(branchID string) error
}

var ErrBranchChanged = errors.New("branch changed concurrently; reload and retry")

// ErrBranchNotFound reports that no live branch has the ID or name asked for.
var ErrBranchNotFound = wal.ErrBranchNotFound

// notFound gives the driver's "no documents" from a lookup by ID its
// meaning, a branch that is gone, where the error goes to a person. The
// lookups themselves return the driver's error as they always have: callers
// compare against it.
func notFound(err error) error {
	if errors.Is(err, mongo.ErrNoDocuments) {
		return ErrBranchNotFound
	}
	return err
}

// NotFound is notFound for the packages that call the lookups.
func NotFound(err error) error { return notFound(err) }

// ErrBranchExists reports that the project already has a branch of that name.
var ErrBranchExists = errors.New("branch already exists")

// SetDeleteGuard registers a check that can refuse DeleteBranch.
func (s *BranchService) SetDeleteGuard(guard func(branchID string) error) {
	s.deleteGuard = guard
}

// SetDeleteHook registers a callback invoked after a successful
// DeleteBranch.
func (s *BranchService) SetDeleteHook(hook func(branchID string)) {
	s.onDelete = hook
}

// NewBranchService creates a new WAL branch service
func NewBranchService(db *mongo.Database, walService *wal.Service) (*BranchService, error) {
	s := &BranchService{
		db:         db,
		collection: db.Collection("wal_branches"),
		wal:        walService,
	}

	if err := ensureBranchIndexes(context.Background(), s.collection); err != nil {
		return nil, err
	}

	return s, nil
}

// liveNameIndex makes a name unique among a project's live branches. A
// deleted branch keeps its record, and its name is free to use again.
const liveNameIndex = "project_id_1_name_1_live"

// legacyNameIndex is the index releases up to 2.12.0 created: unique over
// deleted branches too, so a deleted branch's name could never be reused.
const legacyNameIndex = "project_id_1_name_1"

// ensureBranchIndexes creates the branch indexes and retires the legacy name
// index. The new index is created first, so there is no moment at which
// live names are not unique. An older release that connects later creates
// the legacy index again where no name has been reused; the next call here
// drops it.
func ensureBranchIndexes(ctx context.Context, collection *mongo.Collection) error {
	indexes := []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "project_id", Value: 1},
				{Key: "name", Value: 1},
			},
			Options: options.Index().SetName(liveNameIndex).SetUnique(true).
				SetPartialFilterExpression(bson.D{{Key: "is_deleted", Value: false}}),
		},
		{
			Keys: bson.D{
				{Key: "project_id", Value: 1},
				{Key: "is_deleted", Value: 1},
			},
		},
	}
	if _, err := collection.Indexes().CreateMany(ctx, indexes); err != nil {
		return fmt.Errorf("failed to create indexes: %w", err)
	}
	specs, err := collection.Indexes().ListSpecifications(ctx)
	if err != nil {
		return fmt.Errorf("inspect branch indexes: %w", err)
	}
	for _, spec := range specs {
		if spec.Name != legacyNameIndex {
			continue
		}
		if _, err := collection.Indexes().DropOne(ctx, spec.Name); err != nil {
			// Another process retiring it at the same moment got there first.
			var command mongo.CommandError
			if errors.As(err, &command) && command.Code == 27 { // IndexNotFound
				continue
			}
			return fmt.Errorf("retire branch index %s: %w", spec.Name, err)
		}
	}
	return nil
}

// insertBranch inserts the branch and appends its create record in one
// transaction, so a create that loses to another of the same name leaves no
// record of a branch that was never made. The branch goes in first: a loser
// stops there, before a sequence number is reserved for its record. stamp
// sets on the branch what depends on the record's LSN and returns the same
// as the fields to store.
func (s *BranchService) insertBranch(branch *wal.Branch, entry func() *wal.Entry, stamp func(lsn int64) bson.M) error {
	ctx := context.Background()
	runs := 0
	attempt := func() error {
		_, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
			// The driver runs this again at once while another create of
			// the name is still uncommitted. Wait a little longer each time
			// rather than spin.
			if runs > 0 {
				time.Sleep(min(time.Duration(runs)*5*time.Millisecond, 100*time.Millisecond))
			}
			runs++
			if _, err := s.collection.InsertOne(sc, branch); err != nil {
				return nil, err
			}
			lsn, err := s.wal.AppendContext(sc, entry())
			if err != nil {
				return nil, fmt.Errorf("failed to append WAL entry: %w", err)
			}
			if fields := stamp(lsn); len(fields) > 0 {
				if _, err := s.collection.UpdateOne(sc, bson.M{"_id": branch.ID}, bson.M{"$set": fields}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		return err
	}
	for tries := 0; ; tries++ {
		err := attempt()
		conflict, legacy := nameConflict(err)
		if !conflict {
			if err != nil {
				return fmt.Errorf("failed to create branch: %w", err)
			}
			return nil
		}
		// On the live index a conflict means a live branch held the name
		// when this one was inserted, whatever has become of it since.
		if !legacy {
			return ErrBranchExists
		}
		// The legacy index, created again by an older release, also counts
		// deleted branches. If no live branch has the name, retire the
		// index and try again.
		if existing, _ := s.GetBranch(branch.ProjectID, branch.Name); existing != nil {
			return ErrBranchExists
		}
		if tries >= 2 {
			return fmt.Errorf("failed to create branch: the index %s keeps coming back, which an Argon release up to 2.12.0 running against this metadata database does; upgrade it: %w", legacyNameIndex, err)
		}
		if indexErr := ensureBranchIndexes(ctx, s.collection); indexErr != nil {
			return indexErr
		}
	}
}

// nameConflict reports whether err is a duplicate key on a branch name
// index, and whether that index is the legacy one. Other duplicates, such
// as a branch ID in use or a WAL sequence collision, are not a name in use
// and are reported as they are.
func nameConflict(err error) (conflict, legacy bool) {
	check := func(code int, message string) {
		if code != 11000 {
			return
		}
		// "... collection: <db>.wal_branches index: <name> dup key: {...}".
		// The index is named before the key values, which hold the branch
		// name and can say anything.
		head, _, _ := strings.Cut(message, " dup key:")
		const marker = ".wal_branches index: "
		at := strings.LastIndex(head, marker)
		if at < 0 {
			return
		}
		// (A collation, if the index had one, would follow the name.)
		index, _, _ := strings.Cut(strings.TrimSpace(head[at+len(marker):]), " ")
		switch index {
		case liveNameIndex:
			conflict = true
		case legacyNameIndex:
			conflict, legacy = true, true
		}
	}
	var write mongo.WriteException
	if errors.As(err, &write) {
		for _, e := range write.WriteErrors {
			check(e.Code, e.Message)
		}
	}
	var command mongo.CommandError
	if errors.As(err, &command) {
		check(int(command.Code), command.Message)
	}
	return conflict, legacy
}

// CreateBranch creates a new WAL-based branch
func (s *BranchService) CreateBranch(projectID, name, parentID string) (*wal.Branch, error) {
	return s.createBranch(projectID, name, parentID, nil)
}

// CreateCredentialSandbox reserves the username and expiration in the initial
// branch record, so cleanup can find the user even after a process restart.
func (s *BranchService) CreateCredentialSandbox(projectID, name, parentID string, expires time.Time) (*wal.Branch, error) {
	return s.createBranch(projectID, name, parentID, &expires)
}

func (s *BranchService) createBranch(projectID, name, parentID string, credentialExpiry *time.Time) (*wal.Branch, error) {
	// The usual answer for a name in use. Creates that arrive together all
	// pass this check; the unique index decides among them in insertBranch.
	existing, _ := s.GetBranch(projectID, name)
	if existing != nil {
		return nil, ErrBranchExists
	}

	// Get parent branch if specified
	var parentBranch *wal.Branch
	if parentID != "" {
		var err error
		parentBranch, err = s.GetBranchByID(parentID)
		if err != nil {
			return nil, fmt.Errorf("parent branch not found: %w", err)
		}
	}

	// The branch ID is generated up front so the WAL entry can reference
	// it; data entries key on branch IDs, so control entries must too.
	branchID := primitive.NewObjectID().Hex()

	entry := func() *wal.Entry {
		return &wal.Entry{
			ProjectID: projectID,
			BranchID:  branchID,
			Operation: wal.OpCreateBranch,
			Metadata: map[string]interface{}{
				"branch_name": name,
				"parent_id":   parentID,
			},
		}
	}
	branch := &wal.Branch{
		ID:        branchID,
		ProjectID: projectID,
		Name:      name,
		ParentID:  parentID,
		CreatedAt: time.Now(),
		IsDeleted: false,
	}
	if credentialExpiry != nil {
		branch.SandboxUsername = "sandbox_" + branch.ID
		branch.ExpiresAt = credentialExpiry
	}
	if parentBranch != nil {
		branch.HeadLSN = parentBranch.HeadLSN // Inherit parent's HEAD
		branch.BaseLSN = parentBranch.HeadLSN // Fork point
	}
	err := s.insertBranch(branch, entry, func(lsn int64) bson.M {
		branch.CreatedLSN = lsn
		fields := bson.M{"created_lsn": lsn}
		if parentBranch == nil {
			// A branch with no parent starts at its own create record.
			branch.HeadLSN = lsn
			fields["head_lsn"] = lsn
		}
		return fields
	})
	if err != nil {
		return nil, err
	}
	return branch, nil
}

// GetBranch retrieves a branch by project ID and name
func (s *BranchService) GetBranch(projectID, name string) (*wal.Branch, error) {
	ctx := context.Background()
	var branch wal.Branch

	err := s.collection.FindOne(ctx, bson.M{
		"project_id": projectID,
		"name":       name,
		"is_deleted": false,
	}).Decode(&branch)

	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, ErrBranchNotFound
		}
		return nil, err
	}

	return &branch, nil
}

// GetBranchByID retrieves a branch by its ID
func (s *BranchService) GetBranchByID(branchID string) (*wal.Branch, error) {
	return s.GetBranchByIDContext(context.Background(), branchID)
}

// GetBranchByIDContext retrieves a branch using the caller's context.
func (s *BranchService) GetBranchByIDContext(ctx context.Context, branchID string) (*wal.Branch, error) {
	var branch wal.Branch

	err := s.collection.FindOne(ctx, bson.M{
		"_id":        branchID,
		"is_deleted": false,
	}).Decode(&branch)

	if err != nil {
		return nil, err
	}

	return &branch, nil
}

// GetBranchByIDAny retrieves a branch by its ID regardless of deletion
// state. Ancestry resolution uses this: a force-deleted parent must still
// anchor its descendants' history, whose entries remain in the WAL.
func (s *BranchService) GetBranchByIDAny(branchID string) (*wal.Branch, error) {
	return s.GetBranchByIDAnyContext(context.Background(), branchID)
}

// GetBranchByIDAnyContext includes deleted ancestors and respects the caller's deadline.
func (s *BranchService) GetBranchByIDAnyContext(ctx context.Context, branchID string) (*wal.Branch, error) {
	var branch wal.Branch

	err := s.collection.FindOne(ctx, bson.M{"_id": branchID}).Decode(&branch)
	if err != nil {
		return nil, err
	}

	return &branch, nil
}

// SetExpiry stamps (or clears, with nil) a branch's sandbox TTL.
func (s *BranchService) SetExpiry(branchID string, expiresAt *time.Time) error {
	ctx := context.Background()
	update := bson.M{"$unset": bson.M{"expires_at": ""}}
	if expiresAt != nil {
		update = bson.M{"$set": bson.M{"expires_at": *expiresAt}}
	}
	result, err := s.collection.UpdateOne(ctx, bson.M{"_id": branchID, "sandbox_revoking": bson.M{"$ne": true}}, update)
	if err == nil && result.MatchedCount == 0 {
		// Historically updating an absent branch was a no-op.
		err = s.collection.FindOne(ctx, bson.M{"_id": branchID, "sandbox_revoking": true}).Err()
		if err == nil {
			return ErrBranchChanged
		}
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
	}
	return err
}

// SetCheckoutState records (or clears, with empty values) a branch's
// physical-database checkout.
func (s *BranchService) SetCheckoutState(branchID, physicalDB, state string, checkedOutLSN int64) error {
	ctx := context.Background()
	update := bson.M{}
	update["$inc"] = bson.M{"mutation_version": 1}
	if physicalDB == "" && state == "" {
		update["$unset"] = bson.M{"physical_db": "", "state": "", "checked_out_lsn": ""}
	} else {
		update["$set"] = bson.M{
			"physical_db":     physicalDB,
			"state":           state,
			"checked_out_lsn": checkedOutLSN,
		}
	}
	_, err := s.collection.UpdateOne(ctx, bson.M{"_id": branchID}, update)
	return err
}

// CompareAndSetCheckout publishes a materialization only if the branch is
// still the exact metadata state from which it was built.
func (s *BranchService) CompareAndSetCheckout(ctx context.Context, expected *wal.Branch, physicalDB string, checkedOutLSN int64) error {
	filter := bson.M{"_id": expected.ID, "head_lsn": expected.HeadLSN, "is_deleted": false, "follows": bson.M{"$in": bson.A{nil, ""}},
		"mutation_version": mutationVersionCondition(expected.MutationVersion), "state": bson.M{"$ne": wal.BranchStateLive}}
	if expected.SandboxUsername != "" {
		filter["sandbox_revoking"] = bson.M{"$ne": true}
	}
	res, err := s.collection.UpdateOne(ctx, filter, bson.M{"$set": bson.M{
		"physical_db": physicalDB, "state": wal.BranchStateLive, "checked_out_lsn": checkedOutLSN},
		"$inc": bson.M{"mutation_version": 1}})
	if err != nil {
		return err
	}
	if res.MatchedCount != 1 {
		return ErrBranchChanged
	}
	return nil
}

// ListBranches lists all branches for a project
func (s *BranchService) ListBranches(projectID string) ([]*wal.Branch, error) {
	return s.ListBranchesContext(context.Background(), projectID)
}

// ListBranchesContext lists non-deleted branches using the caller's context.
func (s *BranchService) ListBranchesContext(ctx context.Context, projectID string) ([]*wal.Branch, error) {
	cursor, err := s.collection.Find(ctx, bson.M{
		"project_id": projectID,
		"is_deleted": false,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var branches []*wal.Branch
	if err := cursor.All(ctx, &branches); err != nil {
		return nil, err
	}

	return branches, nil
}

// ListBranchesAny lists all branches for a project including deleted ones.
// Deleted branches can still anchor live descendants' history, so tools
// that walk every timeline (e.g. WAL migration) must see them.
func (s *BranchService) ListBranchesAny(projectID string) ([]*wal.Branch, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{"project_id": projectID})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var branches []*wal.Branch
	if err := cursor.All(ctx, &branches); err != nil {
		return nil, err
	}

	return branches, nil
}

// DeleteBranch deletes a branch (simple version - just removes the pointer)
func (s *BranchService) DeleteBranch(projectID, name string) error {
	branch, err := s.GetBranch(projectID, name)
	if err != nil {
		return err
	}
	return s.deleteBranch(branch)
}

// DeleteBranchByID deletes the branch with this ID. A caller that holds a
// branch must delete it this way: by now its name may belong to another
// branch, made after this one was deleted.
func (s *BranchService) DeleteBranchByID(branchID string) error {
	branch, err := s.GetBranchByID(branchID)
	if err != nil {
		return notFound(err)
	}
	return notFound(s.deleteBranch(branch))
}

func (s *BranchService) deleteBranch(branch *wal.Branch) error {
	ctx := context.Background()
	projectID := branch.ProjectID
	if err := s.RequireDeletable(branch.ID); err != nil {
		return err
	}
	if branch.SandboxUsername != "" && (!branch.SandboxRevoking || branch.PhysicalDB != "") {
		return fmt.Errorf("discard sandbox %q before deleting the branch", branch.Name)
	}

	// Create WAL entry for deletion
	entry := &wal.Entry{
		ProjectID: projectID,
		BranchID:  branch.ID,
		Operation: wal.OpDeleteBranch,
		Metadata:  map[string]interface{}{"branch_id": branch.ID, "final_lsn": branch.HeadLSN},
	}
	if _, err := s.wal.Append(entry); err != nil {
		return fmt.Errorf("failed to append WAL entry: %w", err)
	}
	if _, err := s.collection.UpdateOne(ctx, bson.M{"_id": branch.ID}, bson.M{"$set": bson.M{"is_deleted": true}}); err != nil {
		return err
	}
	if s.onDelete != nil {
		s.onDelete(branch.ID)
	}
	return nil
}

// RequireDeletable performs the same main/child/pin checks as deletion,
// before a caller discards a physical database or stops its capture worker.
func (s *BranchService) RequireDeletable(branchID string) error {
	ctx := context.Background()
	branch, err := s.GetBranchByID(branchID)
	if err != nil {
		return err
	}

	// Validate it's not the main branch (unless force is specified)
	if branch.Name == "main" {
		return errors.New("cannot delete main branch")
	}

	// Check for child branches
	childCount, err := s.collection.CountDocuments(ctx, bson.M{
		"project_id": branch.ProjectID,
		"parent_id":  branch.ID,
		"is_deleted": false,
	})
	if err != nil {
		return err
	}
	if childCount > 0 {
		return errors.New("cannot delete branch with active children")
	}

	if s.deleteGuard != nil {
		if err := s.deleteGuard(branch.ID); err != nil {
			return fmt.Errorf("cannot delete branch: %w", err)
		}
	}

	return nil
}

// UpdateBranchHead advances the head LSN of a branch. $max keeps the head
// monotonic under concurrent writers: with $set, a writer holding a smaller
// LSN could land after one holding a larger LSN and move the head backwards,
// hiding already-written entries from materialization. To move a head
// backwards deliberately (restore/reset), use ResetHead.
func (s *BranchService) UpdateBranchHead(branchID string, newLSN int64) error {
	return s.UpdateBranchHeadContext(context.Background(), branchID, newLSN)
}

func (s *BranchService) UpdateBranchHeadContext(ctx context.Context, branchID string, newLSN int64) error {
	_, err := s.collection.UpdateOne(ctx,
		bson.M{"_id": branchID},
		bson.M{"$max": bson.M{"head_lsn": newLSN}, "$inc": bson.M{"mutation_version": 1}},
	)
	return err
}

// CompareAndSetHead fences a mutation against the state it was prepared
// from. It must be used in the same transaction as the mutation's WAL.
func (s *BranchService) CompareAndSetHead(ctx context.Context, branchID string, expected, next int64) error {
	result, err := s.collection.UpdateOne(ctx, bson.M{"_id": branchID, "head_lsn": expected, "is_deleted": false},
		bson.M{"$set": bson.M{"head_lsn": next}, "$inc": bson.M{"mutation_version": 1}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return ErrBranchChanged
	}
	return nil
}

// FenceBranch locks an exact branch incarnation inside a transaction. Empty
// checkout fields match missing legacy fields as well as explicit empties.
func (s *BranchService) FenceBranch(ctx context.Context, branch *wal.Branch) error {
	filter := bson.M{"_id": branch.ID, "head_lsn": branch.HeadLSN, "is_deleted": false}
	filter["mutation_version"] = mutationVersionCondition(branch.MutationVersion)
	if branch.IsLive() {
		filter["state"] = wal.BranchStateLive
		filter["physical_db"] = branch.PhysicalDB
	} else {
		filter["state"] = bson.M{"$ne": wal.BranchStateLive}
	}
	res, err := s.collection.UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"mutation_version": 1}})
	if err != nil {
		return err
	}
	if res.MatchedCount != 1 {
		return ErrBranchChanged
	}
	return nil
}

func mutationVersionCondition(version int64) interface{} {
	if version == 0 {
		return bson.M{"$in": bson.A{nil, int64(0)}}
	}
	return version
}

// ResetHead atomically records the abandoned interval and rewinds the head.
// It is the only way a head moves backwards, so every rewind leaves a
// discarded range behind. Live branches must first be drained/released:
// resetting only metadata while applications continue writing would expose
// two conflicting states.
//
// The recorded window runs to the history high-water mark rather than to
// the current head. After two resets in a row the head is already below the
// first window; ending the second window at that head would make it look
// as if views captured inside the first window — a backup branch, a pin —
// came after the second reset, and they would silently lose the history
// between the two targets.
func (s *BranchService) ResetHead(branch *wal.Branch, target int64) error {
	if branch.IsFollower() {
		return branch.RefuseFollowerWrite("reset")
	}
	if branch.IsLive() {
		return fmt.Errorf("cannot reset a checked-out branch; release it after capture has drained first")
	}
	if target != branch.HeadLSN && target >= branch.BaseLSN {
		if err := branch.ValidateResetTarget(target); err != nil {
			return err
		}
	}
	if target < branch.BaseLSN || target > branch.HeadLSN {
		return fmt.Errorf("invalid reset target %d", target)
	}
	update := bson.M{"$set": bson.M{"head_lsn": target}, "$inc": bson.M{"mutation_version": 1}}
	if target < branch.HeadLSN {
		update["$push"] = bson.M{"discarded_ranges": wal.LSNRange{From: target + 1, To: branch.HistoryHighWater()}}
	}
	result, err := s.collection.UpdateOne(context.Background(), bson.M{"_id": branch.ID, "head_lsn": branch.HeadLSN, "is_deleted": false,
		"mutation_version": mutationVersionCondition(branch.MutationVersion),
		"follows":          bson.M{"$in": bson.A{nil, ""}},
		"state":            bson.M{"$ne": wal.BranchStateLive}}, update)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("branch changed concurrently; reload and retry reset")
	}
	return nil
}

// GetChildBranches returns all child branches of a given branch
func (s *BranchService) GetChildBranches(parentID string) ([]*wal.Branch, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{
		"parent_id":  parentID,
		"is_deleted": false,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var branches []*wal.Branch
	if err := cursor.All(ctx, &branches); err != nil {
		return nil, err
	}

	return branches, nil
}

// ForceDeleteBranch deletes a branch without safety checks (for project deletion)
func (s *BranchService) ForceDeleteBranch(projectID, name string) error {
	ctx := context.Background()

	// Get branch
	branch, err := s.GetBranch(projectID, name)
	if err != nil {
		return err
	}
	if branch.SandboxUsername != "" {
		return fmt.Errorf("discard sandbox %q before deleting the branch", branch.Name)
	}

	// Create WAL entry for deletion
	entry := &wal.Entry{
		ProjectID: projectID,
		BranchID:  name,
		Operation: wal.OpDeleteBranch,
		Metadata: map[string]interface{}{
			"branch_id": branch.ID,
			"final_lsn": branch.HeadLSN,
			"forced":    true,
		},
	}

	_, err = s.wal.Append(entry)
	if err != nil {
		return fmt.Errorf("failed to append WAL entry: %w", err)
	}

	// Mark as deleted
	_, err = s.collection.UpdateOne(ctx,
		bson.M{"_id": branch.ID},
		bson.M{"$set": bson.M{"is_deleted": true}},
	)

	return err
}

// CreateBranchWithData creates a branch with specific metadata
func (s *BranchService) CreateBranchWithData(branch *wal.Branch) error {
	// Check if branch already exists
	existing, _ := s.GetBranch(branch.ProjectID, branch.Name)
	if existing != nil {
		return ErrBranchExists
	}

	// Keyed by the branch ID, as every record of a branch is: the name can
	// belong to another branch later.
	entry := func() *wal.Entry {
		return &wal.Entry{
			ProjectID: branch.ProjectID,
			BranchID:  branch.ID,
			Operation: wal.OpCreateBranch,
			Metadata: map[string]interface{}{
				"branch_name": branch.Name,
				"base_lsn":    branch.BaseLSN,
				"head_lsn":    branch.HeadLSN,
			},
		}
	}
	if branch.ID == "" {
		branch.ID = primitive.NewObjectID().Hex()
	}
	branch.IsDeleted = false
	if branch.CreatedAt.IsZero() {
		branch.CreatedAt = time.Now()
	}
	return s.insertBranch(branch, entry, func(lsn int64) bson.M {
		branch.CreatedLSN = lsn
		return bson.M{"created_lsn": lsn}
	})
}

// SetFollows marks a branch as following an external database, or clears the
// mark with an empty name. A checked-out branch cannot become a follower: it
// already has a writer.
func (s *BranchService) SetFollows(ctx context.Context, branchID, database string) error {
	filter := bson.M{"_id": branchID, "is_deleted": false}
	update := bson.M{"$unset": bson.M{"follows": ""}, "$inc": bson.M{"mutation_version": 1}}
	if database != "" {
		filter["state"] = bson.M{"$ne": wal.BranchStateLive}
		update = bson.M{"$set": bson.M{"follows": database}, "$inc": bson.M{"mutation_version": 1}}
	}
	result, err := s.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return ErrBranchChanged
	}
	return nil
}
