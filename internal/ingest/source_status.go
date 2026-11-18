package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

// Position writers replace the whole source subdocument. An older writer
// therefore drops this digest when it replaces the position.
type sourceCheckpoint struct {
	SourceStatus   `bson:",inline"`
	PositionDigest []byte `bson:"position_digest"`
}

// The stream resumes from the raw token when present. A stream opened by
// timestamp uses the eight bytes of the BSON timestamp value instead.
func positionSHA256(token bson.Raw, startAt *primitive.Timestamp) []byte {
	if len(token) == 0 {
		if startAt == nil {
			return nil
		}
		token = bsoncore.AppendTimestamp(nil, startAt.T, startAt.I)
	}
	sum := sha256.Sum256(token)
	return sum[:]
}

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
// Outside a transaction, Owner == OwnerExpired means a fresh snapshot also
// showed an overdue heartbeat. Within a transaction the conditional lease
// write fences takeover against the same snapshot used for admission.
func captureSnapshots(ctx context.Context, metadata *mongo.Database, branchID *string) ([]captureSnapshot, error) {
	branches := metadata.Collection("wal_branches", options.Collection().
		SetReadConcern(readconcern.Snapshot()).SetReadPreference(readpref.Primary()))
	filter := bson.M{"is_deleted": false}
	publication := bson.M{}
	if branchID != nil {
		filter["_id"] = *branchID
	} else {
		// Exclude staged projects before loading their checkpoints. An orphan
		// has an empty project array and must remain visible to operators.
		publication["project.importing"] = bson.M{"$ne": true}
	}
	read := func(ctx context.Context) ([]captureSnapshot, error) {
		cursor, err := branches.Aggregate(ctx, mongo.Pipeline{
			{{Key: "$match", Value: filter}},
			{{Key: "$project", Value: bson.M{"_id": 0, "branch": "$$ROOT"}}},
			{{Key: "$lookup", Value: bson.M{"from": "wal_projects", "localField": "branch.project_id", "foreignField": "_id", "as": "project"}}},
			{{Key: "$match", Value: publication}},
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
		if err != nil {
			return nil, err
		}
		var rows []captureSnapshot
		if err := cursor.All(ctx, &rows); err != nil {
			return nil, err
		}
		return rows, nil
	}
	rows, err := read(ctx)
	if err != nil {
		return nil, err
	}
	// The v1 driver exposes transaction state through XSession.
	if session, ok := mongo.SessionFromContext(ctx).(mongo.XSession); ok && session.ClientSession().TransactionRunning() { //nolint:staticcheck
		return rows, nil
	}
	for _, row := range rows {
		if !row.LeaseFresh && row.Branch.IsFollower() {
			if _, owned := row.Stored.Lookup("owner").DocumentOK(); owned {
				return read(mongo.NewSessionContext(ctx, nil))
			}
		}
	}
	return rows, nil
}

// ReadCaptureStatus reads a branch and its saved status at one metadata snapshot.
// An ordinary branch without a capture report returns a nil status.
func ReadCaptureStatus(ctx context.Context, metadata *mongo.Database, branchID string) (*wal.Branch, *Status, error) {
	rows, err := captureSnapshots(ctx, metadata, &branchID)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, mongo.ErrNoDocuments
	}
	snapshot := &rows[0]
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
	rows, err := captureSnapshots(ctx, metadata, nil)
	if err != nil {
		return nil, err
	}
	var reports []CaptureReport
	for i := range rows {
		report, err := rows[i].report()
		if err != nil {
			return nil, err
		}
		if report != nil {
			reports = append(reports, *report)
		}
	}
	return reports, nil
}

func (snapshot *captureSnapshot) report() (*CaptureReport, error) {
	status, err := snapshot.status()
	if err != nil || status == nil {
		return nil, err
	}
	var healthy bool
	if status.Health != nil {
		healthy = status.Health.Kind == SourceHealthy
	} else {
		// Ordinary checkout capture has its own lifecycle.
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
		f.CleanStart = status.Owner.CleanStart
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
		if stored := snapshot.Stored.Lookup("source", "position_digest"); stored.Type != 0 {
			f.PositionDigest = DigestDiffers
			subtype, digest, ok := stored.BinaryOK()
			if ok && subtype == 0 && len(digest) == sha256.Size && bytes.Equal(digest, positionSHA256(snapshot.ResumeToken, snapshot.StartAt)) {
				f.PositionDigest = DigestMatches
			}
		}
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
