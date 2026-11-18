package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

type captureSnapshot struct {
	Branch  wal.Branch `bson:"branch"`
	Project *struct {
		Importing bool `bson:"importing"`
	} `bson:"project"`
	// A missing head cannot verify a checkpoint at LSN zero.
	BranchHead   *int64               `bson:"branch_head"`
	CheckpointID string               `bson:"checkpoint_id"`
	StartAt      *primitive.Timestamp `bson:"start_at"`
	Stored       bson.Raw             `bson:"capture_status"`
	ResumeToken  bson.Raw             `bson:"resume_token"`
	LeaseFresh   bool                 `bson:"lease_fresh"`
}

// A follower transaction changes the head and position together. Snapshot
// reads keep the join from combining the two sides of a concurrent commit.
func captureSnapshots(ctx context.Context, metadata *mongo.Database, filter bson.M) (*mongo.Cursor, error) {
	branches := metadata.Collection("wal_branches", options.Collection().
		SetReadConcern(readconcern.Snapshot()).SetReadPreference(readpref.Primary()))
	// Publication, branch and checkpoint must all come from this snapshot.
	return branches.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$project", Value: bson.M{"_id": 0, "branch": "$$ROOT"}}},
		{{Key: "$lookup", Value: bson.M{"from": "wal_projects", "localField": "branch.project_id", "foreignField": "_id", "as": "project"}}},
		{{Key: "$lookup", Value: bson.M{"from": "wal_ingest_state", "localField": "branch._id", "foreignField": "_id", "as": "checkpoint"}}},
		{{Key: "$project", Value: bson.M{"branch": 1, "project": bson.M{"$first": "$project"}, "checkpoint": bson.M{"$first": "$checkpoint"}}}},
		{{Key: "$project", Value: bson.M{
			"branch": 1, "branch_head": "$branch.head_lsn",
			"project._id": 1, "project.importing": 1,
			"checkpoint_id": "$checkpoint._id", "capture_status": "$checkpoint.capture_status",
			"resume_token": "$checkpoint.resume_token", "start_at": "$checkpoint.start_at",
		}}},
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
	Status  *Status
	Fields  bson.M
	Healthy bool
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
		report, err := snapshot.report()
		if err != nil {
			return nil, err
		}
		if report != nil {
			reports = append(reports, *report)
		}
	}
	return reports, cursor.Err()
}

func (snapshot *captureSnapshot) report() (*CaptureReport, error) {
	status, err := snapshot.status()
	if err != nil || status == nil {
		return nil, err
	}
	var healthy bool
	if status.Health != nil {
		if status.Health.Kind == SourceImporting {
			return nil, nil
		}
		healthy = status.Health.Kind == SourceHealthy
	} else {
		// Ordinary checkout capture has its own lifecycle. Unpublished
		// projects have no deployment report for either kind of capture.
		if snapshot.projectState() == ProjectImporting {
			return nil, nil
		}
		healthy = status.State != "degraded"
		if !healthy && status.Error == "" {
			status.Error = "capture is degraded"
		}
	}
	fields := bson.M{}
	if len(snapshot.Stored) != 0 {
		if err := bson.Unmarshal(snapshot.Stored, &fields); err != nil {
			return nil, err
		}
	}
	return &CaptureReport{Status: status, Fields: fields, Healthy: healthy}, nil
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
		status.checkpointExists = snapshot.CheckpointID != ""
		status.resumeToken, status.startAt = snapshot.ResumeToken, snapshot.StartAt
		status.facts = snapshot.facts(status)
		verdict := Evaluate(status.facts)
		status.Health = &verdict
		status.State = verdict.DisplayState(status.State)
		status.SourceLeaseExpired = verdict.LeaseExpired
		if problem := verdict.HealthError(); problem != nil {
			status.Error = problem.Error()
		}
	}
	return status, nil
}

func (snapshot *captureSnapshot) projectState() ProjectState {
	if snapshot.Project == nil {
		return ProjectMissing
	}
	if snapshot.Project.Importing {
		return ProjectImporting
	}
	return ProjectPublished
}

// facts interprets only records joined at the same snapshot. The server evaluates
// the lease deadline even when a stopped run left an owner behind.
func (snapshot *captureSnapshot) facts(status *Status) Facts {
	branch := &snapshot.Branch
	f := Facts{
		State: followerState(status.State), HasError: status.Error != "", StoredError: status.Error,
		CheckedOut: branch.IsLive() || branch.PhysicalDB != "", BranchName: branch.Name,
		Unverified: status.SourceUnverified != nil, Owner: OwnerNone, Head: HeadMissing,
		Project: snapshot.projectState(),
	}
	if status.Owner != nil {
		f.Owner = OwnerExpired
		if snapshot.LeaseFresh {
			f.Owner = OwnerLive
		}
	}
	switch {
	case f.Owner == OwnerExpired:
		f.OwnerLoss = sourceOwnerLoss(status.Owner)
	case status.SourceUnverified != nil && status.SourceUnverified.PreviousOwner != nil:
		f.OwnerLoss = sourceOwnerLoss(status.SourceUnverified.PreviousOwner)
	case f.Unverified:
		f.OwnerLoss = "the previous live source report was left without a lease owner"
	default:
		f.OwnerLoss = sourceOwnerLoss(nil)
	}
	if head, ok := snapshot.Stored.Lookup("head_lsn").AsInt64OK(); ok && snapshot.BranchHead != nil {
		f.Head = HeadEqual
		if head != branch.HeadLSN {
			f.Head = HeadDifferent
			f.HeadMismatch = headMovedMessage(branch, head)
		}
	}
	if src := status.Source; src != nil {
		f.Mode = checkpointMode(src.Mode)
		f.UnfinishedResync = status.ResyncUnfinished()
		f.OpenRange = src.InexactFromLSN != 0 || src.InexactReason != ""
		tokenAt, ok := tokenTime(snapshot.ResumeToken)
		// Import and resync open at the tick after their confirmed boundary.
		// On an idle source, reached records the boundary while MongoDB
		// returns that opening token. Later checkpoints record the token time.
		boundary, hasBoundary := previousTimestamp(tokenAt)
		idleOpening := snapshot.StartAt != nil && snapshot.StartAt.Equal(tokenAt) &&
			hasBoundary && boundary.Equal(src.Position)
		f.PositionPresent = src.Database == branch.Follows && ok && !src.Position.IsZero() &&
			(tokenAt.Equal(src.Position) || idleOpening)
		consistentLSN, hasLSN := snapshot.Stored.Lookup("source", "consistent", "lsn").AsInt64OK()
		f.ExactPointAtHead = snapshot.BranchHead != nil && src.Consistent != nil && hasLSN && consistentLSN == branch.HeadLSN &&
			!src.Consistent.ClusterTime.IsZero() && src.Consistent.ClusterTime.Equal(src.Position) &&
			src.Mode == ModeExact
	}
	return f
}

func sourceHeadMismatch(branch *wal.Branch, savedHead int64) *CaptureError {
	return &CaptureError{Message: headMovedMessage(branch, savedHead) + ". Upgrade every process that uses this metadata database, then run `argon source resync`"}
}

func headMovedMessage(branch *wal.Branch, savedHead int64) string {
	return fmt.Sprintf("the head of branch %s moved from LSN %d to %d without the follower", branch.Name, savedHead, branch.HeadLSN)
}

// ResyncUnfinished reports whether a resync still has to reach a consistent point.
func (s *Status) ResyncUnfinished() bool {
	return s != nil && s.Source != nil && s.Source.Resync != nil
}
