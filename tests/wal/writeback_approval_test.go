package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const wrongApproval = "0000000000000000000000000000000000000000000000000000000000000000"

func approvalFor(t *testing.T, p *merge.Plan) string {
	t.Helper()
	digest, err := merge.ApprovalDigest(p)
	require.NoError(t, err)
	require.Len(t, digest, 64)
	return digest
}

func approvalFixture(t *testing.T) (*followFixture, string) {
	t.Helper()
	f, branch := planFixture(t)
	ctx := t.Context()
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, err = writer.PutMany(ctx, "catalog", []bson.M{{"_id": "extra", "stock": int32(2)}, {"_id": "item-01", "stock": int32(20)}})
	require.NoError(t, err)
	return f, branch
}

func TestWriteBack_ApprovalBindsSavedContents(t *testing.T) {
	for _, change := range []string{"missing", "wrong", "another plan", "after byte", "before byte", "operation", "remove", "add", "clear provenance", "target", "empty replacement", "changed before claim"} {
		t.Run(change, func(t *testing.T) {
			f, branch := approvalFixture(t)
			ctx := t.Context()
			plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
			catalog := f.source.Collection("catalog")
			readCatalog := func() []bson.Raw {
				cursor, err := catalog.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
				require.NoError(t, err)
				var docs []bson.Raw
				require.NoError(t, cursor.All(ctx, &docs))
				return docs
			}
			p := f.preview(t, branch)
			require.Len(t, p.Changes, 2)
			require.Equal(t, "extra", p.Changes[0].DocumentID)
			if change == "clear provenance" {
				_, err := plans.UpdateByID(ctx, p.ID, bson.M{"$set": bson.M{"changes.0.sorted_images": bson.A{"document"}}})
				require.NoError(t, err)
				p = f.plan(t, p)
			}
			expected := approvalFor(t, p)
			approved := expected
			altered := f.plan(t, p)
			target := merge.WriteBackTarget{Database: f.source}
			switch change {
			case "missing":
				approved = ""
			case "wrong":
				approved = wrongApproval
			case "another plan":
				approved = approvalFor(t, f.preview(t, branch))
			case "after byte":
				altered.Changes[0].DocumentRaw.Lookup("stock").Value[0] ^= 1
			case "before byte":
				altered.Changes[1].BeforeRaw.Lookup("stock").Value[0] ^= 1
			case "operation":
				altered.Changes[0].Delete = true
			case "remove":
				altered.Changes = altered.Changes[1:]
			case "add":
				c := altered.Changes[0]
				c.DocumentID, c.ID = "another", "another"
				raw, err := bson.Marshal(bson.D{{Key: "_id", Value: "another"}})
				require.NoError(t, err)
				c.DocumentRaw = raw
				altered.Changes = append(altered.Changes, c)
			case "clear provenance":
				altered.Changes[0].SortedImages = nil
			case "target":
				altered.TargetBranchID = branch
			case "empty replacement":
				altered.Changes = []merge.Change{}
			}
			_, err := plans.ReplaceOne(ctx, bson.M{"_id": p.ID}, altered)
			require.NoError(t, err)
			readRecord := func() bson.Raw {
				raw, err := plans.FindOne(ctx, bson.M{"_id": p.ID}).Raw()
				require.NoError(t, err)
				return raw
			}
			record := readRecord()
			hookRan := false
			if change == "changed before claim" {
				// Source collection preflight finishes after the early plan read and before its metadata claim.
				src := electionClient(t, f.sourceURI, &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
					if e.CommandName == "listCollections" && !hookRan {
						hookRan = true
						_, err := plans.UpdateByID(ctx, p.ID, bson.M{"$set": bson.M{"changes.0.document.stock": int32(30)}})
						require.NoError(t, err)
						record = readRecord()
					}
				}})
				target.Database = src.Database(f.source.Name())
			}
			before := readCatalog()
			result, err := f.services.Merge.ApplyToSource(ctx, p.ID, "", target, approved)
			require.Equal(t, change == "changed before claim", hookRan)
			require.ErrorContains(t, err, "--output json")
			require.NotContains(t, err.Error(), expected)
			require.NotContains(t, err.Error(), approvalFor(t, f.plan(t, p)))
			require.Nil(t, result)
			require.Equal(t, record, readRecord())
			require.Equal(t, before, readCatalog())
		})
	}
}

func TestWriteBack_ApprovalAppliesOnce(t *testing.T) {
	f, branch := approvalFixture(t)
	ctx := t.Context()
	catalog := f.source.Collection("catalog")
	p := f.preview(t, branch)
	digest := approvalFor(t, p)
	result, err := f.services.WriteBackMerge(ctx, p.ID, "", f.sourceURI, digest)
	require.NoError(t, err)
	require.Equal(t, 2, result.Applied)
	for _, c := range p.Changes {
		require.Equal(t, c.DocumentRaw, readSourceCase(t, catalog, c.DocumentID))
	}
	applied := f.plan(t, p)
	require.Equal(t, merge.StatusApplied, applied.Status)
	require.NotNil(t, applied.WriteBack)
	require.Equal(t, digest, applied.WriteBack.ApprovedDigest)
	require.Equal(t, digest, approvalFor(t, applied))
	_, err = f.services.WriteBackMerge(ctx, p.ID, "", f.sourceURI, digest)
	require.Error(t, err)
}

func TestWriteBack_OlderPlanRequiresNewPreview(t *testing.T) {
	f, branch := planFixture(t)
	ctx := t.Context()
	plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
	// Use the release's encoded images and typed keys. The earlier format-3 writers
	// stored BSON in the same fields with and without exact_bytes_version.
	for _, version := range []string{"release", "ordered", "exact bytes"} {
		for _, shape := range []string{"actionable", "empty", "conflict only"} {
			t.Run(version+"/"+shape, func(t *testing.T) {
				fixture := "followed"
				if shape == "conflict only" {
					fixture = "conflict"
				}
				var raw bson.D
				require.NoError(t, bson.Unmarshal(v270Fixture(t, fixture), &raw))
				template := f.preview(t, branch)
				fields := bson.M{"_id": template.ID, "target_branch_id": template.TargetBranchID,
					"source_branch_id": template.SourceBranchID, "project_id": template.ProjectID,
					"source_head": template.SourceHead, "source_version": template.SourceVersion}
				if shape == "empty" {
					fields["changes"] = bson.A{}
				}
				if version != "release" {
					fields["format_version"] = int32(3)
				}
				if version == "exact bytes" {
					fields["exact_bytes_version"] = int32(1)
				}
				for i, elem := range raw {
					if v, ok := fields[elem.Key]; ok {
						raw[i].Value = v
						delete(fields, elem.Key)
					}
				}
				for k, v := range fields {
					raw = append(raw, bson.E{Key: k, Value: v})
				}
				_, err := plans.ReplaceOne(ctx, bson.M{"_id": template.ID}, raw)
				require.NoError(t, err)
				p := f.plan(t, template)
				switch shape {
				case "actionable":
					require.Len(t, p.Changes, 1)
				case "empty":
					require.Empty(t, p.Changes)
					require.Empty(t, p.Conflicts)
				case "conflict only":
					require.Empty(t, p.Changes)
					require.Len(t, p.Conflicts, 1)
				}
				body, err := p.CLIJSON(true)
				require.NoError(t, err)
				require.NotContains(t, string(body), "approval_digest")
				digest, err := merge.ApprovalDigest(p)
				require.ErrorContains(t, err, "preview again with this version")
				require.Empty(t, digest)
				before := f.sourceState(t, nil)
				result, err := f.services.WriteBackMerge(ctx, p.ID, merge.StrategyOurs, f.sourceURI, wrongApproval)
				require.ErrorContains(t, err, "preview again with this version")
				require.Nil(t, result)
				require.Equal(t, p, f.plan(t, p))
				require.Equal(t, before, f.sourceState(t, nil))
			})
		}
	}
}

func TestWriteBack_ApprovalFencesDeploymentAtClaim(t *testing.T) {
	f, plan := detachPlan(t)
	ctx := t.Context()
	bindings := f.services.Client.Database(f.metadataName).Collection("wal_sources")
	approved := approvalFor(t, plan)
	switched := false
	src := electionClient(t, f.sourceURI, &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if !switched && e.CommandName == "listCollections" {
			switched = true
			_, err := bindings.UpdateByID(ctx, plan.TargetBranchID, bson.M{"$set": bson.M{"replica_set_id": "other-deployment"}})
			require.NoError(t, err)
		}
	}})
	result, err := f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: src.Database(f.source.Name())}, approved)
	require.True(t, switched)
	require.ErrorContains(t, err, "binding changed")
	require.Nil(t, result)
	// With the binding already switched, preflight must refuse before connecting to a supplied URI.
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", "invalid-source-uri", approved)
	require.ErrorContains(t, err, "binding has changed")
	stored := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, stored.Status)
	require.Nil(t, stored.WriteBack)
	require.Nil(t, readSourceCase(t, f.source.Collection("catalog"), "claim-test"))
}
