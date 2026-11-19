package snapshot

import (
	"context"
	"errors"
	"fmt"

	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	keysTrusted   = "trusted"
	keysAmbiguous = "ambiguous"
	keysReclaimed = "reclaimed"
)

// The old boolean by itself could have certified an already reclaimed prefix.
// The partial index includes those decisions until this check replaces them.
func uncheckedManifests() bson.D {
	return bson.D{{Key: "exact_old_binary_keys", Value: nil}, {Key: "old_key_check", Value: nil}}
}

// CertifyOldSnapshots checks pending manifests within the caller's deadline.
// Failed checks leave a manifest available for retry at startup or on demand.
func (s *Service) CertifyOldSnapshots(ctx context.Context) {
	// Ordinary selection uses the branch/collection/LSN index. This index is
	// only for the upgrade pass, so building it shares the same deadline.
	index := bson.D{{Key: "old_key_check", Value: 1}}
	_, err := s.manifests.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: index, Options: options.Index().SetPartialFilterExpression(uncheckedManifests()),
	})
	if err != nil {
		return
	}
	cursor, err := s.manifests.Find(ctx, uncheckedManifests(), options.Find().SetHint(index))
	if err != nil {
		return
	}
	defer func() { _ = cursor.Close(ctx) }()
	pass := s.newCertificationPass()
	for ctx.Err() == nil && cursor.Next(ctx) {
		var snap Snapshot
		if cursor.Decode(&snap) != nil {
			continue
		}
		branch, err := pass.history.Branch(ctx, snap.BranchID)
		if err != nil {
			continue
		}
		_ = pass.certify(ctx, branch, &snap)
	}
}

type chunkKeys struct {
	oldBinary, shared bool
}

// A different copy can be certified independently if this one's chunks
// cannot be read. Other certification errors still abort selection.
type candidateChunkError struct{ error }

func (e candidateChunkError) Unwrap() error { return e.error }

type projectCollection struct {
	project, collection string
}

type certificationPass struct {
	service  *Service
	history  *materializer.OldBinarySnapshotPass
	chunks   map[string]chunkKeys
	evidence map[projectCollection]bool
}

func (s *Service) newCertificationPass() *certificationPass {
	return &certificationPass{
		service: s, history: s.materializer.NewOldBinarySnapshotPass(),
		chunks: make(map[string]chunkKeys), evidence: make(map[projectCollection]bool),
	}
}

func (p *certificationPass) certify(ctx context.Context, branch *wal.Branch, snap *Snapshot) error {
	history, err := p.history.Check(ctx, branch, snap.Collection, snap.LSN)
	if err != nil {
		return err
	}
	check, err := p.classify(ctx, snap, history)
	if err != nil {
		return err
	}
	fields := bson.M{"old_key_check": check, "old_keys_trusted": check == keysTrusted}
	if check != keysTrusted && history.Reclaimed != nil {
		fields["old_key_gap"] = history.Reclaimed
	}
	// GC can remove the last trace while two processes check the same manifest.
	// Preserve evidence observed by either process, even if the other finished first.
	filter := bson.M{"_id": snap.ID, "old_key_check": nil}
	if check != keysTrusted {
		filter["old_key_check"] = bson.M{"$in": bson.A{nil, keysTrusted}}
	}
	res, err := p.service.manifests.UpdateOne(ctx, filter, bson.M{"$set": fields})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return p.service.manifests.FindOne(ctx, bson.M{"_id": snap.ID}).Decode(snap)
	}
	snap.OldKeyCheck = check
	if check != keysTrusted && history.Reclaimed != nil {
		snap.OldKeyGap = history.Reclaimed
	}
	return nil
}

func (p *certificationPass) classify(ctx context.Context, snap *Snapshot, history materializer.OldBinarySnapshotHistory) (string, error) {
	if history.Ambiguous || (snap.OldKeysTrusted != nil && !*snap.OldKeysTrusted) {
		return keysAmbiguous, nil
	}
	keys, err := p.inspectChunks(ctx, snap.ChunkIDs)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", candidateChunkError{err}
	}
	if keys.shared {
		return keysAmbiguous, nil
	}
	if history.Reclaimed == nil {
		return keysTrusted, nil
	}
	if keys.oldBinary || history.Evidence {
		return keysReclaimed, nil
	}
	found, err := p.otherSnapshotEvidence(ctx, snap)
	if err != nil {
		return "", err
	}
	if found {
		return keysReclaimed, nil
	}
	return keysTrusted, nil
}

func (p *certificationPass) otherSnapshotEvidence(ctx context.Context, snap *Snapshot) (bool, error) {
	group := projectCollection{snap.ProjectID, snap.Collection}
	if seen, ok := p.evidence[group]; ok {
		return seen, nil
	}
	filter := bson.M{"project_id": snap.ProjectID, "collection": snap.Collection, "_id": bson.M{"$ne": snap.ID}}
	cur, err := p.service.manifests.Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 0, "chunk_ids": 1}))
	if err != nil {
		return false, err
	}
	defer func() { _ = cur.Close(ctx) }()
	for cur.Next(ctx) {
		var other Snapshot
		if err := cur.Decode(&other); err != nil {
			return false, err
		}
		keys, err := p.inspectChunks(ctx, other.ChunkIDs)
		if err != nil {
			return false, err
		}
		if keys.oldBinary {
			p.evidence[group] = true
			return true, nil
		}
	}
	if err := cur.Err(); err != nil {
		return false, err
	}
	// The caller already inspected this manifest and found no subtype-2 ID.
	p.evidence[group] = false
	return false, nil
}

func (p *certificationPass) inspectChunks(ctx context.Context, ids []string) (chunkKeys, error) {
	var result chunkKeys
	for _, id := range ids {
		keys, ok := p.chunks[id]
		if !ok {
			data, err := p.service.store.Get(ctx, id)
			if err != nil {
				return chunkKeys{}, err
			}
			keys, err = p.inspectChunk(ctx, data)
			if err != nil {
				return chunkKeys{}, fmt.Errorf("chunk %s: %w", id, err)
			}
			p.chunks[id] = keys
		}
		result.oldBinary = result.oldBinary || keys.oldBinary
		if keys.shared {
			result.shared = true
			break
		}
	}
	return result, nil
}

var errSharedKey = errors.New("old snapshot may have collapsed two old-binary document keys")

func (p *certificationPass) inspectChunk(ctx context.Context, data []byte) (chunkKeys, error) {
	var keys chunkKeys
	err := walkChunk(ctx, data, p.service.compressor, func(f frame) error {
		if wal.IsSharedOldBinaryKey(f.Key) {
			return errSharedKey
		}
		rawID := f.Doc.Lookup("_id")
		if rawID.IsZero() {
			// Before typed keys, a literal string ID could have the same
			// spelling as an envelope. Prefer the document whenever present.
			keys.oldBinary = keys.oldBinary || wal.IsOldBinaryKey(f.Key)
		} else if wal.IsOldBinaryID(rawID) {
			keys.oldBinary = true
			id, err := wal.DocumentIDFromImage(f.Doc)
			if err != nil {
				return err
			}
			if wal.IsSharedOldBinaryKey(wal.DocumentIDString(id)) {
				return errSharedKey
			}
		}
		return nil
	})
	if errors.Is(err, errSharedKey) {
		return chunkKeys{shared: true, oldBinary: true}, nil
	}
	return keys, err
}
