package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

type captureSnapshot struct {
	Branch      wal.Branch `bson:"branch"`
	Stored      bson.Raw   `bson:"capture_status"`
	ResumeToken bson.Raw   `bson:"resume_token"`
	LeaseFresh  bool       `bson:"lease_fresh"`
}

// A follower transaction changes the head and position together. Snapshot
// reads keep the join from combining the two sides of a concurrent commit.
func captureSnapshots(ctx context.Context, metadata *mongo.Database, filter bson.M) (*mongo.Cursor, error) {
	branches := metadata.Collection("wal_branches", options.Collection().
		SetReadConcern(readconcern.Snapshot()).SetReadPreference(readpref.Primary()))
	return branches.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$project", Value: bson.M{"_id": 0, "branch": "$$ROOT"}}},
		{{Key: "$lookup", Value: bson.M{"from": "wal_ingest_state", "localField": "branch._id", "foreignField": "_id", "as": "checkpoint"}}},
		{{Key: "$project", Value: bson.M{"branch": 1, "checkpoint": bson.M{"$first": "$checkpoint"}}}},
		{{Key: "$project", Value: bson.M{"branch": 1, "capture_status": "$checkpoint.capture_status", "resume_token": "$checkpoint.resume_token"}}},
		{{Key: "$set", Value: bson.M{"lease_fresh": sourceLeaseFresh()}}},
	})
}

// ReadCaptureStatus reads a branch and its saved status at one metadata snapshot.
// An ordinary branch without a capture report returns a nil status.
func ReadCaptureStatus(ctx context.Context, metadata *mongo.Database, branchID string) (*wal.Branch, *Status, error) {
	cursor, err := captureSnapshots(ctx, metadata, bson.M{"_id": branchID, "is_deleted": false})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	if !cursor.Next(ctx) {
		if err := cursor.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, mongo.ErrNoDocuments
	}
	var snapshot captureSnapshot
	if err := cursor.Decode(&snapshot); err != nil {
		return nil, nil, err
	}
	status, err := snapshot.status()
	return &snapshot.Branch, status, err
}

// SourceStatusFor reports the health of the source position saved for a branch.
func (s *Service) SourceStatusFor(ctx context.Context, branchID string) (*Status, error) {
	_, status, err := s.CaptureStatusFor(ctx, branchID)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return status, err
}

// CaptureStatusFor reads the branch record together with its evaluated status.
func (s *Service) CaptureStatusFor(ctx context.Context, branchID string) (*wal.Branch, *Status, error) {
	return ReadCaptureStatus(ctx, s.state.Database(), branchID)
}

// CaptureReport pairs an evaluated status with its stored fields for display.
type CaptureReport struct {
	Status *Status
	Fields bson.M
}

// ReadCaptureReports reads capture health across the metadata deployment.
func ReadCaptureReports(ctx context.Context, metadata *mongo.Database) ([]CaptureReport, error) {
	cursor, err := captureSnapshots(ctx, metadata, bson.M{"is_deleted": false})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	var reports []CaptureReport
	for cursor.Next(ctx) {
		var snapshot captureSnapshot
		if err := cursor.Decode(&snapshot); err != nil {
			return nil, err
		}
		status, err := snapshot.status()
		if err != nil {
			return nil, err
		}
		if status == nil {
			continue
		}
		fields := bson.M{}
		if len(snapshot.Stored) != 0 {
			if err := bson.Unmarshal(snapshot.Stored, &fields); err != nil {
				return nil, err
			}
		}
		reports = append(reports, CaptureReport{Status: status, Fields: fields})
	}
	return reports, cursor.Err()
}

func (snapshot *captureSnapshot) status() (*Status, error) {
	if len(snapshot.Stored) == 0 && !snapshot.Branch.IsFollower() {
		return nil, nil
	}
	status := &Status{}
	if len(snapshot.Stored) != 0 {
		if err := bson.Unmarshal(snapshot.Stored, status); err != nil {
			return nil, err
		}
	}
	status.BranchID = snapshot.Branch.ID
	if snapshot.Branch.IsFollower() {
		status.hasResumeToken = len(snapshot.ResumeToken) != 0
		status.applySourceLeaseFreshness(snapshot.LeaseFresh)
		if reason := status.sourceDegradation(&snapshot.Branch, snapshot.Stored.Lookup("head_lsn")); reason != "" {
			status.synthesizedDegradation = status.State != "degraded"
			status.State, status.Error = "degraded", reason
		}
	}
	return status, nil
}

// sourceDegradation checks the branch and position from the same snapshot.
func (s *Status) sourceDegradation(branch *wal.Branch, savedHead bson.RawValue) string {
	if branch.IsLive() || branch.PhysicalDB != "" {
		return sourceCheckoutError(branch.Name).Error()
	}
	if s.State == "degraded" {
		if s.Error != "" {
			return s.Error
		}
		return "the last follower reported a capture failure"
	}
	if !s.hasSavedSourcePosition(branch.Follows) {
		return sourcePositionError(branch.Name).Error()
	}
	if head, ok := savedHead.AsInt64OK(); ok && head != branch.HeadLSN {
		return sourceHeadMismatch(branch, head).Error()
	}
	return ""
}

func sourceHeadMismatch(branch *wal.Branch, savedHead int64) *CaptureError {
	return &CaptureError{Message: fmt.Sprintf("the head of branch %s moved from LSN %d to %d without the follower: something else wrote to it, so it no longer equals the source. Upgrade every process that uses this metadata database, then run `argon source resync`", branch.Name, savedHead, branch.HeadLSN)}
}

// ResyncUnfinished reports whether a resync still has to reach a consistent point.
func (s *Status) ResyncUnfinished() bool {
	return s != nil && s.Source != nil && s.Source.Resync != nil
}

func (s *Status) hasSavedSourcePosition(database string) bool {
	// An unfinished resync has deliberately replaced the old resume position.
	return s != nil && s.Source != nil && s.Source.Database == database &&
		(s.hasResumeToken || s.ResyncUnfinished())
}

// RequireSourceWriteBackReady rejects an invalid source position before the
// plan is claimed or a source write is attempted.
func (s *Status) RequireSourceWriteBackReady() error {
	if s.State == "degraded" {
		state := "ended as degraded"
		if s.synthesizedDegradation {
			state = "is degraded"
		}
		return fmt.Errorf("write-back refused: following %s (%s). After repairing the branch with `argon source resync`, preview again", state, s.Error)
	}
	if s.ResyncUnfinished() {
		return errors.New("write-back refused: a resync of the followed branch has not finished. Complete it with `argon source resync`, then preview again")
	}
	if s.SourceLeaseExpired {
		return fmt.Errorf("write-back refused: %s. Restart `argon source follow`, let it catch up, then preview again", s.Error)
	}
	return nil
}
