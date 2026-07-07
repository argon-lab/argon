// Package ingest feeds a checked-out branch's WAL from its physical
// database's change stream. Applications write to the database with any
// MongoDB driver; the ingester converts each change event into a physical
// WAL entry (put with post-image / delete with pre-image), so branching,
// time travel, diff and undo keep working on directly-written data.
//
// Delivery is at-least-once: the resume token is persisted after entries
// are appended, so a crash between the two can re-deliver events. That is
// safe by construction — puts and deletes replay idempotently — at the cost
// of occasional duplicate entries in the log.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
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
	seenMu sync.Mutex
	seen   map[string]bool
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
	}
}

// RunOption configures a Run invocation.
type RunOption func(*runConfig)

type runConfig struct {
	ready chan<- struct{}
}

// WithReady signals on the channel once the change stream is open — i.e.
// once writes are guaranteed to be captured. Without a persisted resume
// token the stream starts "now": writes made between checkout and stream
// open are not captured, so callers that need a hard guarantee should wait
// for readiness before writing.
func WithReady(ch chan<- struct{}) RunOption {
	return func(c *runConfig) { c.ready = ch }
}

// Run watches the branch's physical database until the context is
// canceled, converting every data change into WAL entries and advancing
// the branch head. It resumes from the persisted token when one exists, so
// restarts don't lose events.
func (s *Service) Run(ctx context.Context, branchID string, opts ...RunOption) error {
	var cfg runConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return fmt.Errorf("branch %s not found: %w", branchID, err)
	}
	if !branch.IsLive() {
		return fmt.Errorf("branch %s is not checked out; run checkout first", branch.Name)
	}

	physical := s.client.Database(branch.PhysicalDB)

	csOpts := options.ChangeStream().
		SetFullDocument(options.UpdateLookup).
		SetFullDocumentBeforeChange(options.WhenAvailable).
		SetMaxAwaitTime(500 * time.Millisecond)

	if token, err := s.loadResumeToken(ctx, branchID); err != nil {
		return err
	} else if token != nil {
		csOpts.SetResumeAfter(token)
	}

	stream, err := physical.Watch(ctx, mongo.Pipeline{}, csOpts)
	if err != nil {
		return fmt.Errorf("failed to open change stream on %s: %w", branch.PhysicalDB, err)
	}
	defer func() { _ = stream.Close(context.Background()) }()

	if cfg.ready != nil {
		close(cfg.ready)
	}

	batch := make([]*wal.Entry, 0, maxBatch)
	for {
		if ctx.Err() != nil {
			// Drain what we have, then stop.
			return s.flush(branch, batch, stream.ResumeToken())
		}

		if stream.TryNext(ctx) {
			entry, err := s.convertEvent(ctx, physical, branch, stream.Current)
			if err != nil {
				return err
			}
			if entry != nil {
				batch = append(batch, entry)
			}
			if len(batch) < maxBatch {
				continue
			}
		}

		if err := stream.Err(); err != nil {
			if ctx.Err() != nil {
				return s.flush(branch, batch, stream.ResumeToken())
			}
			return fmt.Errorf("change stream error: %w", err)
		}

		// Stream drained (or batch full): flush and persist the token.
		if len(batch) > 0 {
			if err := s.flush(branch, batch, stream.ResumeToken()); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
}

// changeEvent is the subset of change-stream event fields the ingester
// consumes.
type changeEvent struct {
	OperationType string `bson:"operationType"`
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
			// The document vanished between the update and the lookup; the
			// upcoming delete event carries the removal.
			log.Printf("ingest: skipping %s on %s without a post-image (document deleted before lookup)",
				event.OperationType, event.NS.Collection)
			return nil, nil
		}
		s.ensurePrePostImages(ctx, physical, event.NS.Collection)
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
		}, nil

	case "drop", "dropDatabase", "rename":
		// Collection-level DDL carries no document images; representing it
		// needs dedicated WAL operations (roadmap). Loud, not silent:
		log.Printf("ingest: %s on %s is not captured in the WAL yet — branch history for this collection is now incomplete",
			event.OperationType, event.NS.Collection)
		return nil, nil

	case "invalidate":
		return nil, fmt.Errorf("change stream invalidated (physical database dropped?)")

	default:
		return nil, nil
	}
}

// ensurePrePostImages best-effort enables exact images on collections the
// application created directly (checkout-created ones already have it).
func (s *Service) ensurePrePostImages(ctx context.Context, physical *mongo.Database, collection string) {
	s.seenMu.Lock()
	if s.seen[collection] {
		s.seenMu.Unlock()
		return
	}
	s.seen[collection] = true
	s.seenMu.Unlock()
	_ = checkout.EnablePrePostImages(ctx, physical, collection)
}

// flush appends a batch, advances the branch head and persists the resume
// token — in that order, so a crash can only re-deliver, never lose. It
// runs on its own context: durability of already-received events must not
// depend on whether the caller is being canceled at that instant.
func (s *Service) flush(branch *wal.Branch, batch []*wal.Entry, token bson.Raw) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if len(batch) > 0 {
		lsns, err := s.wal.AppendBatch(batch)
		if err != nil {
			return fmt.Errorf("failed to append ingested entries: %w", err)
		}
		if err := s.branches.UpdateBranchHead(branch.ID, lsns[len(lsns)-1]); err != nil {
			return fmt.Errorf("failed to advance branch head: %w", err)
		}
	}
	if len(token) == 0 {
		return nil
	}
	_, err := s.state.UpdateOne(ctx,
		bson.M{"_id": branch.ID},
		bson.M{"$set": bson.M{"resume_token": token, "updated_at": time.Now()}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("failed to persist resume token: %w", err)
	}
	return nil
}

func (s *Service) loadResumeToken(ctx context.Context, branchID string) (bson.Raw, error) {
	var doc struct {
		ResumeToken bson.Raw `bson:"resume_token"`
	}
	err := s.state.FindOne(ctx, bson.M{"_id": branchID}).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to load resume token: %w", err)
	}
	return doc.ResumeToken, nil
}

// ClearResumeState drops the persisted token (used when a branch is
// re-checked out from scratch, which invalidates the old stream position).
func (s *Service) ClearResumeState(ctx context.Context, branchID string) error {
	_, err := s.state.DeleteOne(ctx, bson.M{"_id": branchID})
	return err
}
