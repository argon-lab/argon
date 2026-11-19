package merge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
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
// read as raw BSON and compared byte for byte with the image saved at preview.
// The first difference aborts the entire transaction. A plan does not have to
// match the target's head, which moves with every production write:
// the per-document comparison is the staleness check, and it is made at the
// only moment that counts.
//
// The source and the Argon metadata can be different deployments, so the
// plan's status cannot commit atomically with the documents. The order is:
// claim the plan in the metadata (pending → writing_back) with the source
// session ID and its current transaction number, run the source transaction,
// mark the plan applied. If the process dies between the last two steps the
// plan stays writing_back, cannot be claimed again, and is closed by the
// follower when the stream delivers the claimed transaction. A plan
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
	ApprovedDigest string `bson:"approved_digest,omitempty" json:"-"`
	// StartTransaction increments the pooled session's counter on every attempt.
	// AfterTxn is its value before the first attempt. Older claims omit it.
	Session   primitive.Binary `bson:"session" json:"-"`
	AfterTxn  *int64           `bson:"after_txn,omitempty" json:"-"`
	CommitTxn *int64           `bson:"commit_txn,omitempty" json:"-"`
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

// StoredImageError means MongoDB's final document differs from the approved
// after-image. The transaction has no commit attempt to reconcile.
type StoredImageError struct {
	Collection string
	DocumentID string
}

func (e *StoredImageError) Error() string {
	return fmt.Sprintf("write-back aborted, nothing was written: for %s/%s the server stored something other than the approved image", e.Collection, e.DocumentID)
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

// EnsureIndexes lets the follower seek the nearest preceding claim in a session.
func (s *Service) EnsureIndexes(ctx context.Context) error {
	_, err := s.plans.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "write_back.session", Value: 1}, {Key: "write_back.after_txn", Value: -1}},
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
func (s *Service) ApplyToSource(ctx context.Context, planID primitive.ObjectID, strategy string, target WriteBackTarget, approved string) (*ApplyResult, error) {
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
	if err := plan.CheckApproval(approved); err != nil {
		return nil, err
	}
	changes, resolved, err := resolveChanges(plan, strategy)
	if err != nil {
		return nil, err
	}
	if err := prepareWriteBack(plan, changes); err != nil {
		return nil, err
	}

	var databaseName string
	if target.Database != nil {
		databaseName = target.Database.Name()
	}
	branch, err := s.admitWriteBack(ctx, plan.TargetBranchID, databaseName)
	if err != nil {
		return nil, err
	}
	if plan.FollowedDatabase != databaseName || plan.FollowedReplicaSetID == "" {
		return nil, errors.New("write-back requires the database and replica-set ID recorded at preview; preview again with this version")
	}
	source, err := s.branches.GetBranchByID(plan.SourceBranchID)
	if err != nil {
		return nil, fmt.Errorf("source branch not found: %w", err)
	}
	if source.HeadLSN != plan.SourceHead || source.MutationVersion != plan.SourceVersion {
		return nil, fmt.Errorf("stale plan: branch %s changed since preview; run preview again", source.Name)
	}

	if len(changes) == 0 {
		if err := s.claimWriteBack(ctx, plan, databaseName, nil, strategy, approved); err != nil {
			return nil, err
		}
		return &ApplyResult{ConflictsResolved: resolved}, nil
	}

	// Write-back changes documents in collections the source already has.
	// It does not create one: a collection made by an upsert would exist in
	// production with default options and a single document, and one that
	// production dropped would come back. Checked here, before the plan is
	// claimed, so that a refusal leaves the plan pending.
	existing, err := target.Database.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("failed to list the source's collections: %w", err)
	}
	capped := make(map[string]bool, len(existing))
	for _, spec := range existing {
		capped[spec.Name], _ = spec.Options.Lookup("capped").BooleanOK()
	}
	for _, c := range changes {
		isCapped, present := capped[c.Collection]
		if isCapped {
			return nil, fmt.Errorf("write-back refused before claim: %s/%s targets a capped collection, which does not support snapshot reads", c.Collection, c.DocumentID)
		}
		if !present && !c.Delete {
			return nil, fmt.Errorf("collection %s does not exist in the source database, and write-back does not create collections. Create it there with the options it should have, let following catch up, and preview again", c.Collection)
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
	// The v1 driver exposes the checked-out server session's counter through XSession.
	driverSession, ok := session.(mongo.XSession) //nolint:staticcheck
	if !ok {
		return nil, errors.New("the source session has no transaction number to record")
	}
	serverSession := driverSession.ClientSession().Server
	afterTxn := serverSession.TxnNumber
	claimClosed := false
	defer func() {
		// An open-ended claim must never match a later, unrelated transaction.
		// Discard its session from the pool if its final state is unknown.
		if !claimClosed {
			serverSession.MarkDirty()
		}
	}()
	record := &WriteBackRecord{
		ApprovedDigest: approved,
		Session:        primitive.Binary{Subtype: subtype, Data: data},
		AfterTxn:       &afterTxn,
		Database:       branch.Follows, Display: boundedAuditText(target.Display, maxDisplayBytes),
		Strategy: strategy, Changes: len(changes),
	}
	// A metadata election must not undo the claim after the separate
	// source transaction commits. Require majority acknowledgement for
	// the claim and both transitions out of it.
	plans := s.plans.Database().Collection(s.plans.Name(), options.Collection().SetWriteConcern(writeconcern.Majority()))
	if err := s.claimWriteBack(writerCtx, plan, databaseName, record, strategy, approved); err != nil {
		return nil, err
	}

	uncertain, txnErr := writeSourceTransaction(writerCtx, session, target.Database, changes)

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
			"write_back.commit_txn":   serverSession.TxnNumber,
		}}}})
		if err == nil && marked.MatchedCount != 1 {
			err = errors.New("the plan record no longer matches the write-back claim")
		}
		result := &ApplyResult{Applied: len(changes), ConflictsResolved: resolved}
		if err != nil {
			return result, fmt.Errorf("the source transaction committed, but plan %s could not be marked applied: %w", plan.ID.Hex(), err)
		}
		claimClosed = true
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
	claimClosed = true
	return nil, txnErr
}

// The driver's WithTransaction detaches commit from the caller's deadline.
// Drive the same body/commit retry decisions with one deadline for all attempts.
// Once a commit has been sent, only a definitive abort can release the claim.
func writeSourceTransaction(ctx context.Context, session mongo.Session, db *mongo.Database, changes []Change) (uncertain bool, err error) {
	maxCommit := writeBackDeadline
	opts := options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()).
		SetReadPreference(readpref.Primary()).SetMaxCommitTime(&maxCommit)
	for ctx.Err() == nil {
		if err := session.StartTransaction(opts); err != nil {
			return false, err
		}
		err := writeChanges(mongo.NewSessionContext(ctx, session), db, changes)
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
	// A collection can become capped after preflight. MongoDB labels that
	// snapshot-read failure transient, but a fresh transaction cannot read it.
	var command mongo.CommandError
	if errors.As(err, &command) && command.HasErrorCodeWithMessage(246, "Reading from capped collections with readConcern snapshot is not supported") {
		return false
	}
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
func (s *Service) claimWriteBack(ctx context.Context, plan *Plan, databaseName string, record *WriteBackRecord, strategy, approved string) error {
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
		// The caller selected and checked its source changes from a record
		// with this same digest. Check again on every transaction retry.
		stored, err := s.GetPlan(sc, plan.ID)
		if err != nil {
			return nil, err
		}
		if err := stored.CheckApproval(approved); err != nil {
			return nil, err
		}
		if err := source.FenceWriteBackClaim(sc, s.plans.Database(), stored.TargetBranchID, stored.ProjectID, stored.FollowedDatabase, stored.FollowedReplicaSetID); err != nil {
			return nil, err
		}
		fields := bson.M{"status": StatusWritingBack, "write_back": bson.M{"$literal": record}, "write_back_last_error": "$$REMOVE"}
		update := mongo.Pipeline{{{Key: "$set", Value: fields}}}
		if record == nil {
			fields["status"] = StatusApplied
			fields["write_back"] = bson.M{
				"approved_digest": approved, "database": bson.M{"$literal": databaseName},
				"strategy": bson.M{"$literal": strategy}, "changes": 0, "started_at": "$$NOW",
			}
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

// A refusal identifies a bounded sample from every group, even for a large plan.
type evidenceFailures map[string]*affectedDocuments

type affectedDocuments struct {
	count int
	names []string
}

func (failures evidenceFailures) add(reason string, c Change) {
	const maxExamples = 5
	group := failures[reason]
	if group == nil {
		group = &affectedDocuments{}
		failures[reason] = group
	}
	group.count++
	if len(group.names) < maxExamples {
		group.names = append(group.names, c.Collection+"/"+c.DocumentID)
	}
}

func (failures evidenceFailures) err() error {
	if len(failures) == 0 {
		return nil
	}
	var message strings.Builder
	message.WriteString("write-back refused before claim")
	for _, reason := range slices.Sorted(maps.Keys(failures)) {
		group := failures[reason]
		noun := "documents"
		if group.count == 1 {
			noun = "document"
		}
		fmt.Fprintf(&message, "\n%s (%d %s): %s", reason, group.count, noun, strings.Join(group.names, ", "))
		if more := group.count - len(group.names); more > 0 {
			fmt.Fprintf(&message, ", and %d more", more)
		}
	}
	return errors.New(message.String())
}

const (
	oldPlanEvidence   = "older plan format or unknown image provenance; preview again with this version using --output json"
	unverifiedBefore  = "unverified before-image: capture recorded an unverified image or target history was reclaimed. Stop following, run argon source resync -p <project> --force, then preview again"
	unverifiedAfter   = "unverified after-image: capture recorded an unverified image or sandbox history was reclaimed. Record the intended sandbox document again with verified capture, then preview again"
	duplicateEvidence = "duplicate fields in a plan image. Remove the repeated field names, recapture the document and preview again"
)

// prepareWriteBack validates only images selected by the conflict strategy.
// The transaction will consume these saved images without consulting either branch's history.
func prepareWriteBack(plan *Plan, changes []Change) error {
	if !plan.approvalEligible() {
		return errors.New(oldPlanEvidence)
	}
	failures := evidenceFailures{}
	for i := range changes {
		c := &changes[i]
		if !c.Delete && c.DocumentRaw != nil {
			checkAfterLayout(*c, failures)
			if bytes.Equal(c.BeforeRaw, c.DocumentRaw) {
				failures.add("replacement has identical before and after images", *c)
			}
		}
		key, err := wal.RawDocumentID(c.DocumentID)
		if err != nil || (!c.storedKey.IsZero() && !c.storedKey.Equal(key)) {
			failures.add("invalid typed document key. Run preview again", *c)
		}
		for _, image := range c.cliImages() {
			if image.raw == nil || (c.Delete && image.name == "document") {
				continue
			}
			id, err := image.raw.LookupErr("_id")
			if err != nil || !id.Equal(key) {
				failures.add(image.name+" image has a missing or different BSON _id. Run preview again", *c)
			}
			// The transaction's MongoDB filter uses the image element itself.
			c.ID = id
		}
		if slices.Contains(c.SortedImages, "before") || (c.BeforeRaw == nil && (c.Delete || c.Before != nil)) {
			failures.add(unverifiedBefore, *c)
		}
		if !c.Delete && (slices.Contains(c.SortedImages, "document") || c.DocumentRaw == nil) {
			failures.add(unverifiedAfter, *c)
		}
		// Report each document once for each cause, even when both images have it.
		for _, image := range c.cliImages() {
			if image.raw == nil || (c.Delete && image.name == "document") {
				continue
			}
			if ingest.HasDuplicateKeys(image.raw) {
				failures.add(duplicateEvidence, *c)
				break
			}
		}
	}
	return failures.err()
}

func checkAfterLayout(c Change, failures evidenceFailures) {
	fields, err := c.DocumentRaw.Elements()
	if err != nil || len(fields) == 0 || fields[0].Key() != "_id" {
		failures.add("after-image must start with _id; preview again", c)
	}
	for _, field := range fields {
		t, i, ok := field.Value().TimestampOK()
		if ok && t == 0 && i == 0 && field.Key() != "_id" {
			failures.add("MongoDB replaces a top-level non-_id Timestamp(0,0) with the current time on write", c)
			break
		}
	}
}

// writeChanges is the body of the source transaction: compare, then write,
// one document at a time. It issues single-document inserts, replacements
// and deletes into collections that exist; it never upserts, drops, renames
// or rewrites a collection.
func writeChanges(ctx mongo.SessionContext, database *mongo.Database, changes []Change) error {
	for _, c := range changes {
		conflict := &ConflictError{Collection: c.Collection, DocumentID: c.DocumentID}
		coll := database.Collection(c.Collection)
		current, err := coll.FindOne(ctx, bson.M{"_id": c.ID}).Raw()
		if err == mongo.ErrNoDocuments {
			current = nil
		} else if err != nil {
			return err
		}
		// For an insert both are nil. Every existing row must match the entire saved document.
		if !bytes.Equal(current, c.BeforeRaw) {
			return conflict
		}
		switch {
		case c.Delete:
			result, err := coll.DeleteOne(ctx, bson.M{"_id": c.ID})
			if err != nil {
				return err
			}
			if result.DeletedCount != 1 {
				return conflict
			}
		case c.BeforeRaw == nil:
			if _, err := coll.InsertOne(ctx, c.DocumentRaw); err != nil {
				return err
			}
		default:
			result, err := coll.ReplaceOne(ctx, bson.M{"_id": c.ID}, c.DocumentRaw)
			if err != nil {
				return err
			}
			if result.MatchedCount != 1 {
				return conflict
			}
			if result.ModifiedCount != 1 {
				return &StoredImageError{Collection: c.Collection, DocumentID: c.DocumentID}
			}
		}
	}
	// Read the final state of the entire batch while the transaction can
	// still roll back every write, including earlier writes to other keys.
	for _, c := range changes {
		stored, err := database.Collection(c.Collection).FindOne(ctx, bson.M{"_id": c.ID}).Raw()
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			return err
		}
		if c.Delete && err == nil || !c.Delete && (err != nil || !bytes.Equal(stored, c.DocumentRaw)) {
			return &StoredImageError{Collection: c.Collection, DocumentID: c.DocumentID}
		}
	}
	return nil
}

// LookupWriteBack returns the plan that claimed this source transaction, or nil.
func (s *Service) LookupWriteBack(ctx context.Context, targetBranchID string, session primitive.Binary, txnNumber int64) (*Plan, error) {
	var plan Plan
	plans := s.plans.Database().Collection(s.plans.Name(), options.Collection().SetReadPreference(readpref.Primary()))
	// The greatest preceding bound owns the transaction, including retries.
	// Missing bounds sort last and preserve the session-only lookup for old claims.
	filter := bson.M{"write_back.session": session, "target_branch_id": targetBranchID, "$or": bson.A{
		bson.M{"write_back.after_txn": bson.M{"$lt": txnNumber}},
		bson.M{"write_back.after_txn": bson.M{"$exists": false}},
	}}
	err := plans.FindOne(ctx, filter, options.FindOne().SetProjection(bson.M{"changes": 0, "conflicts": 0}).
		SetSort(bson.D{{Key: "write_back.after_txn", Value: -1}})).Decode(&plan)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if committed := plan.WriteBack.CommitTxn; committed != nil && *committed != txnNumber {
		return nil, nil
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
