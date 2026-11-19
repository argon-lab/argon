package merge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/mongoexpr"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// Write-back: applying a reviewed plan to the external database its target
// branch follows.
//
// The plan is written to the source and nowhere else. The followed branch is
// not appended to: it learns of the change the way it learns of every other
// change to the source, from the change stream. That keeps one writer per
// branch and means there is nothing to reconcile if the two deployments
// disagree about what happened.
//
// The write is one transaction on the source. Inside it every document is
// read and compared with the before-image the plan was reviewed against,
// exactly as a merge into a checked-out branch does; the first document that
// differs aborts the transaction and nothing is written. A plan does not
// have to match the target's head, which moves with every production write:
// the per-document comparison is the staleness check, and it is made at the
// only moment that counts.
//
// The source and the Argon metadata can be different deployments, so the
// plan's status cannot commit atomically with the documents. The order is:
// claim the plan in the metadata (pending → writing_back) with the ID of a
// session used for nothing else, run the source transaction in that session,
// mark the plan applied. If the process dies between the last two steps the
// plan stays writing_back, cannot be claimed again, and is closed by the
// follower when the stream delivers a transaction from that session. A plan
// is never written twice: after the claim it is no longer pending, and any
// new plan is checked against the documents as they then are.

// StatusWritingBack marks a plan claimed for write-back whose source
// transaction has not been confirmed in the metadata.
const StatusWritingBack = "writing_back"

// A writer gets two minutes from before claim acquisition, including all
// source transaction and commit retries. The five-minute detach window also
// covers a commit already sent with the two-minute server execution limit.
const (
	writeBackDeadline    = 2 * time.Minute
	writeBackClaimWindow = 5 * time.Minute
)

const writingBackAdvice = "This plan will not be written again. Check the source database, let following catch up, then run `argon merge preview` again."

// WritingBackAdvice tells operators how to proceed with an unsettled claim.
func (plan *Plan) WritingBackAdvice() string {
	if plan.Status == StatusWritingBack {
		return writingBackAdvice
	}
	return ""
}

// WriteBackRecord is the audit trail of a plan's write-back.
type WriteBackRecord struct {
	// Session is the source session the transaction ran in. Change events
	// from that session are this plan's.
	Session   primitive.Binary `bson:"session" json:"-"`
	Database  string           `bson:"database" json:"database"`
	Display   string           `bson:"display,omitempty" json:"display,omitempty"`
	Strategy  string           `bson:"strategy,omitempty" json:"strategy,omitempty"`
	Changes   int              `bson:"changes" json:"changes"`
	StartedAt time.Time        `bson:"started_at" json:"started_at"`
	// CommittedAt is set by the process that ran the transaction, SettledAt
	// by the follower that saw it in the stream. Either proves the commit.
	CommittedAt *time.Time `bson:"committed_at,omitempty" json:"committed_at,omitempty"`
	SettledAt   *time.Time `bson:"settled_at,omitempty" json:"settled_at,omitempty"`
}

// ConflictError reports that a source document no longer matches the plan.
// Nothing was written.
type ConflictError struct {
	Collection string
	DocumentID string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("write-back aborted, nothing was written: document %s/%s in the source database differs from the state this plan was reviewed against. Let following catch up and run preview again", e.Collection, e.DocumentID)
}

// WriteBackTarget is the source database as the caller connected to it. The
// caller has already checked that it is the deployment the branch follows.
type WriteBackTarget struct {
	Database *mongo.Database
	Display  string
	// Redact cleans error text before it is stored with the plan. The
	// caller knows the connection string; this package never sees it.
	Redact func(string) string
}

// EnsureIndexes creates the index the follower uses to recognise write-back
// sessions.
func (s *Service) EnsureIndexes(ctx context.Context) error {
	_, err := s.plans.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "write_back.session", Value: 1}},
		Options: options.Index().SetSparse(true),
	})
	return err
}

// resolveChanges is the plan's change set with its conflicts settled by the
// given strategy.
func resolveChanges(plan *Plan, strategy string) ([]Change, int, error) {
	changes := append([]Change{}, plan.Changes...)
	resolved := 0
	if len(plan.Conflicts) > 0 {
		switch strategy {
		case StrategyTheirs:
			for _, c := range plan.Conflicts {
				change := Change{
					Collection:  c.Collection,
					DocumentID:  c.DocumentID,
					Delete:      c.Theirs == nil,
					Document:    c.Theirs,
					Before:      c.Ours,
					DocumentRaw: c.TheirsRaw,
					BeforeRaw:   c.OursRaw,
				}
				if slices.Contains(c.SortedImages, "theirs") {
					change.SortedImages = append(change.SortedImages, "document")
				}
				if slices.Contains(c.SortedImages, "ours") {
					change.SortedImages = append(change.SortedImages, "before")
				}
				changes = append(changes, change)
				resolved++
			}
			sortChanges(changes)
		case StrategyOurs:
			resolved = len(plan.Conflicts) // Keep ours: nothing to write.
		case "":
			return nil, 0, fmt.Errorf("plan has %d conflict(s); pass a strategy (theirs/ours) to resolve them", len(plan.Conflicts))
		default:
			return nil, 0, fmt.Errorf("unknown strategy %q (want theirs or ours)", strategy)
		}
	}
	return changes, resolved, nil
}

// ApplyToSource writes a pending plan to the external database its target
// branch follows. It is never reached implicitly: the caller names it.
func (s *Service) ApplyToSource(ctx context.Context, planID primitive.ObjectID, strategy string, target WriteBackTarget) (*ApplyResult, error) {
	if err := ValidateStrategy(strategy); err != nil {
		return nil, err
	}
	if err := source.RequireExperimentalWriteBack(); err != nil {
		return nil, err
	}
	plan, err := s.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if err := plan.RequirePending(); err != nil {
		return nil, err
	}
	if plan.FormatVersion < 2 {
		return nil, errors.New("this plan predates before-images and cannot be checked against the source; run preview again")
	}

	var databaseName string
	if target.Database != nil {
		databaseName = target.Database.Name()
	}
	branch, err := s.admitWriteBack(ctx, plan.TargetBranchID, databaseName)
	if err != nil {
		return nil, err
	}
	source, err := s.branches.GetBranchByID(plan.SourceBranchID)
	if err != nil {
		return nil, fmt.Errorf("source branch not found: %w", err)
	}
	if source.HeadLSN != plan.SourceHead || source.MutationVersion != plan.SourceVersion {
		return nil, fmt.Errorf("stale plan: branch %s changed since preview; run preview again", source.Name)
	}

	changes, resolved, err := resolveChanges(plan, strategy)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if _, err := documentIDValue(c); err != nil {
			return nil, err
		}
	}

	if len(changes) == 0 {
		if err := s.claimWriteBack(ctx, plan, databaseName, nil, strategy); err != nil {
			return nil, err
		}
		return &ApplyResult{ConflictsResolved: resolved}, nil
	}

	// Write-back changes documents in collections the source already has.
	// It does not create one: a collection made by an upsert would exist in
	// production with default options and a single document, and one that
	// production dropped would come back. Checked here, before the plan is
	// claimed, so that a refusal leaves the plan pending.
	existing, err := target.Database.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("failed to list the source's collections: %w", err)
	}
	present := make(map[string]bool, len(existing))
	for _, name := range existing {
		present[name] = true
	}
	writes := make([]sourceWrite, len(changes))
	for i, c := range changes {
		if !present[c.Collection] && !c.Delete {
			return nil, fmt.Errorf("collection %s does not exist in the source database, and write-back does not create collections. Create it there with the options it should have, let following catch up, and preview again", c.Collection)
		}
		if writes[i], err = s.sourceWriteFor(source, branch, plan, c); err != nil {
			return nil, err
		}
	}

	writerCtx, stopWriter := context.WithTimeout(ctx, writeBackDeadline)
	session, err := target.Database.Client().StartSession()
	if err != nil {
		stopWriter()
		return nil, err
	}
	defer func() {
		// A timed-out commit can leave the v1 session locally in progress.
		// Abort only before a commit attempt, never during session cleanup.
		stopWriter()
		session.EndSession(writerCtx)
	}()
	subtype, data, ok := session.ID().Lookup("id").BinaryOK()
	if !ok {
		return nil, errors.New("the source session has no identifier to record")
	}
	record := &WriteBackRecord{
		Session:  primitive.Binary{Subtype: subtype, Data: data},
		Database: branch.Follows, Display: boundedAuditText(target.Display, maxDisplayBytes),
		Strategy: strategy, Changes: len(changes),
	}
	// A metadata election must not undo the claim after the separate
	// source transaction commits. Require majority acknowledgement for
	// the claim and both transitions out of it.
	plans := s.plans.Database().Collection(s.plans.Name(), options.Collection().SetWriteConcern(writeconcern.Majority()))
	if err := s.claimWriteBack(writerCtx, plan, databaseName, record, strategy); err != nil {
		return nil, err
	}

	uncertain, txnErr := writeSourceTransaction(writerCtx, session, target.Database, writes)

	// The outcome is recorded even if the caller's context has ended.
	record2, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if txnErr == nil {
		now := time.Now()
		// The follower can settle this same claim before we record the commit.
		marked, err := plans.UpdateOne(record2, bson.M{
			"_id": plan.ID, "write_back.session": record.Session,
			"$or": bson.A{
				bson.M{"status": StatusWritingBack},
				bson.M{"status": StatusApplied, "write_back.settled_at": bson.M{"$exists": true}},
			},
		}, mongo.Pipeline{{{Key: "$set", Value: bson.M{
			"status":                  StatusApplied,
			"strategy":                bson.M{"$literal": strategy},
			"applied_at":              bson.M{"$ifNull": bson.A{"$applied_at", now}},
			"write_back.committed_at": now,
		}}}})
		if err == nil && marked.MatchedCount != 1 {
			err = errors.New("the plan record no longer matches the write-back claim")
		}
		result := &ApplyResult{Applied: len(changes), ConflictsResolved: resolved}
		if err != nil {
			return result, fmt.Errorf("the source transaction committed, but plan %s could not be marked applied: %w", plan.ID.Hex(), err)
		}
		return result, nil
	}

	if uncertain {
		return nil, fmt.Errorf("the source commit for plan %s did not report an outcome (%w). The plan stays %s unless following sees the transaction and marks it applied. %s", plan.ID.Hex(), txnErr, StatusWritingBack, writingBackAdvice)
	}
	// Nothing was written. Return the plan to pending so that a transient
	// failure can be retried; a conflict will fail the same way until the
	// plan is previewed again.
	reason := txnErr.Error()
	if target.Redact != nil {
		reason = target.Redact(reason)
	}
	marked, err := plans.UpdateOne(record2, bson.M{"_id": plan.ID, "status": StatusWritingBack, "write_back.session": record.Session, "$expr": recentWriteBackClaim()},
		bson.M{"$set": bson.M{"status": StatusPending, "write_back_last_error": boundedAuditText(reason, maxLastErrorBytes)}, "$unset": bson.M{"write_back": ""}})
	if err != nil {
		return nil, errors.Join(txnErr, fmt.Errorf("nothing was written, but the plan could not be returned to pending: %w", err))
	}
	if marked.MatchedCount != 1 {
		return nil, errors.Join(txnErr, errors.New("the claim could not be released. Inspect the source and let following settle the claim"))
	}
	return nil, txnErr
}

// The driver's WithTransaction detaches commit from the caller's deadline.
// Drive the same body/commit retry decisions with one deadline for all attempts.
// Once a commit has been sent, only a definitive abort can release the claim.
func writeSourceTransaction(ctx context.Context, session mongo.Session, db *mongo.Database, writes []sourceWrite) (uncertain bool, err error) {
	maxCommit := writeBackDeadline
	opts := options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()).
		SetReadPreference(readpref.Primary()).SetMaxCommitTime(&maxCommit)
	for ctx.Err() == nil {
		if err := session.StartTransaction(opts); err != nil {
			return false, err
		}
		err := writeChanges(mongo.NewSessionContext(ctx, session), db, writes)
		if err == nil {
			err = ctx.Err()
		}
		commitAttempted := err == nil
		if !commitAttempted {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = session.AbortTransaction(abortCtx)
			cancel()
		} else {
			for {
				err = session.CommitTransaction(ctx)
				if err == nil {
					return false, nil
				}
				var command mongo.CommandError
				if !errors.As(err, &command) || !command.HasErrorLabel("UnknownTransactionCommitResult") {
					break
				}
				if command.IsMaxTimeMSExpiredError() || ctx.Err() != nil {
					return true, err
				}
			}
		}
		if !transientTransaction(err) {
			return commitAttempted, err
		}
	}
	return false, ctx.Err()
}

func transientTransaction(err error) bool {
	var labeled mongo.LabeledError
	return errors.As(err, &labeled) && labeled.HasErrorLabel("TransientTransactionError")
}

func (s *Service) admitWriteBack(ctx context.Context, branchID, databaseName string) (*wal.Branch, error) {
	branch, status, err := ingest.ReadCaptureStatus(ctx, s.plans.Database(), branchID)
	if err != nil {
		return nil, fmt.Errorf("target branch not found: %w", err)
	}
	if !branch.IsFollower() {
		return branch, fmt.Errorf("branch %s does not follow an external database; apply the plan without write-back", branch.Name)
	}
	if err := status.Health.WriteBackAdmission(); err != nil {
		return nil, err
	}
	if databaseName != branch.Follows {
		return nil, fmt.Errorf("branch %s follows database %q; write-back was pointed at another database", branch.Name, branch.Follows)
	}
	return branch, nil
}

// claimWriteBack checks the capture checkpoint and consumes the pending plan
// in one metadata snapshot. An empty change set is complete at that commit.
func (s *Service) claimWriteBack(ctx context.Context, plan *Plan, databaseName string, record *WriteBackRecord, strategy string) error {
	session, err := s.plans.Database().Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(context.Background())
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (any, error) {
		branch, err := s.admitWriteBack(sc, plan.TargetBranchID, databaseName)
		if err != nil {
			if branch != nil && !branch.IsFollower() {
				return nil, errors.New("following was detached")
			}
			return nil, err
		}
		if err := source.FenceWriteBackClaim(sc, s.plans.Database(), plan.TargetBranchID, databaseName); err != nil {
			return nil, err
		}
		fields := bson.M{"status": StatusWritingBack, "write_back": bson.M{"$literal": record}, "write_back_last_error": "$$REMOVE"}
		update := mongo.Pipeline{{{Key: "$set", Value: fields}}}
		if record == nil {
			fields["status"], fields["write_back"] = StatusApplied, "$$REMOVE"
			fields["applied_at"], fields["strategy"] = "$$NOW", bson.M{"$literal": strategy}
		} else {
			update = append(update, bson.D{{Key: "$set", Value: bson.M{"write_back.started_at": "$$NOW"}}})
		}
		result, err := s.plans.UpdateOne(sc, bson.M{"_id": plan.ID, "status": StatusPending}, update)
		if err == nil && result.MatchedCount != 1 {
			err = errors.New("merge plan is no longer pending")
		}
		return nil, err
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetReadPreference(readpref.Primary()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		return fmt.Errorf("could not claim plan %s with majority write concern: %w", plan.ID.Hex(), err)
	}
	return nil
}

func recentWriteBackClaim() bson.M {
	return bson.M{"$gt": bson.A{"$write_back.started_at", bson.M{"$subtract": bson.A{"$$NOW", writeBackClaimWindow.Milliseconds()}}}}
}

// ClaimsForDetach checks claims in the same metadata transaction that removes
// the binding. It returns the IDs that will need manual inspection after detach.
func (s *Service) ClaimsForDetach(ctx context.Context, branchID string) ([]string, error) {
	cursor, err := s.plans.Find(ctx, bson.M{"target_branch_id": branchID, "status": StatusWritingBack},
		options.Find().SetProjection(bson.M{"_id": 1, "recent": recentWriteBackClaim()}).SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	var recent, unsettled []string
	for cursor.Next(ctx) {
		var row struct {
			ID     primitive.ObjectID `bson:"_id"`
			Recent bool               `bson:"recent"`
		}
		if err := cursor.Decode(&row); err != nil {
			return nil, err
		}
		if row.Recent {
			recent = append(recent, row.ID.Hex())
		} else {
			unsettled = append(unsettled, row.ID.Hex())
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	if len(recent) != 0 {
		return nil, fmt.Errorf("cannot detach while write-back plans %s are writing_back. Please wait for the writer or for the follower to settle the claim", strings.Join(recent, ", "))
	}
	return unsettled, nil
}

// sourceWrite is one planned change prepared for the source transaction.
type sourceWrite struct {
	change Change
	id     interface{}
	// before is the document as the followed branch stored it when the plan
	// was made, byte for byte, when that is known. The comparison then sees
	// field order too.
	before bson.Raw
	// A current plan supplies raw BSON. An older plan supplies a bson.D
	// to be ordered against the existing source document on replacement.
	document any
}

// sourceWriteFor resolves what the comparison and the write need from the
// two branches' histories, before the transaction starts.
func (s *Service) sourceWriteFor(source, target *wal.Branch, plan *Plan, c Change) (sourceWrite, error) {
	id, err := documentIDValue(c)
	if err != nil {
		return sourceWrite{}, err
	}
	write := sourceWrite{change: c, id: id}
	if c.Before != nil {
		// A followed branch stores the source's own bytes.
		entry, err := s.storedImage(target, plan.TargetHead, c.Collection, c.DocumentID, c.Before)
		if err != nil {
			return sourceWrite{}, fmt.Errorf("could not read write-back before-image for %s/%s: %w", c.Collection, c.DocumentID, err)
		}
		// GC publishes the floor before deleting entries. At or below it,
		// a missing or surviving image cannot prove which bytes were latest.
		floor, err := s.wal.ReclaimFloor(target.ID, c.Collection)
		if err != nil {
			return sourceWrite{}, fmt.Errorf("could not check write-back before-image history for %s/%s: %w", c.Collection, c.DocumentID, err)
		}
		if floor > target.BaseLSN && (entry == nil || entry.LSN <= floor) {
			return sourceWrite{}, fmt.Errorf("the exact write-back before-image for %s/%s at LSN %d may have been reclaimed: %w", c.Collection, c.DocumentID, plan.TargetHead, wal.ErrHistoryReclaimed)
		}
		// An unverified change-stream image may contain a moved field
		// twice. It records the decoded values but lacks the exact bytes.
		if plan.FormatVersion >= orderedPlanVersion && !slices.Contains(c.SortedImages, "before") {
			write.before = c.BeforeRaw
		} else if entry != nil && entry.Metadata["capture_images"] != "attached_unverified" {
			if plan.FormatVersion >= orderedPlanVersion && ingest.HasDuplicateFields(entry.PostImage) {
				return sourceWrite{}, fmt.Errorf("write-back before-image for %s/%s contains duplicate fields; run preview again after capturing an unambiguous document", c.Collection, c.DocumentID)
			}
			write.before = entry.PostImage
		}
	}
	if c.Delete {
		return write, nil
	}
	if plan.FormatVersion >= orderedPlanVersion {
		if c.DocumentRaw == nil || slices.Contains(c.SortedImages, "document") {
			return sourceWrite{}, fmt.Errorf("no verified write-back replacement for %s/%s; run a new preview after the sandbox history is intact", c.Collection, c.DocumentID)
		}
		storedID, err := wal.DocumentIDFromImage(c.DocumentRaw)
		if err != nil || wal.DocumentIDString(storedID) != c.DocumentID {
			return sourceWrite{}, fmt.Errorf("write-back replacement for %s/%s has a different _id; run preview again", c.Collection, c.DocumentID)
		}
		write.document = c.DocumentRaw
		return write, nil
	}
	// The replacement needs the sandbox's field order. A map in the
	// plan also cannot distinguish a repeated field from its last value.
	entry, err := s.storedImage(source, plan.SourceHead, c.Collection, c.DocumentID, c.Document)
	if err != nil {
		return sourceWrite{}, fmt.Errorf("could not read write-back replacement for %s/%s: %w", c.Collection, c.DocumentID, err)
	}
	floor, err := s.wal.ReclaimFloor(source.ID, c.Collection)
	if err != nil {
		return sourceWrite{}, err
	}
	if floor > source.BaseLSN && (entry == nil || entry.LSN <= floor) {
		return sourceWrite{}, fmt.Errorf("write-back replacement for %s/%s: %w; run a new preview after the sandbox history is intact", c.Collection, c.DocumentID, wal.ErrHistoryReclaimed)
	}
	if !verifiedReplacement(entry) {
		return sourceWrite{}, fmt.Errorf("no verified write-back replacement for %s/%s; run a new preview after the sandbox history is intact", c.Collection, c.DocumentID)
	}
	var document bson.D
	if err := bson.Unmarshal(entry.PostImage, &document); err != nil {
		return sourceWrite{}, err
	}
	ordered := bson.D{{Key: "_id", Value: id}}
	for _, field := range document {
		if field.Key != "_id" {
			ordered = append(ordered, field)
		}
	}
	write.document = ordered
	return write, nil
}

func verifiedReplacement(entry *wal.Entry) bool {
	return entry != nil && len(entry.PostImage) != 0 && entry.Metadata["capture_images"] != "attached_unverified" && !ingest.HasDuplicateFields(entry.PostImage)
}

// storedImage returns the newest visible entry that holds the given document
// at or below head. A missing image requires a successful read.
func (s *Service) storedImage(branch *wal.Branch, head int64, collection, documentID string, want bson.M) (*wal.Entry, error) {
	history, err := s.wal.GetDocumentHistory(branch.ID, collection, documentID, branch.BaseLSN+1, head)
	if err != nil {
		return nil, err
	}
	for i := len(history) - 1; i >= 0; i-- {
		entry := history[i]
		if branch.IsDiscardedForRead(entry.LSN, head) {
			continue
		}
		if !entry.IsData() {
			continue
		}
		if entry.Operation != wal.OpPut || len(entry.PostImage) == 0 {
			return nil, nil
		}
		var stored bson.M
		if err := bson.Unmarshal(entry.PostImage, &stored); err != nil {
			return nil, err
		}
		differs, err := docsUnequal(stored, want)
		if err != nil {
			return nil, err
		}
		if differs {
			return nil, fmt.Errorf("stored image at LSN %d differs from the plan; run preview again", entry.LSN)
		}
		return entry, nil
	}
	return nil, nil
}

// writeChanges is the body of the source transaction: compare, then write,
// one document at a time. It issues single-document inserts, replacements
// and deletes into collections that exist; it never upserts, drops, renames
// or rewrites a collection.
func writeChanges(ctx mongo.SessionContext, database *mongo.Database, writes []sourceWrite) error {
	for _, write := range writes {
		c := write.change
		conflict := &ConflictError{Collection: c.Collection, DocumentID: c.DocumentID}
		coll := database.Collection(c.Collection)
		current, err := coll.FindOne(ctx, bson.M{"_id": write.id}).Raw()
		if err == mongo.ErrNoDocuments {
			current = nil
		} else if err != nil {
			return err
		}
		switch {
		case c.Before == nil || current == nil:
			if c.Before != nil || current != nil {
				return conflict
			}
		case write.before != nil:
			// MongoDB distinguishes documents whose fields are reordered.
			if !bytes.Equal(current, write.before) {
				return conflict
			}
		default:
			// Order-insensitive comparison is reserved for states for
			// which history never recorded exact bytes.
			// A map would discard all but the last value of a repeated
			// field and could authorize overwriting a production change.
			if ingest.HasDuplicateFields(current) {
				return conflict
			}
			var decoded bson.M
			if err := bson.Unmarshal(current, &decoded); err != nil {
				return err
			}
			changed, err := docsUnequal(decoded, c.Before)
			if err != nil {
				return err
			}
			if changed {
				return conflict
			}
		}
		switch {
		case c.Delete:
			result, err := coll.DeleteOne(ctx, bson.M{"_id": write.id})
			if err != nil {
				return err
			}
			if result.DeletedCount != 1 {
				return conflict
			}
		case current == nil:
			if _, err := coll.InsertOne(ctx, write.document); err != nil {
				return err
			}
		default:
			document := write.document
			if legacy, ok := document.(bson.D); ok {
				var existing bson.D
				if err := bson.Unmarshal(current, &existing); err != nil {
					return err
				}
				document = orderLike(existing, legacy)
			}
			result, err := coll.ReplaceOne(ctx, bson.M{"_id": write.id}, document)
			if err != nil {
				return err
			}
			if result.MatchedCount != 1 {
				return conflict
			}
		}
	}
	return nil
}

// orderLike returns want with its fields in the order have has them, at
// every level: a field both documents have keeps its position in have, and
// fields only want has follow in want's order. Inside arrays, only matching
// elements borrow the source's field order, which matters to anything that
// compares embedded documents whole.
func orderLike(have, want bson.D) bson.D {
	wanted := make(map[string]interface{}, len(want))
	for _, field := range want {
		wanted[field.Key] = field.Value
	}
	placed := make(map[string]bool, len(have))
	ordered := make(bson.D, 0, len(want))
	for _, field := range have {
		value, ok := wanted[field.Key]
		if !ok || placed[field.Key] {
			continue
		}
		placed[field.Key] = true
		ordered = append(ordered, bson.E{Key: field.Key, Value: orderValueLike(field.Value, value)})
	}
	for _, field := range want {
		if !placed[field.Key] {
			placed[field.Key] = true
			ordered = append(ordered, field)
		}
	}
	return ordered
}

func orderValueLike(have, want interface{}) interface{} {
	switch wanted := want.(type) {
	case bson.D:
		if had, ok := have.(bson.D); ok {
			return orderLike(had, wanted)
		}
	case bson.A:
		if had, ok := have.(bson.A); ok {
			return orderArrayLike(had, wanted)
		}
	}
	return want
}

func orderArrayLike(have, want bson.A) bson.A {
	// Reserve all exact matches before a fallback can consume an element
	// with the same decoded values and different field order.
	exact := make(map[string][]int)
	for i, item := range have {
		if key := arrayItemExactKey(item); key != "" {
			exact[key] = append(exact[key], i)
		}
	}
	used := make([]bool, len(have))
	wantKeys := make([]string, len(want))
	wantCounts := make(map[string]int)
	for i, item := range want {
		key := arrayItemExactKey(item)
		indices := exact[key]
		if len(indices) > 0 {
			last := len(indices) - 1
			used[indices[last]] = true
			exact[key] = indices[:last]
			continue
		}
		if key := arrayItemKey(item); key != "" {
			wantKeys[i] = key
			wantCounts[key]++
		}
	}
	matches := make(map[string][]int)
	for i, item := range have {
		if used[i] {
			continue
		}
		if key := arrayItemKey(item); wantCounts[key] == 1 {
			matches[key] = append(matches[key], i)
		}
	}
	ordered := append(bson.A{}, want...)
	for i, key := range wantKeys {
		// Only an unambiguous remaining pair borrows the source's order.
		if indices := matches[key]; len(indices) == 1 {
			ordered[i] = orderValueLike(have[indices[0]], want[i])
		}
	}
	return ordered
}

func arrayItemExactKey(item interface{}) string {
	switch item.(type) {
	case bson.D, bson.A:
		typ, value, err := bson.MarshalValue(item)
		if err == nil {
			return typ.String() + string(value)
		}
	}
	return ""
}

// arrayItemKey uses a subdocument's _id if present and otherwise its value
// with document keys sorted for comparison. Scalars have no fields to order.
func arrayItemKey(item interface{}) string {
	switch value := item.(type) {
	case bson.D:
		for _, field := range value {
			if field.Key == "_id" {
				return "id:" + wal.DocumentIDString(field.Value)
			}
		}
	case bson.A:
	default:
		return ""
	}
	value, err := mongoexpr.CanonicalBytes(item)
	if err != nil {
		return ""
	}
	return "value:" + string(value)
}

// LookupWriteBack returns the plan whose write-back ran in a source session,
// or nil. The follower calls it for transactions it has not seen before.
func (s *Service) LookupWriteBack(ctx context.Context, targetBranchID string, session primitive.Binary) (*Plan, error) {
	var plan Plan
	plans := s.plans.Database().Collection(s.plans.Name(), options.Collection().SetReadPreference(readpref.Primary()))
	err := plans.FindOne(ctx, bson.M{"write_back.session": session, "target_branch_id": targetBranchID},
		options.FindOne().SetProjection(bson.M{"changes": 0, "conflicts": 0})).Decode(&plan)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

// SettleWriteBack closes a plan whose transaction the follower has seen in
// the source's change stream. ctx is the transaction that stores its entries.
func (s *Service) SettleWriteBack(ctx context.Context, planID primitive.ObjectID) error {
	now := time.Now()
	// The process that wrote the plan may have recorded the commit already;
	// keep what it recorded and add what the stream confirms.
	_, err := s.plans.UpdateOne(ctx, bson.M{"_id": planID, "write_back.session": bson.M{"$exists": true}},
		mongo.Pipeline{{{Key: "$set", Value: bson.M{
			"status":                StatusApplied,
			"strategy":              bson.M{"$ifNull": bson.A{"$strategy", "$write_back.strategy"}},
			"applied_at":            bson.M{"$ifNull": bson.A{"$applied_at", now}},
			"write_back.settled_at": now,
		}}}})
	return err
}
