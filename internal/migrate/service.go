// Package migrate rewrites legacy schema-v1 WAL entries into the v2
// physical-log format in place.
//
// v1 logged updates and deletes as their original filter/update expressions;
// v2 logs the resolved outcome (post/pre-images), which is what makes replay
// deterministic. Migration therefore has to resolve those expressions one
// final time: branches are processed parents-first, each branch's data
// entries are walked in LSN order over a materialized running state, and
// every legacy entry is rewritten as a put/delete with images — preserving
// its LSN. Legacy update/delete entries that matched nothing are removed
// entirely; the resulting LSN gaps are harmless because WAL consumers rely
// on ordering, never density.
//
// Fidelity note: v1 replay resolved "first match" through randomized Go map
// iteration, so for multi-match filters there is no single historical truth
// to reproduce. Migration resolves first-match in sorted document-ID order,
// the same deterministic rule the v2 write path uses.
package migrate

import (
	"context"
	"fmt"
	"sort"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service migrates a project's WAL from schema v1 to v2.
type Service struct {
	db           *mongo.Database
	walLog       *mongo.Collection
	branches     *branchwal.BranchService
	materializer *materializer.Service
	compressor   *wal.Compressor
}

// NewService creates a migration service over the given Argon database.
func NewService(db *mongo.Database, branches *branchwal.BranchService, mat *materializer.Service) (*Service, error) {
	compressor, err := wal.NewCompressor(nil)
	if err != nil {
		return nil, err
	}
	return &Service{
		db:           db,
		walLog:       db.Collection("wal_log"),
		branches:     branches,
		materializer: mat,
		compressor:   compressor,
	}, nil
}

// Result summarizes one project's migration.
type Result struct {
	ProjectID        string
	BranchesVisited  int
	EntriesRewritten int
	EntriesRemoved   int
}

// legacyEntry is the raw shape of a wal_log document, including the v1
// payload fields that the v2 Entry model no longer has.
type legacyEntry struct {
	ID            interface{}       `bson:"_id"`
	SchemaVersion int               `bson:"v"`
	LSN           int64             `bson:"lsn"`
	BranchID      string            `bson:"branch_id"`
	Operation     wal.OperationType `bson:"operation"`
	Collection    string            `bson:"collection"`
	DocumentID    string            `bson:"document_id"`
	// v1 payloads
	CompressedDocument    []byte `bson:"compressed_document"`
	CompressedOldDocument []byte `bson:"compressed_old_document"`
	// v2 payloads (present on entries written after the upgrade)
	CompressedPostImage []byte                 `bson:"post"`
	Metadata            map[string]interface{} `bson:"metadata"`
}

func (e *legacyEntry) isLegacyData() bool {
	if e.SchemaVersion >= wal.EntrySchemaVersion {
		return false
	}
	switch e.Operation {
	case wal.LegacyOpInsert, wal.LegacyOpUpdate, wal.LegacyOpDelete:
		return true
	default:
		return false
	}
}

// MigrateProject rewrites all legacy entries of a project in place and
// returns a summary. It is idempotent: a second run finds no legacy entries
// and changes nothing.
func (s *Service) MigrateProject(ctx context.Context, projectID string) (*Result, error) {
	branches, err := s.branches.ListBranchesAny(projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	// Parents were necessarily created before their children, so processing
	// in creation order guarantees every ancestor is already migrated when
	// a child materializes its fork-point state through the ancestry chain.
	sort.Slice(branches, func(i, j int) bool { return branches[i].CreatedLSN < branches[j].CreatedLSN })

	result := &Result{ProjectID: projectID}
	for _, branch := range branches {
		rewritten, removed, err := s.migrateBranch(ctx, branch)
		if err != nil {
			return nil, fmt.Errorf("branch %s (%s): %w", branch.Name, branch.ID, err)
		}
		result.BranchesVisited++
		result.EntriesRewritten += rewritten
		result.EntriesRemoved += removed
	}
	return result, nil
}

func (s *Service) migrateBranch(ctx context.Context, branch *wal.Branch) (rewritten, removed int, err error) {
	// Starting state: everything inherited from (already migrated)
	// ancestors up to the fork point.
	state, err := s.materializer.MaterializeBranchAtLSN(branch, branch.BaseLSN)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to materialize fork state: %w", err)
	}

	// Buffer the branch's entries before rewriting any of them: mutating
	// documents underneath a live cursor on the same collection risks
	// re-visits or skips depending on the query plan.
	cursor, err := s.walLog.Find(ctx,
		bson.M{"branch_id": branch.ID, "lsn": bson.M{"$gt": branch.BaseLSN}},
		options.Find().SetSort(bson.M{"lsn": 1}),
	)
	if err != nil {
		return 0, 0, err
	}
	var entries []legacyEntry
	if err := cursor.All(ctx, &entries); err != nil {
		return 0, 0, fmt.Errorf("failed to load entries: %w", err)
	}

	for i := range entries {
		entry := entries[i]
		if entry.Collection == "" {
			continue // Control entries need no migration.
		}
		collState := stateFor(state, entry.Collection)

		if !entry.isLegacyData() {
			// Already v2: fold it into the running state and move on.
			if err := s.applyV2(&entry, collState); err != nil {
				return rewritten, removed, err
			}
			continue
		}

		didRewrite, didRemove, err := s.migrateEntry(ctx, &entry, collState)
		if err != nil {
			return rewritten, removed, fmt.Errorf("entry LSN %d: %w", entry.LSN, err)
		}
		if didRewrite {
			rewritten++
		}
		if didRemove {
			removed++
		}
	}
	return rewritten, removed, nil
}

// applyV2 folds an already-migrated entry into the running state.
func (s *Service) applyV2(entry *legacyEntry, collState map[string]bson.M) error {
	switch entry.Operation {
	case wal.OpPut:
		doc, err := s.decompressDoc(entry.CompressedPostImage)
		if err != nil {
			return err
		}
		collState[entry.DocumentID] = doc
	case wal.OpDelete:
		delete(collState, entry.DocumentID)
	}
	return nil
}

// migrateEntry resolves one legacy entry against the running state and
// rewrites (or removes) it.
func (s *Service) migrateEntry(ctx context.Context, entry *legacyEntry, collState map[string]bson.M) (rewritten, removed bool, err error) {
	switch entry.Operation {
	case wal.LegacyOpInsert:
		doc, err := s.decompressDoc(entry.CompressedDocument)
		if err != nil {
			return false, false, err
		}
		docID := entry.DocumentID
		if docID == "" {
			id, exists := doc["_id"]
			if !exists {
				return false, false, fmt.Errorf("legacy insert has no document ID")
			}
			docID = wal.DocumentIDString(id)
		}
		if err := s.rewriteAsPut(ctx, entry, docID, doc, collState[docID]); err != nil {
			return false, false, err
		}
		collState[docID] = doc
		return true, false, nil

	case wal.LegacyOpUpdate:
		payload, err := s.decompressDoc(entry.CompressedDocument)
		if err != nil {
			return false, false, err
		}
		filter, update, err := decodeLegacyUpdatePayload(payload)
		if err != nil {
			return false, false, err
		}
		docID, oldDoc, found, err := firstMatch(collState, filter)
		if err != nil {
			return false, false, err
		}
		if !found {
			return false, true, s.removeEntry(ctx, entry)
		}
		newDoc, err := driverwal.ApplyUpdate(oldDoc, update, false)
		if err != nil {
			return false, false, err
		}
		if err := s.rewriteAsPut(ctx, entry, docID, newDoc, oldDoc); err != nil {
			return false, false, err
		}
		collState[docID] = newDoc
		return true, false, nil

	case wal.LegacyOpDelete:
		filter, err := s.decompressDoc(entry.CompressedDocument)
		if err != nil {
			return false, false, err
		}
		docID, oldDoc, found, err := firstMatch(collState, filter)
		if err != nil {
			return false, false, err
		}
		if !found {
			return false, true, s.removeEntry(ctx, entry)
		}
		if err := s.rewriteAsDelete(ctx, entry, docID, oldDoc); err != nil {
			return false, false, err
		}
		delete(collState, docID)
		return true, false, nil
	}
	return false, false, nil
}

func (s *Service) rewriteAsPut(ctx context.Context, entry *legacyEntry, docID string, post, pre bson.M) error {
	postBytes, err := s.compressDoc(post)
	if err != nil {
		return err
	}
	set := bson.M{
		"v":           wal.EntrySchemaVersion,
		"operation":   wal.OpPut,
		"document_id": docID,
		"post":        postBytes,
	}
	if pre != nil {
		preBytes, err := s.compressDoc(pre)
		if err != nil {
			return err
		}
		set["pre"] = preBytes
	}
	_, err = s.walLog.UpdateOne(ctx,
		bson.M{"_id": entry.ID},
		bson.M{
			"$set":   set,
			"$unset": bson.M{"compressed_document": "", "compressed_old_document": ""},
		},
	)
	return err
}

func (s *Service) rewriteAsDelete(ctx context.Context, entry *legacyEntry, docID string, pre bson.M) error {
	preBytes, err := s.compressDoc(pre)
	if err != nil {
		return err
	}
	_, err = s.walLog.UpdateOne(ctx,
		bson.M{"_id": entry.ID},
		bson.M{
			"$set": bson.M{
				"v":           wal.EntrySchemaVersion,
				"operation":   wal.OpDelete,
				"document_id": docID,
				"pre":         preBytes,
			},
			"$unset": bson.M{"compressed_document": "", "compressed_old_document": ""},
		},
	)
	return err
}

func (s *Service) removeEntry(ctx context.Context, entry *legacyEntry) error {
	// The operation matched nothing, so it contributed no state; dropping
	// it leaves an LSN gap, which is legal.
	_, err := s.walLog.DeleteOne(ctx, bson.M{"_id": entry.ID})
	return err
}

func (s *Service) decompressDoc(compressed []byte) (bson.M, error) {
	if len(compressed) == 0 {
		return nil, fmt.Errorf("entry has no payload")
	}
	raw, err := s.compressor.Decompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress payload: %w", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("failed to decode payload: %w", err)
	}
	return doc, nil
}

func (s *Service) compressDoc(doc bson.M) ([]byte, error) {
	raw, err := bson.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return s.compressor.Compress(raw)
}

// decodeLegacyUpdatePayload unpacks the v1 {filter, update} envelope.
func decodeLegacyUpdatePayload(payload bson.M) (filter, update bson.M, err error) {
	toDoc := func(v interface{}, name string) (bson.M, error) {
		switch d := v.(type) {
		case bson.M:
			return d, nil
		case bson.Raw:
			var doc bson.M
			if err := bson.Unmarshal(d, &doc); err != nil {
				return nil, fmt.Errorf("failed to decode legacy %s: %w", name, err)
			}
			return doc, nil
		default:
			// Whatever BSON shape the driver decoded it into: round-trip
			// it through marshalling to normalize to a map.
			raw, err := bson.Marshal(bson.M{"v": v})
			if err != nil {
				return nil, fmt.Errorf("failed to normalize legacy %s: %w", name, err)
			}
			var wrapper struct {
				V bson.M `bson:"v"`
			}
			if err := bson.Unmarshal(raw, &wrapper); err != nil {
				return nil, fmt.Errorf("failed to normalize legacy %s: %w", name, err)
			}
			return wrapper.V, nil
		}
	}
	filter, err = toDoc(payload["filter"], "filter")
	if err != nil {
		return nil, nil, err
	}
	update, err = toDoc(payload["update"], "update")
	if err != nil {
		return nil, nil, err
	}
	return filter, update, nil
}

// firstMatch resolves a filter against a collection state in sorted
// document-ID order.
func firstMatch(collState map[string]bson.M, filter bson.M) (string, bson.M, bool, error) {
	docIDs := make([]string, 0, len(collState))
	for docID := range collState {
		docIDs = append(docIDs, docID)
	}
	sort.Strings(docIDs)

	for _, docID := range docIDs {
		matched, err := driverwal.MatchesFilter(collState[docID], filter)
		if err != nil {
			return "", nil, false, err
		}
		if matched {
			return docID, collState[docID], true, nil
		}
	}
	return "", nil, false, nil
}

func stateFor(state map[string]map[string]bson.M, collection string) map[string]bson.M {
	if _, exists := state[collection]; !exists {
		state[collection] = make(map[string]bson.M)
	}
	return state[collection]
}
