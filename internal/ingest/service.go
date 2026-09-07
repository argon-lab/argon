// Package ingest feeds a checked-out branch's WAL from its physical
// database's change stream. Applications write to the database with any
// MongoDB driver; the ingester converts each change event into a physical
// WAL entry (put with post-image / delete with pre-image), so branching,
// time travel, diff and undo keep working on directly-written data.
//
// Each batch commits its WAL entries, branch head and resume token in one
// MongoDB transaction, so a crash cannot checkpoint writes that were not stored.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// maxBatch bounds how many change events are appended in one WAL batch.
const maxBatch = 200

// Service turns change streams into WAL entries.
type Service struct {
	client   *mongo.Client
	wal      *wal.Service
	branches *branchwal.BranchService
	state    *mongo.Collection // wal_ingest_state: resume tokens per branch

	// Collections observed with pre/post images enabled, per run.
	seenMu       sync.Mutex
	seen         map[string]bool
	mu           sync.Mutex
	runs         map[string]*running
	statuses     map[string]Status
	autoSnapshot interface{ MaybeSnapshot(*wal.Branch) }
}

// NewService creates an ingester over the deployment holding both the
// Argon metadata and the physical branch databases.
func NewService(client *mongo.Client, metaDB *mongo.Database, walService *wal.Service, branches *branchwal.BranchService) *Service {
	return &Service{
		client:   client,
		wal:      walService,
		branches: branches,
		state:    metaDB.Collection("wal_ingest_state"),
		seen:     make(map[string]bool),
		runs:     make(map[string]*running),
		statuses: make(map[string]Status),
	}
}

// RunOption configures a Run invocation.
type RunOption func(*runConfig)

type runConfig struct {
	ready chan<- struct{}
	actor string
}

// WithActor labels all native writes captured on this branch/run. MongoDB
// change streams do not identify individual application actors.
func WithActor(actor string) RunOption { return func(c *runConfig) { c.actor = actor } }

func (s *Service) SetAutoSnapshotter(a interface{ MaybeSnapshot(*wal.Branch) }) { s.autoSnapshot = a }

// WithReady signals on the channel once the change stream is open — i.e.
// and its position is durably checkpointed. Checkout also persists a start
// timestamp so a first watcher can recover writes made before it opened.
func WithReady(ch chan<- struct{}) RunOption {
	return func(c *runConfig) { c.ready = ch }
}

// Run watches the branch's physical database until the context is
// canceled, converting every data change into WAL entries and advancing
// the branch head. It resumes from the persisted token when one exists, so
// restarts don't lose events.
func (s *Service) runStream(ctx context.Context, branchID string, cfg runConfig, run *running) error {
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return &ConfigurationError{fmt.Sprintf("branch %s not found", branchID)}
		}
		return fmt.Errorf("branch %s not found: %w", branchID, err)
	}
	if !branch.IsLive() {
		return &ConfigurationError{fmt.Sprintf("branch %s is not checked out; run checkout first", branch.Name)}
	}

	physical := s.client.Database(branch.PhysicalDB)

	csOpts := options.ChangeStream().
		SetBatchSize(maxBatch).
		SetFullDocument(options.WhenAvailable).
		SetFullDocumentBeforeChange(options.WhenAvailable).
		SetShowExpandedEvents(true).
		SetMaxAwaitTime(500 * time.Millisecond)

	if token, start, actor, err := s.loadResumeState(ctx, branchID); err != nil {
		return err
	} else {
		if token != nil {
			csOpts.SetResumeAfter(token)
		} else if start != nil {
			csOpts.SetStartAtOperationTime(start)
		}
		if cfg.actor == "" {
			cfg.actor = actor
		}
	}
	if cfg.actor == "" {
		cfg.actor = "ingest"
	}
	actorResult, err := s.state.UpdateOne(ctx,
		bson.M{"_id": branchID, "$or": bson.A{bson.M{"actor": cfg.actor}, bson.M{"actor": bson.M{"$exists": false}}}},
		bson.M{"$set": bson.M{"actor": cfg.actor}})
	if err != nil {
		return err
	}
	if actorResult.MatchedCount != 1 {
		return &ConfigurationError{"capture actor differs from this branch's persisted label; use its existing actor or create a new sandbox for a new run"}
	}
	collections, err := physical.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return err
	}
	for _, collection := range collections {
		if collection == barrierCollection {
			continue
		}
		if err := s.ensurePrePostImages(ctx, physical, collection); err != nil {
			return imageSetupError(err)
		}
	}

	stream, err := physical.Watch(ctx, mongo.Pipeline{}, csOpts)
	if err != nil {
		return fmt.Errorf("failed to open change stream on %s: %w", branch.PhysicalDB, err)
	}
	defer func() { _ = stream.Close(context.Background()) }()

	// Checkpoint the stream position immediately. Without a persisted
	// token a later restart would open the stream at "now" and silently
	// skip whatever landed while unsupervised; with this checkpoint the
	// capture gap closes at stream open instead of at the first event.
	if err := s.flush(branch, nil, stream.ResumeToken()); err != nil {
		return err
	}
	if err := s.report(branchID, "running", cfg.actor, ""); err != nil {
		return err
	}
	run.readyOnce.Do(func() {
		close(run.ready)
		if cfg.ready != nil {
			close(cfg.ready)
		}
	})

	batch := make([]*wal.Entry, 0, maxBatch)
	lastSafeToken := append(bson.Raw(nil), stream.ResumeToken()...)
	var transactionStartToken bson.Raw
	transactionBarrier := ""
	flushCanceled := func() error {
		complete, token := completePrefix(batch, lastSafeToken, transactionStartToken)
		return s.flush(branch, complete, token)
	}
	for {
		if ctx.Err() != nil {
			return flushCanceled()
		}

		if stream.TryNext(ctx) {
			var marker changeEvent
			if err := bson.Unmarshal(stream.Current, &marker); err != nil {
				return err
			}
			if marker.NS.Collection == barrierCollection {
				var acknowledgements []string
				internal, _ := marker.FullDocument.Lookup("internal").BooleanOK()
				if marker.OperationType == "insert" && !internal {
					acknowledgements = []string{fmt.Sprint(marker.DocumentKey.ID)}
				}
				if err := s.flush(branch, batch, stream.ResumeToken(), acknowledgements...); err != nil {
					return err
				}
				batch = batch[:0]
				transactionBarrier = ""
				lastSafeToken = append(lastSafeToken[:0], stream.ResumeToken()...)
				if marker.OperationType == "insert" {
					if internal {
						_, _ = physical.Collection(barrierCollection).DeleteOne(ctx, bson.M{"_id": marker.DocumentKey.ID})
					} else {
						s.ackBarrier(run, fmt.Sprint(marker.DocumentKey.ID))
					}
				}
				continue
			}
			entry, err := s.convertEvent(ctx, physical, branch, stream.Current)
			if err != nil {
				complete, token := batch, lastSafeToken
				if len(batch) > 0 && marker.txnID() != "" && marker.txnID() == batch[len(batch)-1].TxnID {
					complete, token = completePrefix(batch, lastSafeToken, transactionStartToken)
				}
				if flushErr := s.flush(branch, complete, token); flushErr != nil {
					return flushErr
				}
				return err
			}
			if entry != nil {
				entry.Actor = cfg.actor
				// Never publish a head inside a multi-document transaction. Once
				// the size target is reached, finish the group before flushing.
				if len(batch) >= maxBatch && batch[len(batch)-1].TxnID != "" && batch[len(batch)-1].TxnID != entry.TxnID {
					if err := s.flush(branch, batch, lastSafeToken); err != nil {
						return err
					}
					batch = batch[:0]
				}
				if len(batch) == 0 || batch[len(batch)-1].TxnID != entry.TxnID {
					transactionStartToken = append(transactionStartToken[:0], lastSafeToken...)
					transactionBarrier = ""
				}
				batch = append(batch, entry)
			}
			lastSafeToken = append(lastSafeToken[:0], stream.ResumeToken()...)
			if len(batch) < maxBatch || (len(batch) > 0 && batch[len(batch)-1].TxnID != "") {
				continue
			}
		}

		if err := stream.Err(); err != nil {
			if ctx.Err() != nil {
				return flushCanceled()
			}
			return fmt.Errorf("change stream error: %w", err)
		}

		// An empty server batch is not a transaction-end signal. A later
		// database marker proves the complete committed group precedes it,
		// including groups spanning cursor batches. Resume tokens stay opaque.
		if len(batch) > 0 && batch[len(batch)-1].TxnID != "" {
			if transactionBarrier == "" {
				transactionBarrier = primitive.NewObjectID().Hex()
				if _, err := physical.Collection(barrierCollection).InsertOne(ctx, bson.M{"_id": transactionBarrier, "internal": true}); err != nil {
					if ctx.Err() != nil {
						return flushCanceled()
					}
					return err
				}
			}
			continue
		}
		// Stream drained (or batch full): flush and persist the token.
		if len(batch) > 0 || len(stream.ResumeToken()) > 0 {
			if err := s.flush(branch, batch, stream.ResumeToken()); err != nil {
				return err
			}
			batch = batch[:0]
			lastSafeToken = append(lastSafeToken[:0], stream.ResumeToken()...)
		}
	}
}

// Without a following event/barrier, cancellation cannot prove the trailing
// transaction is complete. Replay that entire group after the saved boundary.
func completePrefix(batch []*wal.Entry, last, beforeTransaction bson.Raw) ([]*wal.Entry, bson.Raw) {
	if len(batch) == 0 || batch[len(batch)-1].TxnID == "" {
		return batch, last
	}
	txn := batch[len(batch)-1].TxnID
	end := len(batch)
	for end > 0 && batch[end-1].TxnID == txn {
		end--
	}
	return batch[:end], beforeTransaction
}

// changeEvent is the subset of change-stream event fields the ingester
// consumes.
type changeEvent struct {
	WallTime      time.Time `bson:"wallTime"`
	OperationType string    `bson:"operationType"`
	NS            struct {
		Collection string `bson:"coll"`
	} `bson:"ns"`
	DocumentKey struct {
		ID interface{} `bson:"_id"`
	} `bson:"documentKey"`
	FullDocument             bson.Raw `bson:"fullDocument"`
	FullDocumentBeforeChange bson.Raw `bson:"fullDocumentBeforeChange"`
	// Present on events that belong to a multi-document transaction.
	LSID      bson.Raw `bson:"lsid"`
	TxnNumber int64    `bson:"txnNumber"`
}

// txnID derives a stable identifier for the transaction an event belongs
// to, or "" for non-transactional writes.
func (e *changeEvent) txnID() string {
	if len(e.LSID) == 0 {
		return ""
	}
	sum := sha256.Sum256(e.LSID)
	return fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:8]), e.TxnNumber)
}

// convertEvent maps one change event to a WAL entry (nil for events that
// carry no document state).
func (s *Service) convertEvent(ctx context.Context, physical *mongo.Database, branch *wal.Branch, raw bson.Raw) (*wal.Entry, error) {
	var event changeEvent
	if err := bson.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("failed to decode change event: %w", err)
	}

	switch event.OperationType {
	case "insert", "update", "replace":
		if len(event.FullDocument) == 0 {
			return nil, &CaptureError{fmt.Sprintf("exact post-image unavailable for %s on %s; history is incomplete; enable changeStreamPreAndPostImages before writing", event.OperationType, event.NS.Collection)}
		}
		if err := s.ensurePrePostImages(ctx, physical, event.NS.Collection); err != nil {
			return nil, imageSetupError(err)
		}
		return &wal.Entry{
			ProjectID:  branch.ProjectID,
			BranchID:   branch.ID,
			Operation:  wal.OpPut,
			Collection: event.NS.Collection,
			DocumentID: wal.DocumentIDString(event.DocumentKey.ID),
			PostImage:  event.FullDocument,
			PreImage:   event.FullDocumentBeforeChange,
			TxnID:      event.txnID(),
			Actor:      "ingest",
			Metadata:   map[string]interface{}{"capture_operation": event.OperationType, "capture_wall_time": event.WallTime},
		}, nil

	case "delete":
		return &wal.Entry{
			ProjectID:  branch.ProjectID,
			BranchID:   branch.ID,
			Operation:  wal.OpDelete,
			Collection: event.NS.Collection,
			DocumentID: wal.DocumentIDString(event.DocumentKey.ID),
			PreImage:   event.FullDocumentBeforeChange,
			TxnID:      event.txnID(),
			Actor:      "ingest",
			Metadata:   map[string]interface{}{"capture_operation": event.OperationType, "capture_wall_time": event.WallTime},
		}, nil

	case "drop", "dropDatabase", "rename":
		// Collection-level DDL carries no document images; representing it
		// needs dedicated WAL operations (roadmap). Loud, not silent:
		return nil, &CaptureError{fmt.Sprintf("%s on %s is not captured; branch history is incomplete", event.OperationType, event.NS.Collection)}
	case "create":
		if err := s.ensurePrePostImages(ctx, physical, event.NS.Collection); err != nil {
			return nil, imageSetupError(err)
		}
		return nil, nil

	case "invalidate":
		return nil, &CaptureError{"change stream invalidated (physical database dropped?)"}

	default:
		return nil, nil
	}
}

// ensurePrePostImages enables exact images on collections the
// application created directly (checkout-created ones already have it).
func (s *Service) ensurePrePostImages(ctx context.Context, physical *mongo.Database, collection string) error {
	key := physical.Name() + "/" + collection
	s.seenMu.Lock()
	if s.seen[key] {
		s.seenMu.Unlock()
		return nil
	}
	s.seenMu.Unlock()
	if err := checkout.EnablePrePostImages(ctx, physical, collection); err != nil {
		return err
	}
	s.seenMu.Lock()
	s.seen[key] = true
	s.seenMu.Unlock()
	return nil
}

func imageSetupError(err error) error {
	if mongo.IsNetworkError(err) || mongo.IsTimeout(err) {
		return err
	}
	var command mongo.CommandError
	if errors.As(err, &command) {
		switch command.Name {
		case "BackgroundOperationInProgressForNamespace", "BackgroundOperationInProgressForDatabase", "ConflictingOperationInProgress", "LockBusy", "InterruptedDueToReplStateChange", "NotWritablePrimary", "PrimarySteppedDown", "InterruptedAtShutdown", "ShutdownInProgress":
			return err
		}
	}
	return &CaptureError{err.Error()}
}

// flush atomically appends a batch, advances the head and checkpoints the
// resume token. It
// runs on its own context: durability of already-received events must not
// depend on whether the caller is being canceled at that instant.
func (s *Service) flush(branch *wal.Branch, batch []*wal.Entry, token bson.Raw, acknowledgements ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var head int64
	// This timestamp samples checkpoint submission. It does not include commit
	// retries and is not a live queue-lag measurement or a durability SLA.
	capturedAt := time.Now()
	var eventAt time.Time
	for _, entry := range batch {
		if wall, ok := entry.Metadata["capture_wall_time"].(time.Time); ok && wall.After(eventAt) {
			eventAt = wall
		}
	}
	lagMS := int64(0)
	if !eventAt.IsZero() && capturedAt.After(eventAt) {
		lagMS = capturedAt.Sub(eventAt).Milliseconds()
	}
	_, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if len(batch) > 0 {
			lsns, err := s.wal.AppendBatchContext(sc, batch)
			if err != nil {
				return nil, fmt.Errorf("failed to append ingested entries: %w", err)
			}
			head = lsns[len(lsns)-1]
			if err := s.branches.CompareAndSetHead(sc, branch.ID, branch.HeadLSN, head); err != nil {
				return nil, err
			}
		} else {
			// A second process must not rewind a newer checkpoint with an
			// idle-stream token. Fence every checkpoint against the head read
			// with this stream's starting position, even for an empty batch.
			if err := s.branches.CheckpointHead(sc, branch.ID, branch.HeadLSN); err != nil {
				return nil, err
			}
		}
		if len(token) > 0 {
			fields := bson.M{"resume_token": token, "updated_at": capturedAt, "capture_status.last_captured_at": capturedAt, "capture_status.head_lsn": branch.HeadLSN}
			if len(batch) > 0 {
				fields["capture_status.head_lsn"] = head
			}
			if !eventAt.IsZero() {
				fields["capture_status.last_event_at"] = eventAt
				fields["capture_status.last_event_lag_ms"] = lagMS
			}
			_, err := s.state.UpdateOne(sc, bson.M{"_id": branch.ID},
				bson.M{"$set": fields}, options.Update().SetUpsert(true))
			if err != nil {
				return nil, fmt.Errorf("failed to persist resume token: %w", err)
			}
		}
		for _, marker := range acknowledgements {
			_, err := s.client.Database(branch.PhysicalDB).Collection(barrierCollection).UpdateOne(sc,
				bson.M{"_id": marker}, bson.M{"$set": bson.M{"captured": true}})
			if err != nil {
				return nil, fmt.Errorf("failed to acknowledge capture barrier: %w", err)
			}
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	if len(token) > 0 {
		s.mu.Lock()
		status := s.statuses[branch.ID]
		status.LastCapturedAt = capturedAt
		status.HeadLSN = branch.HeadLSN
		if len(batch) > 0 {
			status.HeadLSN = head
		}
		if !eventAt.IsZero() {
			status.LastEventAt = eventAt
			status.LastEventLagMS = lagMS
		}
		s.statuses[branch.ID] = status
		s.mu.Unlock()
	}
	if len(batch) > 0 {
		branch.HeadLSN = head
		if s.autoSnapshot != nil {
			for range batch {
				copy := *branch
				s.autoSnapshot.MaybeSnapshot(&copy)
			}
		}
	}
	return nil
}

func (s *Service) loadResumeState(ctx context.Context, branchID string) (bson.Raw, *primitive.Timestamp, string, error) {
	var doc struct {
		ResumeToken bson.Raw             `bson:"resume_token"`
		StartAt     *primitive.Timestamp `bson:"start_at"`
		Actor       string               `bson:"actor"`
	}
	err := s.state.FindOne(ctx, bson.M{"_id": branchID}).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil, "", nil
		}
		return nil, nil, "", fmt.Errorf("failed to load resume token: %w", err)
	}
	return doc.ResumeToken, doc.StartAt, doc.Actor, nil
}

// ClearResumeState drops the persisted token (used when a branch is
// re-checked out from scratch, which invalidates the old stream position).
func (s *Service) ClearResumeState(ctx context.Context, branchID string) error {
	_, err := s.state.DeleteOne(ctx, bson.M{"_id": branchID})
	if err == nil {
		s.mu.Lock()
		delete(s.statuses, branchID)
		s.mu.Unlock()
		s.seenMu.Lock()
		prefix := checkout.PhysicalDBName(branchID) + "/"
		for key := range s.seen {
			if strings.HasPrefix(key, prefix) {
				delete(s.seen, key)
			}
		}
		s.seenMu.Unlock()
	}
	return err
}
