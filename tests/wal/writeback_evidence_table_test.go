package wal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/bsonrw"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type sourceDifference struct {
	name         string
	before, live bson.D // fields after _id; nil means no source document
}

func existingSourceDifferences(t *testing.T) []sourceDifference {
	t.Helper()
	field := func(v any) bson.D { return bson.D{{Key: "v", Value: v}} }
	change := func(name string, before, live any) sourceDifference {
		return sourceDifference{name: name, before: field(before), live: field(live)}
	}
	decimal, err := primitive.ParseDecimal128("1.0")
	require.NoError(t, err)
	otherScale, err := primitive.ParseDecimal128("1.00")
	require.NoError(t, err)
	shape := bson.D{{Key: "rear", Value: int32(31)}, {Key: "front", Value: int64(12)}}
	reversed := bson.D{shape[1], shape[0]}
	duplicate := append(slices.Clone(shape), shape[0])
	nested := func(v any) bson.A { return bson.A{bson.A{v}} }
	nan := math.Float64frombits(0x7ff8000000000011)
	decimalNaN := primitive.NewDecimal128(0x7c00000000000000, 1)
	padding := strings.Repeat("p", 1<<20)
	missing := sourceDifference{name: "missing document", before: field("sample")}
	return []sourceDifference{
		change("none", int32(1), int32(1)),
		{"none with NaN payload", field(nan), field(nan)},
		{"top level field order", shape, reversed},
		{"duplicate top level name", shape, duplicate},
		change("embedded field order", shape, reversed),
		change("duplicate nested name", shape, duplicate),
		change("duplicate name in array document", nested(shape), nested(duplicate)),
		change("field order inside nested arrays", nested(shape), nested(reversed)),
		change("int32 to equal int64", int32(1), int64(1)),
		change("int32 to equal double", int32(1), float64(1)),
		change("int32 to equal decimal", int32(1), decimal),
		change("decimal encoding for equal value", decimal, otherScale),
		change("double signed zero", float64(0), math.Copysign(0, -1)),
		{"double NaN payload", field(nan), field(math.Float64frombits(0x7ff8000000000012))},
		{"double NaN sign", field(nan), field(math.Float64frombits(0xfff8000000000011))},
		change("opposite infinities", math.Inf(1), math.Inf(-1)),
		{"decimal NaN payload", field(decimalNaN), field(primitive.NewDecimal128(0x7c00000000000000, 2))},
		change("decimal signed zero", primitive.NewDecimal128(0x3040000000000000, 0), primitive.NewDecimal128(0xb040000000000000, 0)),
		change("decimal opposite infinities", primitive.NewDecimal128(0x7800000000000000, 0), primitive.NewDecimal128(0xf800000000000000, 0)),
		change("regex option order", primitive.Regex{Pattern: "sample", Options: "im"}, bson.RawValue{Type: bson.TypeRegex, Value: []byte("sample\x00mi\x00")}),
		change("non UTF-8 string", "\uFFFD", string([]byte{0xff})),
		change("Unicode normalization", "café", "cafe\u0301"),
		change("embedded NUL", "a\x00b", "ab"),
		change("binary subtype", primitive.Binary{Data: []byte{1, 2, 3}}, primitive.Binary{Subtype: 0x80, Data: []byte{1, 2, 3}}),
		change("binary payload", primitive.Binary{Data: []byte{1, 2, 3}}, primitive.Binary{Data: []byte{1, 2, 4}}),
		change("date to timestamp", primitive.DateTime(1000), primitive.Timestamp{T: 1}),
		change("null to empty document", nil, bson.D{}),
		change("null to empty array", nil, bson.A{}),
		{"null to missing", field(nil), bson.D{}},
		change("array order", bson.A{2, 4, 4}, bson.A{4, 2, 4}),
		change("array duplicate count", bson.A{2, 4, 4}, bson.A{2, 4}),
		change("last byte in one MiB value", padding, padding[:len(padding)-1]+"q"),
		{"extra field", shape, append(slices.Clone(shape), bson.E{Key: "extra", Value: nil})},
		missing,
	}
}

func sourceCaseImage(t *testing.T, id string, fields bson.D) bson.Raw {
	t.Helper()
	if fields == nil {
		return nil
	}
	raw, err := bson.Marshal(append(bson.D{{Key: "_id", Value: id}}, fields...))
	require.NoError(t, err)
	return raw
}

type sourceEvidenceCell struct {
	id, operation string
	sourceDifference
}

const (
	verifiedEvidence   = "verified format 3"
	unverifiedBefore   = "before marked unverified"
	unverifiedAfter    = "after marked unverified"
	unmarkedFormat3    = "format 3 without provenance"
	previousFormat2    = "42efadc format 2"
	releaseFormat2     = "2.7.0 format 2"
	reclaimedAfter     = "history reclaimed after preview"
	reclaimedBefore    = "history reclaimed before preview"
	written            = "written"
	refusedBeforeClaim = "refused before claim"
	aborted            = "aborted in the transaction"
	zeroAfterTime      = "top-level zero timestamp"
	nestedAfterTime    = "nested zero timestamp"
	rewrittenAfterTime = "timestamp transformed in transaction"
)

// The oracle uses only the scenario inputs. History reclaimed after saving a
// verified plan supplies the same evidence as a newly saved verified plan.
func (c sourceEvidenceCell) outcome(t *testing.T, provenance string) string {
	t.Helper()
	switch provenance {
	case unverifiedBefore, unverifiedAfter, reclaimedBefore, previousFormat2, releaseFormat2, unmarkedFormat3:
		return refusedBeforeClaim
	case verifiedEvidence, reclaimedAfter:
	default:
		t.Fatalf("missing expectation for %q", provenance)
	}
	switch c.name {
	case zeroAfterTime:
		return refusedBeforeClaim
	case "none", "none with NaN payload", nestedAfterTime:
		return written
	}
	return aborted
}

func exactSourceCells(t *testing.T) []sourceEvidenceCell {
	changes := existingSourceDifferences(t)
	var cells []sourceEvidenceCell
	for _, operation := range []string{"replace", "delete", "insert"} {
		variants := changes
		if operation == "insert" {
			variants = []sourceDifference{
				{name: "none"},
				{name: "document present", live: bson.D{{Key: "v", Value: true}}},
			}
		}
		if operation != "delete" {
			var original bson.D
			if operation == "replace" {
				original = bson.D{{Key: "v", Value: int32(1)}}
			}
			variants = slices.Clone(variants)
			for _, name := range []string{zeroAfterTime, nestedAfterTime, rewrittenAfterTime} {
				variants = append(variants, sourceDifference{name, original, original})
			}
		}
		for i, variant := range variants {
			cells = append(cells, sourceEvidenceCell{fmt.Sprintf("%s-%02d", operation, i), operation, variant})
		}
	}
	return cells
}

func seedEvidenceBranch(t *testing.T, f *followFixture, cells []sourceEvidenceCell) (mainID, childID string) {
	t.Helper()
	ctx := t.Context()
	_, err := f.source.Collection("catalog").InsertOne(ctx, bson.M{"_id": "stock-anchor"})
	require.NoError(t, err)
	var originals []any
	for _, cell := range cells {
		if before := sourceCaseImage(t, cell.id, cell.before); before != nil {
			originals = append(originals, before)
		}
	}
	_, err = f.source.Collection("samples").InsertMany(ctx, originals)
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	child, err := f.services.Branches.CreateBranch(main.ProjectID, "exact-source-writes", main.ID)
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, child.Name)
	require.NoError(t, err)
	_, err = writer.Put(ctx, "catalog", bson.M{"_id": "parcel", "status": "sent"})
	require.NoError(t, err)
	var puts []bson.M
	for _, cell := range cells {
		if cell.operation == "delete" {
			_, deleted, err := writer.Delete(ctx, "samples", cell.id)
			require.NoError(t, err)
			require.True(t, deleted)
		} else {
			doc := bson.M{"_id": cell.id, "action": "changed"}
			switch cell.name {
			case zeroAfterTime:
				doc["eventTime"] = primitive.Timestamp{}
			case nestedAfterTime:
				doc["event"] = bson.M{"time": primitive.Timestamp{}}
			case rewrittenAfterTime:
				doc["eventTime"] = primitive.Timestamp{T: 42}
			}
			puts = append(puts, doc)
		}
	}
	_, err = writer.PutMany(ctx, "samples", puts)
	require.NoError(t, err)
	return main.ID, child.ID
}

// Let a zero timestamp reach MongoDB after admission without exposing a
// bypass in the production writer. Only this source connection uses the hook.
func zeroTimestampOnWrite(t *testing.T, db *mongo.Database) *mongo.Database {
	t.Helper()
	reg := bson.NewRegistry()
	rawType := reflect.TypeFor[bson.Raw]()
	encoder, err := reg.LookupEncoder(rawType)
	require.NoError(t, err)
	reg.RegisterTypeEncoder(rawType, bsoncodec.ValueEncoderFunc(func(ec bsoncodec.EncodeContext, w bsonrw.ValueWriter, v reflect.Value) error {
		raw := v.Interface().(bson.Raw)
		if timestamp := raw.Lookup("eventTime"); timestamp.Type == bson.TypeTimestamp {
			clear(timestamp.Value)
		}
		return encoder.EncodeValue(ec, w, v)
	}))
	return db.Client().Database(db.Name(), options.Database().SetRegistry(reg))
}

func markEvidenceCapture(t *testing.T, f *followFixture, branchID string, unverified bool) {
	t.Helper()
	filter := bson.M{"branch_id": branchID, "collection": "samples"}
	update := bson.M{"$unset": bson.M{"metadata.capture_images": ""}}
	if unverified {
		update = bson.M{"$set": bson.M{"metadata.capture_images": "attached_unverified"}}
	}
	rows := f.services.Client.Database(f.metadataName).Collection("wal_log")
	result, err := rows.UpdateMany(t.Context(), filter, update)
	require.NoError(t, err)
	require.Positive(t, result.ModifiedCount)
}

func reclaimEvidenceImages(t *testing.T, f *followFixture, branches ...string) {
	t.Helper()
	// Take all snapshots before deleting any underlying data. Keep the
	// companion insert in catalog intact throughout every scenario.
	for _, id := range branches {
		branch, err := f.services.Branches.GetBranchByID(id)
		require.NoError(t, err)
		_, err = f.services.Snapshots.CreateSnapshot(t.Context(), id, branch.HeadLSN)
		require.NoError(t, err)
	}
	for _, id := range branches {
		branch, err := f.services.Branches.GetBranchByID(id)
		require.NoError(t, err)
		require.NoError(t, f.services.WAL.RecordReclaimFloor(t.Context(), id, "samples", branch.HeadLSN))
		removed, err := f.services.WAL.DeleteDataEntriesUpTo(id, "samples", branch.HeadLSN)
		require.NoError(t, err)
		require.Positive(t, removed)
	}
}

func legacyEvidenceGuards(t *testing.T) map[string]merge.Change {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "42efadc", "nested.json"))
	require.NoError(t, err)
	var previous bson.Raw
	require.NoError(t, bson.UnmarshalExtJSON(data, true, &previous))
	guards := make(map[string]merge.Change)
	for name, data := range map[string]bson.Raw{previousFormat2: previous, releaseFormat2: v270Fixture(t, "followed")} {
		var p merge.Plan
		require.NoError(t, bson.Unmarshal(data, &p))
		require.Equal(t, 2, p.FormatVersion)
		require.Len(t, p.Changes, 1)
		guards[name] = p.Changes[0]
		require.Equal(t, "parcel", guards[name].DocumentID)
	}
	return guards
}

func readSourceCase(t *testing.T, collection *mongo.Collection, id string) bson.Raw {
	t.Helper()
	raw, err := collection.FindOne(t.Context(), bson.M{"_id": id}).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	require.NoError(t, err)
	return raw
}

func saveEvidenceCell(t *testing.T, storedPlans *mongo.Collection, template *merge.Plan, guard, proposal merge.Change, cell sourceEvidenceCell, provenance string, guards map[string]merge.Change) *merge.Plan {
	t.Helper()
	require.Equal(t, cell.id, proposal.DocumentID)
	require.Equal(t, cell.operation == "delete", proposal.Delete)
	if provenance == unverifiedBefore || (provenance == reclaimedBefore && cell.operation != "insert") {
		require.Contains(t, proposal.SortedImages, "before")
	} else {
		require.True(t, bytes.Equal(sourceCaseImage(t, cell.id, cell.before), proposal.BeforeRaw), "stored original bytes")
	}
	if provenance == unverifiedAfter || (provenance == reclaimedBefore && cell.operation != "delete") {
		require.Contains(t, proposal.SortedImages, "document")
	}
	require.Empty(t, guard.SortedImages)
	p := *template
	p.ID = primitive.NewObjectID()
	if provenance == unmarkedFormat3 {
		p.ProvenanceVersion = 0
	}
	if historicalGuard, ok := guards[provenance]; ok {
		// The first change is the insert saved by that binary. Use its
		// format and the same map-based encoding for the selected row.
		p.FormatVersion, p.ProvenanceVersion = 2, 0
		guard = historicalGuard
		var err error
		if proposal.Before != nil {
			proposal.BeforeRaw, err = bson.Marshal(proposal.Before)
			require.NoError(t, err)
		}
		if proposal.Document != nil {
			proposal.DocumentRaw, err = bson.Marshal(proposal.Document)
			require.NoError(t, err)
		}
	}
	p.Changes = []merge.Change{guard, proposal}
	_, err := storedPlans.InsertOne(t.Context(), &p)
	require.NoError(t, err)
	return &p
}

func TestWriteBack_SourceEvidenceTable(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := t.Context()
	f := newFollowFixture(t, reproSourceURI(t))
	cells := exactSourceCells(t)
	mainID, childID := seedEvidenceBranch(t, f, cells)
	verified := f.preview(t, childID)
	require.Len(t, verified.Changes, 1+len(cells))
	require.Equal(t, "parcel", verified.Changes[0].DocumentID)
	guards := legacyEvidenceGuards(t)

	// Count claim attempts even if the writer later removes its claim. On
	// the source, observe the earlier, valid insert inside the transaction.
	var claims, guardInserts, aborts, writes, finalReads atomic.Int64
	meta := electionClient(t, metadataDeploymentURI, &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if e.DatabaseName == f.metadataName && e.CommandName == "update" && e.Command.Lookup("autocommit").Type != 0 &&
				e.Command.Lookup("update").StringValue() == "wal_merge_plans" {
				claims.Add(1)
			}
		},
	}).Database(f.metadataName)
	src := electionClient(t, f.sourceURI, &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if e.CommandName == "abortTransaction" {
				aborts.Add(1)
			}
			if e.DatabaseName == f.source.Name() && e.CommandName == "insert" && e.Command.Lookup("autocommit").Type != 0 &&
				e.Command.Lookup("insert").StringValue() == "catalog" {
				guardInserts.Add(1)
			}
			if e.DatabaseName == f.source.Name() && e.Command.Lookup("autocommit").Type != 0 {
				switch e.CommandName {
				case "insert", "update", "delete":
					writes.Add(1)
				case "find":
					if writes.Load() == 2 {
						finalReads.Add(1)
					}
				}
			}
		},
	}).Database(f.source.Name())
	writer := merge.NewService(meta, f.services.WAL, f.services.Branches, f.services.Materializer, meta.Client())
	transformed := zeroTimestampOnWrite(t, src)
	storedPlans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
	catalog, samples := f.source.Collection("catalog"), f.source.Collection("samples")
	count := map[string]int{}
	for _, provenance := range []string{
		verifiedEvidence, unverifiedBefore, unverifiedAfter, unmarkedFormat3,
		previousFormat2, releaseFormat2, reclaimedAfter, reclaimedBefore,
	} {
		template := verified
		switch provenance {
		case unverifiedBefore, unverifiedAfter:
			branch := mainID
			if provenance == unverifiedAfter {
				branch = childID
			}
			markEvidenceCapture(t, f, branch, true)
			template = f.preview(t, childID)
			markEvidenceCapture(t, f, branch, false)
		case reclaimedBefore:
			template = f.preview(t, childID)
		}
		require.Len(t, template.Changes, 1+len(cells))
		proposals := make(map[string]merge.Change)
		for _, change := range template.Changes {
			proposals[change.DocumentID] = change
		}
		plans := make(map[string]*merge.Plan)
		for _, cell := range cells {
			// An insert has no before-image, a delete has no after-image.
			if provenance == unverifiedBefore && cell.operation == "insert" || provenance == unverifiedAfter && cell.operation == "delete" {
				continue
			}
			plans[cell.id] = saveEvidenceCell(t, storedPlans, template, proposals["parcel"], proposals[cell.id], cell, provenance, guards)
		}
		if provenance == reclaimedAfter {
			reclaimEvidenceImages(t, f, mainID, childID)
		}
		for _, cell := range cells {
			p := plans[cell.id]
			if p == nil {
				continue
			}
			t.Run(provenance+"/"+cell.operation+"/"+cell.name, func(t *testing.T) {
				guard, proposal := p.Changes[0], p.Changes[1]
				for _, row := range []struct {
					collection *mongo.Collection
					key        string
				}{{catalog, "parcel"}, {samples, cell.id}} {
					_, err := row.collection.DeleteOne(ctx, bson.M{"_id": row.key})
					require.NoError(t, err)
				}
				live := sourceCaseImage(t, cell.id, cell.live)
				if live != nil {
					// MongoDB 7 stores every variant in this table, including
					// duplicate fields, unsorted regex options and invalid UTF-8.
					_, err := samples.InsertOne(ctx, live)
					require.NoError(t, err, "source storage for %s", cell.name)
				}
				require.True(t, bytes.Equal(live, readSourceCase(t, samples, cell.id)), "server must retain the supplied bytes")
				// Check refusals first so every digest case starts with the same pending plan and live BSON.
				for _, approval := range []string{"missing", "wrong", "correct"} {
					t.Run(approval, func(t *testing.T) {
						digest := wrongApproval
						if approval == "missing" {
							digest = ""
						} else if approval == "correct" && p.ProvenanceVersion != 0 {
							digest = approvalFor(t, p)
						}
						claims.Store(0)
						guardInserts.Store(0)
						aborts.Store(0)
						writes.Store(0)
						finalReads.Store(0)
						target := merge.WriteBackTarget{Database: src}
						if cell.name == rewrittenAfterTime {
							target.Database = transformed
						}
						result, applyErr := writer.ApplyToSource(ctx, p.ID, "", target, digest)
						state := f.plan(t, p)
						outcome := refusedBeforeClaim
						if approval == "correct" {
							outcome = cell.outcome(t, provenance)
						}
						switch outcome {
						case written:
							require.NoError(t, applyErr)
							require.Equal(t, 2, result.Applied)
							require.Equal(t, merge.StatusApplied, state.Status)
							require.EqualValues(t, 1, claims.Load())
							require.EqualValues(t, 1, guardInserts.Load())
							require.Zero(t, aborts.Load())
							require.EqualValues(t, 2, finalReads.Load(), "read each result after all writes")
							require.Equal(t, guard.DocumentRaw, readSourceCase(t, catalog, "parcel"))
							live = proposal.DocumentRaw // nil after a successful delete
						case refusedBeforeClaim:
							if p.ProvenanceVersion == 0 {
								require.ErrorContains(t, applyErr, "preview again with this version")
							} else if approval == "correct" {
								require.ErrorContains(t, applyErr, "samples/"+cell.id)
								if cell.name == zeroAfterTime && (provenance == verifiedEvidence || provenance == reclaimedAfter) {
									require.ErrorContains(t, applyErr, "current time on write")
								}
							} else {
								require.ErrorContains(t, applyErr, "--output json")
							}
							require.Nil(t, result)
							require.Equal(t, merge.StatusPending, state.Status)
							require.Nil(t, state.WriteBack)
							require.Empty(t, state.WriteBackLastError)
							require.Zero(t, claims.Load())
							require.Zero(t, guardInserts.Load())
							require.Zero(t, aborts.Load())
							require.Nil(t, readSourceCase(t, catalog, "parcel"))
						case aborted:
							if cell.name == rewrittenAfterTime {
								var mismatch *merge.StoredImageError
								require.ErrorAs(t, applyErr, &mismatch)
								require.Equal(t, "samples", mismatch.Collection)
								require.Equal(t, cell.id, mismatch.DocumentID)
								require.Contains(t, state.WriteBackLastError, "the server stored something other than the approved image")
								require.Equal(t, digest, approvalFor(t, state))
								require.Nil(t, state.AppliedAt)
								require.EqualValues(t, 2, finalReads.Load())
							} else {
								var conflict *merge.ConflictError
								require.ErrorAs(t, applyErr, &conflict)
								require.Equal(t, cell.id, conflict.DocumentID)
							}
							require.Nil(t, result)
							require.Equal(t, merge.StatusPending, state.Status)
							require.Nil(t, state.WriteBack)
							require.NotEmpty(t, state.WriteBackLastError)
							require.EqualValues(t, 1, claims.Load())
							require.EqualValues(t, 1, guardInserts.Load())
							require.EqualValues(t, 1, aborts.Load())
							require.Nil(t, readSourceCase(t, catalog, "parcel"), "transaction rolled back its earlier insert")
						default:
							t.Fatalf("unhandled outcome %q", outcome)
						}
						require.True(t, bytes.Equal(live, readSourceCase(t, samples, cell.id)), "source bytes after apply")
						count[outcome]++
						if outcome == aborted && cell.name == rewrittenAfterTime {
							result, err := writer.ApplyToSource(ctx, p.ID, "", merge.WriteBackTarget{Database: src}, digest)
							require.NoError(t, err, "the released plan accepts the same approval")
							require.Equal(t, 2, result.Applied)
							require.Equal(t, proposal.DocumentRaw, readSourceCase(t, samples, cell.id))
						}
					})
				}
			})
		}
	}
	t.Logf("%d written, %d refused before claim, %d aborted in the transaction, 0 not storable", count[written], count[refusedBeforeClaim], count[aborted])
}
