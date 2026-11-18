package merge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
				changes = append(changes, Change{
					Collection: c.Collection,
					DocumentID: c.DocumentID,
					Delete:     c.Theirs == nil,
					Document:   c.Theirs,
					ID:         imageID(c.Theirs, c.Ours, c.Base),
					Before:     c.Ours,
				})
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
	if err := source.RequireExperimental(); err != nil {
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

	branch, status, err := ingest.ReadCaptureStatus(ctx, s.plans.Database(), plan.TargetBranchID)
	if err != nil {
		return nil, fmt.Errorf("target branch not found: %w", err)
	}
	if !branch.IsFollower() {
		return nil, fmt.Errorf("branch %s does not follow an external database; apply the plan without write-back", branch.Name)
	}
	if err := status.Health.WriteBackAdmission(); err != nil {
		return nil, err
	}
	if target.Database == nil || target.Database.Name() != branch.Follows {
		return nil, fmt.Errorf("branch %s follows database %q; write-back was pointed at another database", branch.Name, branch.Follows)
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

	session, err := target.Database.Client().StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(context.Background())
	subtype, data, ok := session.ID().Lookup("id").BinaryOK()
	if !ok {
		return nil, errors.New("the source session has no identifier to record")
	}
	record := &WriteBackRecord{
		Session:  primitive.Binary{Subtype: subtype, Data: data},
		Database: branch.Follows, Display: target.Display,
		Strategy: strategy, Changes: len(changes), StartedAt: time.Now(),
	}
	// A metadata election must not undo the claim after the separate
	// source transaction commits. Require majority acknowledgement for
	// the claim and both transitions out of it.
	plans := s.plans.Database().Collection(s.plans.Name(), options.Collection().SetWriteConcern(writeconcern.Majority()))
	claimed, err := plans.UpdateOne(ctx, bson.M{"_id": plan.ID, "status": StatusPending},
		bson.M{"$set": bson.M{"status": StatusWritingBack, "write_back": record}})
	if err != nil {
		return nil, fmt.Errorf("could not claim plan %s with majority write concern: %w", plan.ID.Hex(), err)
	}
	if claimed.MatchedCount != 1 {
		return nil, errors.New("merge plan is no longer pending")
	}

	// reachedCommit separates "the transaction body failed" (nothing can
	// have been written) from "the commit itself failed" (unknown).
	reachedCommit := false
	database := target.Database
	_, txnErr := session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		reachedCommit = false
		if err := writeChanges(sc, database, writes); err != nil {
			return nil, err
		}
		reachedCommit = true
		return nil, nil
	}, options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority()).
		SetReadPreference(readpref.Primary()))

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

	var labeled mongo.LabeledError
	aborted := !reachedCommit || (errors.As(txnErr, &labeled) && labeled.HasErrorLabel("TransientTransactionError"))
	// The driver sends the commit on a context of its own. When it returns
	// the caller's bare context error after the body succeeded, it noticed
	// the cancellation before committing and aborted instead: no commit was
	// ever sent.
	var answered mongo.CommandError
	if !aborted && ctx.Err() != nil && errors.Is(txnErr, ctx.Err()) && !errors.As(txnErr, &answered) && !errors.As(txnErr, &labeled) {
		aborted = true
	}
	if !aborted {
		return nil, fmt.Errorf("the source commit for plan %s did not report an outcome (%w). The plan stays %s unless following sees the transaction and marks it applied. %s", plan.ID.Hex(), txnErr, StatusWritingBack, writingBackAdvice)
	}
	// Nothing was written. Return the plan to pending so that a transient
	// failure can be retried; a conflict will fail the same way until the
	// plan is previewed again.
	reason := txnErr.Error()
	if target.Redact != nil {
		reason = target.Redact(reason)
	}
	if _, err := plans.UpdateOne(record2, bson.M{"_id": plan.ID, "status": StatusWritingBack, "write_back.session": record.Session},
		bson.M{"$set": bson.M{"status": StatusPending, "write_back_last_error": reason}, "$unset": bson.M{"write_back": ""}}); err != nil {
		return nil, errors.Join(txnErr, fmt.Errorf("nothing was written, but the plan could not be returned to pending: %w", err))
	}
	return nil, txnErr
}

// sourceWrite is one planned change prepared for the source transaction.
type sourceWrite struct {
	change Change
	id     interface{}
	// before is the document as the followed branch stored it when the plan
	// was made, byte for byte, when that is known. The comparison then sees
	// field order too.
	before bson.Raw
	// document is the planned replacement. Existing fields can keep their
	// source order when an existing document is replaced.
	document bson.D
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
		if entry != nil && entry.Metadata["capture_images"] != "attached_unverified" {
			write.before = entry.PostImage
		}
	}
	if c.Delete {
		return write, nil
	}
	// A plan holds each document as a map, which has no field order. The
	// branch being merged recorded the document as its database stored it;
	// failing that the fields are sorted, which is at least the same on
	// every run.
	entry, err := s.storedImage(source, plan.SourceHead, c.Collection, c.DocumentID, c.Document)
	if err != nil {
		return sourceWrite{}, fmt.Errorf("could not read write-back replacement for %s/%s: %w", c.Collection, c.DocumentID, err)
	}
	if entry != nil {
		if err := bson.Unmarshal(entry.PostImage, &write.document); err != nil {
			return sourceWrite{}, err
		}
	} else {
		canonical, err := mongoexpr.Canonicalize(c.Document)
		if err != nil {
			return sourceWrite{}, err
		}
		write.document, _ = canonical.(bson.D)
	}
	ordered := bson.D{{Key: "_id", Value: id}}
	for _, field := range write.document {
		if field.Key != "_id" {
			ordered = append(ordered, field)
		}
	}
	write.document = ordered
	return write, nil
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
			var existing bson.D
			if err := bson.Unmarshal(current, &existing); err != nil {
				return err
			}
			result, err := coll.ReplaceOne(ctx, bson.M{"_id": write.id}, orderLike(existing, write.document))
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
	err := s.plans.FindOne(ctx, bson.M{"write_back.session": session, "target_branch_id": targetBranchID},
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
