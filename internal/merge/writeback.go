package merge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/argon-lab/argon/v2/internal/mongoexpr"
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
	plan, err := s.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	switch plan.Status {
	case StatusPending:
	case StatusWritingBack:
		return nil, fmt.Errorf("merge plan is %s: an earlier write-back claimed it and did not confirm its commit. It will not be written again. Following settles it as applied if that transaction reached the source; if the plan is still %s once following has caught up, nothing was written, and a new preview shows what is left to merge", plan.Status, plan.Status)
	default:
		return nil, fmt.Errorf("merge plan is %s, not pending", plan.Status)
	}
	if plan.FormatVersion < 2 {
		return nil, errors.New("this plan predates before-images and cannot be checked against the source; run preview again")
	}

	branch, err := s.branches.GetBranchByID(plan.TargetBranchID)
	if err != nil {
		return nil, fmt.Errorf("target branch not found: %w", err)
	}
	if !branch.IsFollower() {
		return nil, fmt.Errorf("branch %s does not follow an external database; apply the plan without write-back", branch.Name)
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

	documents := make([]interface{}, len(changes))
	for i, c := range changes {
		if c.Delete {
			continue
		}
		if documents[i], err = s.documentToWrite(source, plan.SourceHead, c); err != nil {
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
	claimed, err := s.plans.UpdateOne(ctx, bson.M{"_id": plan.ID, "status": StatusPending},
		bson.M{"$set": bson.M{"status": StatusWritingBack, "write_back": record}})
	if err != nil {
		return nil, err
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
		if err := writeChanges(sc, database, changes, documents); err != nil {
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
		// The follower may have closed the plan already; either is fine.
		if _, err := s.plans.UpdateOne(record2, bson.M{"_id": plan.ID, "status": StatusWritingBack},
			bson.M{"$set": bson.M{"status": StatusApplied, "strategy": strategy, "applied_at": now, "write_back.committed_at": now}}); err != nil {
			return &ApplyResult{Applied: len(changes), ConflictsResolved: resolved},
				fmt.Errorf("the plan was written to the source, but recording that failed (%w); the plan stays %s until following sees the transaction and closes it", err, StatusWritingBack)
		}
		return &ApplyResult{Applied: len(changes), ConflictsResolved: resolved}, nil
	}

	var labeled mongo.LabeledError
	aborted := !reachedCommit || (errors.As(txnErr, &labeled) && labeled.HasErrorLabel("TransientTransactionError"))
	if !aborted {
		return nil, fmt.Errorf("the source transaction's commit did not report an outcome (%w). The plan stays %s and will not be written again; following closes it as applied if the transaction reached the source", txnErr, StatusWritingBack)
	}
	// Nothing was written. Return the plan to pending so that a transient
	// failure can be retried; a conflict will fail the same way until the
	// plan is previewed again.
	reason := txnErr.Error()
	if target.Redact != nil {
		reason = target.Redact(reason)
	}
	if _, err := s.plans.UpdateOne(record2, bson.M{"_id": plan.ID, "status": StatusWritingBack, "write_back.session": record.Session},
		bson.M{"$set": bson.M{"status": StatusPending, "write_back_last_error": reason}, "$unset": bson.M{"write_back": ""}}); err != nil {
		return nil, errors.Join(txnErr, fmt.Errorf("nothing was written, but the plan could not be returned to pending: %w", err))
	}
	return nil, txnErr
}

// writeChanges is the body of the source transaction: compare, then write,
// one document at a time. It issues only single-document deletes and
// replacements; it never drops, renames or rewrites a collection.
func writeChanges(ctx mongo.SessionContext, database *mongo.Database, changes []Change, documents []interface{}) error {
	for i, c := range changes {
		id, err := documentIDValue(c)
		if err != nil {
			return err
		}
		coll := database.Collection(c.Collection)
		var current bson.M
		err = coll.FindOne(ctx, bson.M{"_id": id}).Decode(&current)
		if err == mongo.ErrNoDocuments {
			current = nil
		} else if err != nil {
			return err
		}
		changed, err := docsUnequal(current, c.Before)
		if err != nil {
			return err
		}
		if changed {
			return &ConflictError{Collection: c.Collection, DocumentID: c.DocumentID}
		}
		if c.Delete {
			if _, err := coll.DeleteOne(ctx, bson.M{"_id": id}); err != nil {
				return err
			}
			continue
		}
		if _, err := coll.ReplaceOne(ctx, bson.M{"_id": id}, documents[i], options.Replace().SetUpsert(true)); err != nil {
			return err
		}
	}
	return nil
}

// documentToWrite chooses the bytes a put sends to the source. A plan holds
// each document as a map, which has no field order. The branch being merged
// recorded the document as its database stored it, so that image is written
// when it is the same document; otherwise the fields go out in sorted order,
// which is at least the same on every run.
func (s *Service) documentToWrite(source *wal.Branch, sourceHead int64, c Change) (interface{}, error) {
	id, err := documentIDValue(c)
	if err != nil {
		return nil, err
	}
	if history, err := s.wal.GetDocumentHistory(source.ID, c.Collection, c.DocumentID, source.BaseLSN+1, sourceHead); err == nil && len(history) > 0 {
		last := history[len(history)-1]
		if last.Operation == wal.OpPut && len(last.PostImage) > 0 && !source.IsDiscardedForRead(last.LSN, sourceHead) {
			var stored bson.M
			if bson.Unmarshal(last.PostImage, &stored) == nil {
				if differs, err := docsUnequal(stored, c.Document); err == nil && !differs {
					return last.PostImage, nil
				}
			}
		}
	}
	canonical, err := mongoexpr.Canonicalize(c.Document)
	if err != nil {
		return nil, err
	}
	fields, _ := canonical.(bson.D)
	ordered := bson.D{{Key: "_id", Value: id}}
	for _, field := range fields {
		if field.Key != "_id" {
			ordered = append(ordered, field)
		}
	}
	return ordered, nil
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
