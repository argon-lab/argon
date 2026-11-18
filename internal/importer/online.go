package importer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/internal/wal"
)

// Follower is the part of the ingest service an online import needs.
type Follower interface {
	InitSourceState(ctx context.Context, branchID string) error
	CatchUp(ctx context.Context, branchID string, src *mongo.Database, startAt primitive.Timestamp, firstLSN int64, reason string, redact func(string) string) (*ingest.ConsistentPoint, error)
}

// EnableOnline wires what an online import needs beyond a quiesced one.
func (s *ImportService) EnableOnline(sources *source.Registry, follower Follower) {
	s.sources, s.follower = sources, follower
}

// importOnline imports a database that keeps being written.
//
// The order of the first steps matters: boundary, then the list of
// collections, then the reads.
//
// The copy reads each collection with ordinary cursors, so different
// documents are read at different moments and the copy alone is not a state
// the source was ever in. What makes it one is the source's change stream:
// it is replayed from a cluster time taken before the first read, on top of
// the copy, until it has passed every read. Entries are whole documents, so
// replaying a change the copy already contained is harmless, and at the end
// the project holds exactly what the source held at one cluster time. No
// write made during the import is lost, because every one of them is either
// in the copy, in the replayed stream, or both.
//
// Three things keep that argument sound:
//
//   - The reads are majority reads in one causally consistent session that
//     starts at the stream boundary. A read can therefore never show less
//     than the boundary, and never a write that could later roll back and
//     for which no change event would arrive.
//   - The replay ends only after the stream's position has passed the
//     primary's cluster time taken after the last read.
//   - The stream boundary must still be in the source's oplog when the copy
//     ends. If the copy outlasts the oplog window the import fails; it does
//     not publish a project it cannot prove consistent.
func (s *ImportService) importOnline(ctx context.Context, opts ImportOptions) (_ *ImportResult, err error) {
	started := time.Now()
	redact := func(text string) string { return source.Redact(text, opts.MongoURI) }
	defer func() { err = source.RedactError(err, opts.MongoURI) }()
	if s.sources == nil || s.follower == nil {
		return nil, errors.New("online import is not available in this process")
	}
	if err := s.sources.ValidateDatabase(opts.DatabaseName); err != nil {
		return nil, err
	}
	if existing, lookupErr := s.projectService.GetProjectByName(opts.ProjectName); lookupErr == nil && existing != nil {
		return nil, fmt.Errorf("project '%s' already exists", opts.ProjectName)
	}

	client, err := source.Connect(ctx, opts.MongoURI)
	if err != nil {
		return nil, err
	}
	defer func() {
		disconnect, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(disconnect)
	}()
	identity, err := source.Identify(ctx, client)
	if err != nil {
		return nil, err
	}
	sourceDB := client.Database(opts.DatabaseName)

	// The stream boundary comes first, and the list of collections to copy
	// is read from the primary after it. A collection created before the
	// boundary is then in the list; one created after it arrives through the
	// stream. Taking the list first would leave a moment in which a new
	// collection is in neither.
	startAt, err := ingest.PrimaryTime(ctx, sourceDB)
	if err != nil {
		return nil, err
	}
	collections, err := importCollections(ctx, client.Database(opts.DatabaseName, options.Database().SetReadPreference(readpref.Primary())), source.SkipCollection, true)
	if err != nil {
		return nil, err
	}
	// Open the change stream once now. The replay only starts after the
	// copy, which can take hours; a user without the changeStream privilege,
	// or a source that cannot serve the boundary, should fail here.
	if err := probeStream(ctx, sourceDB, startAt); err != nil {
		return nil, err
	}

	result := &ImportResult{Online: true, Follows: opts.DatabaseName, Collections: make([]string, 0, len(collections))}
	// Collections whose images this import switched on. Any failure from
	// here on reports them: the source stays altered when nothing is
	// published.
	var imagesEnabled []string
	defer func() { err = source.ImagesLeftEnabled(err, imagesEnabled) }()

	project, err := s.projectService.BeginImport(ctx, opts.ProjectName)
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		// Removes the unpublished project, its history, its binding and its
		// stream position. All of it lives in the metadata database.
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := s.projectService.AbortImport(cleanup, project.ID); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup incomplete for %q; stop this importer, then run argon import cleanup --project %q --yes: %w", opts.ProjectName, opts.ProjectName, cleanupErr))
		}
	}()
	branch, err := s.branchService.GetBranch(project.ID, "main")
	if err != nil {
		return nil, fmt.Errorf("failed to get main branch: %w", err)
	}
	// Bind before the first read: from here on no Argon path can write to
	// this branch except the follower, and none can drop its source.
	if err := s.branchService.SetFollows(ctx, branch.ID, opts.DatabaseName); err != nil {
		return nil, fmt.Errorf("failed to mark main as following the source: %w", err)
	}
	if err := s.sources.Bind(ctx, &source.Binding{
		BranchID: branch.ID, ProjectID: project.ID, Database: opts.DatabaseName,
		Display: source.Display(opts.MongoURI, opts.DatabaseName),
		SetName: identity.SetName, ReplicaSetID: identity.ReplicaSetID, Hosts: identity.Hosts,
	}); err != nil {
		return nil, err
	}
	if err := s.follower.InitSourceState(ctx, branch.ID); err != nil {
		return nil, fmt.Errorf("failed to create the source stream state: %w", err)
	}
	// Only now, with the project reserved and bound, is the source altered,
	// and only because the operator asked for it by name.
	if opts.EnableSourceImages {
		imagesEnabled, err = source.EnableImages(ctx, sourceDB, collections)
		if err != nil {
			// EnableImages has already named what it changed.
			failed := err
			imagesEnabled = nil
			return nil, failed
		}
		result.ImagesEnabled = imagesEnabled
	}
	if branch, err = s.branchService.GetBranchByID(branch.ID); err != nil {
		return nil, err
	}
	result.ProjectID, result.BranchID = project.ID, branch.ID
	result.StartLSN = s.walService.GetCurrentLSN(project.ID)

	session, err := client.StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(context.Background())
	// The session keeps the pointer it is given, so it gets its own copy.
	readsFrom := startAt
	if err := session.AdvanceOperationTime(&readsFrom); err != nil {
		return nil, err
	}
	// Every read below is causally at or after startAt, so the copy already
	// contains whatever committed at that cluster time; the stream starts
	// with the next one.
	if startAt.I++; startAt.I == 0 {
		startAt.T++
	}
	reads := mongo.NewSessionContext(ctx, session)
	for _, name := range collections {
		collection := sourceDB.Collection(name, options.Collection().SetReadConcern(readconcern.Majority()))
		copied, err := s.copyCollection(reads, collection, branch, opts.BatchSize)
		if err != nil {
			return nil, fmt.Errorf("failed to import collection %s: %w", name, err)
		}
		result.ImportedDocs += copied
		result.WALEntries += copied
		result.Collections = append(result.Collections, name)
	}

	point, err := s.follower.CatchUp(ctx, branch.ID, sourceDB, startAt, result.StartLSN+1,
		"online import: the source was written while it was copied", redact)
	if err != nil {
		return nil, fmt.Errorf("the copy finished, but catching up with the changes made during it did not: %w. Nothing was published. If the source's oplog no longer reaches back to the start of the copy, the copy took longer than the oplog window; retry when the source is quieter or give it a larger oplog", err)
	}
	result.ConsistentLSN, result.ConsistentClusterTime = point.LSN, &point.ClusterTime
	result.EndLSN = s.walService.GetCurrentLSN(project.ID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.projectService.CompleteImport(ctx, project.ID); err != nil {
		return nil, fmt.Errorf("failed to publish imported project: %w", err)
	}
	if s.onImported != nil {
		if fresh, lookupErr := s.branchService.GetBranchByID(branch.ID); lookupErr == nil {
			s.onImported(fresh)
		}
	}
	result.Duration = time.Since(started)
	return result, nil
}

// probeStream opens and closes the source's change stream at the boundary.
func probeStream(ctx context.Context, db *mongo.Database, startAt primitive.Timestamp) error {
	stream, err := db.Watch(ctx, mongo.Pipeline{}, options.ChangeStream().
		SetStartAtOperationTime(&startAt).SetMaxAwaitTime(100*time.Millisecond))
	if err != nil {
		return fmt.Errorf("the source's change stream cannot be opened, so the changes made during the copy could not be replayed (the source user needs the changeStream action on database %s): %w", db.Name(), err)
	}
	stream.TryNext(ctx)
	err = stream.Err()
	_ = stream.Close(context.Background())
	if err != nil {
		return fmt.Errorf("the source's change stream cannot be read: %w", err)
	}
	return nil
}

// copyCollection appends every document of one source collection as a put,
// storing the bytes the source returned.
func (s *ImportService) copyCollection(ctx context.Context, collection *mongo.Collection, branch *wal.Branch, batchSize int) (int64, error) {
	cursor, err := collection.Find(ctx, bson.D{})
	if err != nil {
		return 0, fmt.Errorf("failed to create cursor: %w", err)
	}
	defer func() { _ = cursor.Close(context.Background()) }()

	var copied int64
	entries := make([]*wal.Entry, 0, batchSize)
	flush := func() error {
		if len(entries) == 0 {
			return nil
		}
		if err := s.appendImportBatch(branch, entries); err != nil {
			return fmt.Errorf("failed to process batch: %w", err)
		}
		copied += int64(len(entries))
		entries = entries[:0]
		return nil
	}
	for cursor.Next(ctx) {
		var id interface{}
		if err := cursor.Current.Lookup("_id").Unmarshal(&id); err != nil || id == nil {
			return copied, fmt.Errorf("imported document in %s has no _id", collection.Name())
		}
		entries = append(entries, &wal.Entry{
			ProjectID: branch.ProjectID, BranchID: branch.ID,
			Operation: wal.OpPut, Collection: collection.Name(),
			DocumentID: wal.DocumentIDString(id),
			PostImage:  append(bson.Raw(nil), cursor.Current...),
			Actor:      "importer",
		})
		if len(entries) >= batchSize {
			if err := flush(); err != nil {
				return copied, err
			}
		}
	}
	if err := cursor.Err(); err != nil {
		return copied, fmt.Errorf("cursor error: %w", err)
	}
	return copied, flush()
}
