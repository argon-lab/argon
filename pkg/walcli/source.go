package walcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SourceURIEnv names the environment variable a source connection string is
// read from when no flag gives one. Argon never stores the string.
const SourceURIEnv = "ARGON_SOURCE_URI"

// SourceURI returns the connection string to use for a source: the given one,
// or the environment's.
func SourceURI(given string) string {
	if given != "" {
		return given
	}
	return os.Getenv(SourceURIEnv)
}

// ClusterTime is a MongoDB cluster time: seconds since the epoch and an
// ordinal within the second.
type ClusterTime struct {
	T uint32 `json:"t"`
	I uint32 `json:"i"`
}

func clusterTime(ts primitive.Timestamp) ClusterTime { return ClusterTime{T: ts.T, I: ts.I} }

// OnlineImport configures ImportDatabaseOnline.
type OnlineImport struct {
	URI      string
	Database string
	Project  string
	// BatchSize is the number of documents per copy batch (default 1000).
	BatchSize int
	// EnableSourceImages turns on changeStreamPreAndPostImages for the
	// source's collections. It changes how the source stores writes.
	EnableSourceImages bool
}

// ImportDatabaseOnline imports a database that is being written, without
// pausing its writers, and leaves the new project's main branch following it.
func (s *Services) ImportDatabaseOnline(ctx context.Context, opts OnlineImport) (interface{}, error) {
	return s.Importer.ImportDatabase(ctx, importer.ImportOptions{
		MongoURI: opts.URI, DatabaseName: opts.Database, ProjectName: opts.Project,
		BatchSize: opts.BatchSize, Online: true, EnableSourceImages: opts.EnableSourceImages,
	})
}

// SourceInfo describes the external database a project follows and how far
// following has come.
type SourceInfo struct {
	Project    string `json:"project"`
	Branch     string `json:"branch"`
	BranchID   string `json:"branch_id"`
	Database   string `json:"database"`
	Location   string `json:"location"`
	ReplicaSet string `json:"replica_set"`
	// Identity says what tells this source apart from another deployment:
	// its replica-set ID, or, when that could not be read, its set name and
	// member hosts.
	Identity string `json:"identity"`

	// State is the follower's last report: running, catching_up, retrying,
	// degraded or stopped. It is not proof that a follower is running now.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Mode is exact while every stored change is the source's document at
	// that change, catch_up while the branch is converging after a gap.
	Mode    string `json:"mode"`
	HeadLSN int64  `json:"head_lsn"`
	// Position is the source cluster time the branch head has reached.
	Position       *ClusterTime `json:"position,omitempty"`
	ConsistentLSN  int64        `json:"consistent_lsn,omitempty"`
	ConsistentTime *ClusterTime `json:"consistent_cluster_time,omitempty"`
	ConsistentAt   *time.Time   `json:"consistent_at,omitempty"`

	// InexactRanges are spans of history that are not states the source was
	// in; InexactOpenFromLSN is set while one is still growing.
	InexactRanges        []InexactRange `json:"inexact_ranges"`
	InexactRangesDropped int            `json:"inexact_ranges_dropped,omitempty"`
	InexactOpenFromLSN   int64          `json:"inexact_open_from_lsn,omitempty"`
	InexactOpenReason    string         `json:"inexact_open_reason,omitempty"`

	// ResyncUnfinished is set while a resync has written to main and has not
	// yet brought it to a state of the source; ResyncStartedAt says since
	// when. `argon source resync` and `argon source follow --auto-resync`
	// can complete it.
	ResyncUnfinished bool       `json:"resync_unfinished,omitempty"`
	ResyncStartedAt  *time.Time `json:"resync_started_at,omitempty"`
	// Owner records the lease holder and its most recent heartbeat.
	Owner *SourceOwner `json:"owner,omitempty"`

	UpdatedAt      time.Time `json:"updated_at"`
	LastCapturedAt time.Time `json:"last_captured_at,omitempty"`
	LastEventAt    time.Time `json:"last_event_at,omitempty"`
	LastEventLagMS int64     `json:"last_event_lag_ms,omitempty"`
}

// SourceOwner describes the run recorded as holding the followed branch's lease.
type SourceOwner struct {
	Kind        string    `json:"kind"`
	Host        string    `json:"host"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// InexactRange is a span of a followed branch's history that is not exact.
type InexactRange struct {
	FromLSN     int64       `json:"from_lsn"`
	ToLSN       int64       `json:"to_lsn"`
	Reason      string      `json:"reason"`
	ConvergedAt ClusterTime `json:"converged_at"`
}

func (s *Services) followedBranch(ctx context.Context, projectName string) (*wal.Project, *wal.Branch, *source.Binding, error) {
	project, err := s.Projects.GetProjectByName(projectName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("project %q not found: %w", projectName, err)
	}
	branch, err := s.Branches.GetBranch(project.ID, "main")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("main branch of %q not found: %w", projectName, err)
	}
	binding, err := s.Sources.Get(ctx, branch.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if binding == nil || !branch.IsFollower() {
		return nil, nil, nil, fmt.Errorf("project %q does not follow an external database; only a project created by `argon import database --online` does", projectName)
	}
	return project, branch, binding, nil
}

// identifySource connects to the database and verifies the recorded binding.
func identifySource(ctx context.Context, binding *source.Binding, uri string) (*mongo.Database, source.Identity, error) {
	client, err := source.Connect(ctx, uri)
	if err != nil {
		return nil, source.Identity{}, err
	}
	identity, err := source.Identify(ctx, client)
	if err == nil {
		err = binding.Verify(identity)
	}
	if err != nil {
		disconnect(client)
		return nil, source.Identity{}, source.RedactError(err, uri)
	}
	return client.Database(binding.Database), identity, nil
}

func (s *Services) openSource(ctx context.Context, binding *source.Binding, uri string) (*mongo.Database, error) {
	db, identity, err := identifySource(ctx, binding, uri)
	if err != nil {
		return nil, err
	}
	if err := s.Sources.AdoptID(ctx, binding, identity); err != nil {
		disconnect(db.Client())
		return nil, source.RedactError(err, uri)
	}
	return db, nil
}

// prepareSourceBinding lets a following run adopt a newly readable replica
// set ID only inside a transaction that holds its lease.
func (s *Services) prepareSourceBinding(binding *source.Binding, identity source.Identity) func(context.Context) error {
	if binding.ReplicaSetID != "" || identity.ReplicaSetID == "" {
		return nil
	}
	return func(ctx context.Context) error {
		// AdoptID changes its argument. Each transaction attempt needs the
		// original binding while the adoption is still uncommitted.
		current := *binding
		return s.Sources.AdoptID(ctx, &current, identity)
	}
}

// SourceStatus reports what a project follows and the follower's last state.
func (s *Services) SourceStatus(ctx context.Context, projectName string) (*SourceInfo, error) {
	project, branch, binding, err := s.followedBranch(ctx, projectName)
	if err != nil {
		return nil, err
	}
	info := &SourceInfo{
		Project: project.Name, Branch: branch.Name, BranchID: branch.ID,
		Database: binding.Database, Location: binding.Display, ReplicaSet: binding.SetName, Identity: binding.Strength(),
		HeadLSN: branch.HeadLSN, State: "stopped", Mode: ingest.ModeExact, InexactRanges: []InexactRange{},
	}
	status, err := s.Ingest.SourceStatusFor(ctx, branch.ID)
	if err != nil {
		return nil, err
	}
	if status == nil {
		return info, nil
	}
	info.State, info.Error = status.State, status.Error
	info.UpdatedAt, info.LastCapturedAt = status.UpdatedAt, status.LastCapturedAt
	info.LastEventAt, info.LastEventLagMS = status.LastEventAt, status.LastEventLagMS
	if owner := status.Owner; owner != nil {
		info.Owner = &SourceOwner{Kind: owner.Kind, Host: owner.Host, PID: owner.PID, StartedAt: owner.StartedAt, HeartbeatAt: owner.HeartbeatAt}
	}
	if src := status.Source; src != nil {
		info.Mode = src.Mode
		position := clusterTime(src.Position)
		info.Position = &position
		if src.Consistent != nil {
			at, when := clusterTime(src.Consistent.ClusterTime), src.Consistent.At
			info.ConsistentLSN, info.ConsistentTime, info.ConsistentAt = src.Consistent.LSN, &at, &when
		}
		for _, r := range src.InexactRanges {
			info.InexactRanges = append(info.InexactRanges, InexactRange{FromLSN: r.FromLSN, ToLSN: r.ToLSN, Reason: r.Reason, ConvergedAt: clusterTime(r.ConvergedAt)})
		}
		info.InexactRangesDropped = src.InexactRangesDropped
		info.InexactOpenFromLSN, info.InexactOpenReason = src.InexactFromLSN, src.InexactReason
		if src.Resync != nil {
			started := src.Resync.StartedAt
			info.ResyncUnfinished, info.ResyncStartedAt = true, &started
		}
	}
	return info, nil
}

// FollowSourceOptions configures FollowSource.
type FollowSourceOptions struct {
	// URI is the source connection string. It is not stored.
	URI string
	// AllowInexact lets following continue past changes whose exact
	// documents the source can no longer serve; see ingest.FollowOptions.
	AllowInexact bool
	// EnableSourceImages turns on changeStreamPreAndPostImages for the
	// source's collections before following.
	EnableSourceImages bool
	// Ready is closed once the stream is open.
	Ready chan<- struct{}
	// AutoResync completes an unfinished resync or repairs a source stream
	// gap before following again. See ResyncSource for the effect on history.
	AutoResync bool
	// OnResync is called after each automatic resync. Optional.
	OnResync func(*ResyncInfo)
	// Watch replaces the source's Watch call; see ingest.FollowOptions.
	Watch func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error)
}

// autoResyncSpacing is the least time between two automatic resyncs. A
// source that ends following again sooner than this needs a person.
const autoResyncSpacing = time.Minute

// FollowSource keeps a project's main branch up with the database it was
// imported from until ctx ends, resuming from the saved position.
func (s *Services) FollowSource(ctx context.Context, projectName string, opts FollowSourceOptions) error {
	_, branch, binding, err := s.followedBranch(ctx, projectName)
	if err != nil {
		return err
	}
	db, identity, err := identifySource(ctx, binding, opts.URI)
	if err != nil {
		return err
	}
	defer disconnect(db.Client())
	prepare := s.prepareSourceBinding(binding, identity)
	var beforeStream func(context.Context) error
	if opts.EnableSourceImages {
		beforeStream = func(ctx context.Context) error {
			collections, err := source.Collections(ctx, db)
			if err == nil {
				_, err = source.EnableImages(ctx, db, collections)
			}
			if err != nil {
				return source.RedactError(err, opts.URI)
			}
			beforeStream = nil
			return nil
		}
	}
	var onReady func()
	if opts.Ready != nil {
		onReady = func() {
			close(opts.Ready)
			onReady = nil
		}
	}
	redact := func(text string) string { return source.Redact(text, opts.URI) }
	var lastResync time.Time
	for {
		err := s.Ingest.Follow(ctx, branch.ID, db, ingest.FollowOptions{
			AllowInexact: opts.AllowInexact, OnReady: onReady, Prepare: prepare, BeforeStream: beforeStream,
			WriteBacks: writeBackLedger{s.Merge}, Redact: redact, Watch: opts.Watch,
		})
		if !opts.AutoResync || ctx.Err() != nil {
			return err
		}
		var gap *ingest.CaptureError
		unfinished := errors.Is(err, ingest.ErrUnfinishedResync)
		if !unfinished && (!errors.As(err, &gap) || !gap.AutoResyncAllowed()) {
			return err
		}
		if !lastResync.IsZero() && time.Since(lastResync) < autoResyncSpacing {
			return fmt.Errorf("another automatic resync was needed within %s of the previous one: %w", autoResyncSpacing, err)
		}
		info, resyncErr := s.resyncSource(ctx, branch, db, ResyncSourceOptions{URI: opts.URI}, prepare)
		if resyncErr != nil {
			return fmt.Errorf("automatic resync failed after following stopped (%v): %w", err, resyncErr)
		}
		lastResync = time.Now()
		if opts.OnResync != nil {
			opts.OnResync(info)
		}
	}
}

// ResyncProgress is reported after every batch of a resync's comparison.
type ResyncProgress = ingest.ResyncProgress

// ResyncSourceOptions configures ResyncSource.
type ResyncSourceOptions struct {
	// URI is the source connection string. It is not stored.
	URI string
	// Force resyncs a project whose follower did not end as degraded.
	Force bool
	// BatchSize is the number of documents per batch (default 500).
	BatchSize int
	// Progress is called after every batch. Optional.
	Progress func(ResyncProgress)
	// Watch is as in ingest.ResyncOptions; tests only.
	Watch func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error)
}

// ResyncInfo describes a finished resync.
type ResyncInfo struct {
	Project  string `json:"project"`
	Database string `json:"database"`
	// Main at ConsistentLSN is the source at ConsistentTime. The LSNs from
	// InexactFromLSN to ConsistentLSN are recorded as an inexact range,
	// unless the resync had nothing to write.
	InexactFromLSN int64       `json:"inexact_from_lsn"`
	ConsistentLSN  int64       `json:"consistent_lsn"`
	ConsistentTime ClusterTime `json:"consistent_cluster_time"`
	// Compared counts the source documents read; Unchanged those main
	// already held; Puts and Deletes the entries the comparison wrote.
	Compared           int64    `json:"compared"`
	Unchanged          int64    `json:"unchanged"`
	Puts               int64    `json:"puts"`
	Deletes            int64    `json:"deletes"`
	Collections        []string `json:"collections"`
	RemovedCollections []string `json:"removed_collections"`
	DurationMS         int64    `json:"duration_ms"`
}

// ResyncSource brings a project's main branch to the current state of the
// database it follows, in place, after following has ended as degraded: a
// collection dropped or renamed on the source, or a saved position the
// source's oplog no longer holds. It reads the source and writes nothing to
// it. Afterwards FollowSource continues from the resync's end.
func (s *Services) ResyncSource(ctx context.Context, projectName string, opts ResyncSourceOptions) (*ResyncInfo, error) {
	_, branch, binding, err := s.followedBranch(ctx, projectName)
	if err != nil {
		return nil, err
	}
	db, identity, err := identifySource(ctx, binding, opts.URI)
	if err != nil {
		return nil, err
	}
	defer disconnect(db.Client())
	info, err := s.resyncSource(ctx, branch, db, opts, s.prepareSourceBinding(binding, identity))
	if info != nil {
		info.Project = projectName
	}
	return info, err
}

func (s *Services) resyncSource(ctx context.Context, branch *wal.Branch, db *mongo.Database, opts ResyncSourceOptions, prepare func(context.Context) error) (*ResyncInfo, error) {
	started := time.Now()
	result, err := s.Ingest.Resync(ctx, branch.ID, db, ingest.ResyncOptions{
		State: s.Materializer, Force: opts.Force, BatchSize: opts.BatchSize, Progress: opts.Progress,
		WriteBacks: writeBackLedger{s.Merge}, Watch: opts.Watch, Prepare: prepare,
		Redact: func(text string) string { return source.Redact(text, opts.URI) },
	})
	if err != nil {
		return nil, source.RedactError(err, opts.URI)
	}
	info := &ResyncInfo{
		Database: db.Name(), InexactFromLSN: result.FromLSN,
		ConsistentLSN: result.Point.LSN, ConsistentTime: clusterTime(result.Point.ClusterTime),
		Compared: result.Compared, Unchanged: result.Unchanged, Puts: result.Puts, Deletes: result.Deletes,
		Collections: result.SourceCollections, RemovedCollections: result.RemovedCollections,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if info.Collections == nil {
		info.Collections = []string{}
	}
	if info.RemovedCollections == nil {
		info.RemovedCollections = []string{}
	}
	return info, nil
}

// DetachSource stops a project from following its source for good. Its main
// branch keeps the history it has and becomes an ordinary branch.
func (s *Services) DetachSource(ctx context.Context, projectName string) error {
	_, branch, _, err := s.followedBranch(ctx, projectName)
	if err != nil {
		return err
	}
	if err := s.Ingest.Cancel(ctx, branch.ID); err != nil {
		return err
	}
	if err := s.Branches.SetFollows(ctx, branch.ID, ""); err != nil {
		return err
	}
	if err := s.Sources.Unbind(ctx, branch.ID); err != nil {
		return err
	}
	return s.Ingest.ClearResumeState(ctx, branch.ID)
}

// WriteBackMerge writes a pending plan to the external database its target
// branch follows, in one transaction that compares every document with the
// plan's before-image first. Nothing calls this implicitly.
func (s *Services) WriteBackMerge(ctx context.Context, planID primitive.ObjectID, strategy, uri string) (*merge.ApplyResult, error) {
	plan, err := s.Merge.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	binding, err := s.Sources.Get(ctx, plan.TargetBranchID)
	if err != nil {
		return nil, err
	}
	if binding == nil {
		return nil, errors.New("this plan's target branch does not follow an external database; apply it without --write-back")
	}
	// A plan is reviewed against the followed branch. When following has
	// ended on a gap, or a resync is under way, that branch is known not to
	// be the source, and so is what the reviewer saw.
	if status, err := s.Ingest.SourceStatusFor(ctx, plan.TargetBranchID); err != nil {
		return nil, err
	} else if status != nil {
		if status.State == "degraded" {
			return nil, fmt.Errorf("write-back refused: following ended as degraded (%s), so the branch this plan was reviewed against no longer matches the source. Run `argon source resync`, then preview again", status.Error)
		}
		if status.Source != nil && status.Source.Resync != nil {
			return nil, errors.New("write-back refused: a resync of the followed branch has not finished. Complete it with `argon source resync`, then preview again")
		}
	}
	db, err := s.openSource(ctx, binding, uri)
	if err != nil {
		return nil, err
	}
	defer disconnect(db.Client())
	result, err := s.Merge.ApplyToSource(ctx, planID, strategy, merge.WriteBackTarget{
		Database: db, Display: binding.Display,
		Redact: func(text string) string { return source.Redact(text, uri) },
	})
	return result, source.RedactError(err, uri)
}

// writeBackLedger lets the follower attribute and settle write-backs.
type writeBackLedger struct{ merge *merge.Service }

func (l writeBackLedger) Lookup(ctx context.Context, branchID string, session primitive.Binary) (*ingest.WriteBack, error) {
	plan, err := l.merge.LookupWriteBack(ctx, branchID, session)
	if err != nil || plan == nil {
		return nil, err
	}
	return &ingest.WriteBack{PlanID: plan.ID.Hex(), SourceBranchID: plan.SourceBranchID, SourceBranch: plan.SourceBranch}, nil
}

func (l writeBackLedger) Release(ctx context.Context, branchID string, claimedBefore time.Time) error {
	return l.merge.ReleaseWriteBacks(ctx, branchID, claimedBefore)
}

func (l writeBackLedger) Settle(ctx context.Context, planID string) error {
	id, err := primitive.ObjectIDFromHex(planID)
	if err != nil {
		return err
	}
	return l.merge.SettleWriteBack(ctx, id)
}
