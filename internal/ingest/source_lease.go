package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SourceHeartbeatInterval and SourceLeasePeriod control renewal and takeover.
var (
	SourceHeartbeatInterval = 5 * time.Second
	SourceLeasePeriod       = 60 * time.Second
)

const sourceOwnerPath = "capture_status.owner"

// SourceOwner identifies a run and when the metadata server last heard from it.
type SourceOwner struct {
	ID          string    `bson:"id" json:"-"`
	Kind        string    `bson:"kind" json:"kind"`
	Host        string    `bson:"host" json:"host"`
	PID         int       `bson:"pid" json:"pid"`
	StartedAt   time.Time `bson:"started_at" json:"started_at"`
	HeartbeatAt time.Time `bson:"heartbeat_at" json:"heartbeat_at"`
}

type sourceLease struct {
	branch *wal.Branch
	id     string
}

func (l *sourceLease) filter() bson.M {
	return bson.M{"_id": l.branch.ID, sourceOwnerPath + ".id": l.id}
}

// InitSourceState reserves the ingest state of a newly bound, unpublished
// branch. Catch-up and subsequent runs acquire their lease on this document.
func (s *Service) InitSourceState(ctx context.Context, branchID string) error {
	_, err := s.state.InsertOne(ctx, bson.M{"_id": branchID})
	return err
}

func (s *Service) acquireSourceLease(ctx context.Context, branchID, database, kind string) (*sourceLease, error) {
	id := primitive.NewObjectID().Hex()
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown host"
	}
	result, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		branch, err := s.branches.GetBranchByIDContext(sc, branchID)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return nil, &ConfigurationError{fmt.Sprintf("branch %s not found", branchID)}
			}
			return nil, err
		}
		if branch.Follows != database || branch.IsLive() {
			return nil, sourceDetached(branch.Name, database)
		}
		// The branch and state must be read in the same snapshot. Checkout
		// can create a different state document after a detach deletes this one.
		filter := bson.M{"_id": branchID, "$or": bson.A{
			bson.M{sourceOwnerPath: bson.M{"$exists": false}},
			bson.M{"$expr": bson.M{"$lt": bson.A{"$" + sourceOwnerPath + ".heartbeat_at",
				bson.M{"$subtract": bson.A{"$$NOW", SourceLeasePeriod.Milliseconds()}}}}},
		}}
		owner := bson.M{"id": id, "kind": kind, "host": bson.M{"$literal": host}, "pid": os.Getpid(),
			"started_at": "$$NOW", "heartbeat_at": "$$NOW"}
		updated, err := s.state.UpdateOne(sc, filter, mongo.Pipeline{{{Key: "$set", Value: bson.M{sourceOwnerPath: owner}}}})
		if err != nil {
			return nil, err
		}
		if updated.MatchedCount == 1 {
			return &sourceLease{branch: branch, id: id}, nil
		}
		status, err := s.SourceStatusFor(sc, branchID)
		if err != nil {
			return nil, err
		}
		if status != nil && status.Owner != nil {
			owner := status.Owner
			return nil, &ConfigurationError{fmt.Sprintf("source %s is already running for project %s on %s (pid %d) since %s",
				owner.Kind, branch.ProjectID, owner.Host, owner.PID, owner.StartedAt.UTC().Format(time.RFC3339))}
		}
		return nil, &ConfigurationError{fmt.Sprintf("branch %s has no saved source position to resume from", branch.Name)}
	})
	if err != nil {
		return nil, err
	}
	return result.(*sourceLease), nil
}

func sourceDetached(branch, database string) *ConfigurationError {
	return &ConfigurationError{fmt.Sprintf("branch %s no longer follows database %q (it was detached, checked out or removed)", branch, database)}
}

func (s *Service) sourceLeaseMiss(ctx context.Context, lease *sourceLease) error {
	branch, err := s.branches.GetBranchByIDContext(ctx, lease.branch.ID)
	if errors.Is(err, mongo.ErrNoDocuments) || (err == nil && (branch.Follows != lease.branch.Follows || branch.IsLive())) {
		return sourceDetached(lease.branch.Name, lease.branch.Follows)
	}
	if err != nil {
		return err
	}
	return &ConfigurationError{fmt.Sprintf("another process took over following branch %s for project %s", branch.Name, branch.ProjectID)}
}

func (s *Service) updateSourceLease(ctx context.Context, lease *sourceLease, update bson.M) error {
	result, err := s.state.UpdateOne(ctx, lease.filter(), update)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return s.sourceLeaseMiss(ctx, lease)
	}
	return nil
}

func (s *Service) renewSourceLease(ctx context.Context, lease *sourceLease) error {
	return s.updateSourceLease(ctx, lease, bson.M{"$currentDate": bson.M{sourceOwnerPath + ".heartbeat_at": true}})
}

func (s *Service) releaseSourceLease(lease *sourceLease) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.state.UpdateOne(ctx, lease.filter(), bson.M{"$unset": bson.M{sourceOwnerPath: ""}})
}

// keepSourceLease renews ownership even during slow source reads or retries.
func (s *Service) keepSourceLease(ctx context.Context, run *running) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		ticker := time.NewTicker(SourceHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := s.renewSourceLease(beatCtx, run.lease)
				cancel()
				var lost *ConfigurationError
				if errors.As(err, &lost) {
					done <- lost
					run.cancel()
					return
				}
			}
		}
	}()
	return done
}

func (s *Service) reportSource(ctx context.Context, lease *sourceLease, fields bson.M, state string, stored *Status) error {
	for field, value := range fields {
		literal := bson.M{"$literal": value}
		fields[field] = literal
		if state != "degraded" && (field == "capture_status.state" || field == "capture_status.error") {
			// Keep the known gap visible until a healthy checkpoint replaces it.
			fields[field] = bson.M{"$cond": bson.A{
				bson.M{"$eq": bson.A{"$capture_status.state", "degraded"}}, "$" + field, literal,
			}}
		}
	}
	var doc struct {
		Status Status `bson:"capture_status"`
	}
	err := s.state.FindOneAndUpdate(ctx, lease.filter(), mongo.Pipeline{{{Key: "$set", Value: fields}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return s.sourceLeaseMiss(ctx, lease)
	}
	*stored = doc.Status
	return err
}
