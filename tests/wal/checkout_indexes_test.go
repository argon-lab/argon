package wal_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/indexspec"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// seedIndexedSource writes documents and five kinds of secondary index.
func seedIndexedSource(t *testing.T, f *followFixture) {
	t.Helper()
	ctx := t.Context()
	orders := f.source.Collection("orders")
	docs := make([]interface{}, 20)
	for i := range docs {
		docs[i] = bson.D{{Key: "_id", Value: i}, {Key: "sku", Value: fmt.Sprintf("sku-%02d", i)}, {Key: "cat", Value: i % 3},
			{Key: "price", Value: i * 10}, {Key: "qty", Value: i % 4}, {Key: "name", Value: fmt.Sprintf("item %d", i)},
			{Key: "createdAt", Value: time.Now()}}
	}
	_, err := orders.InsertMany(ctx, docs)
	require.NoError(t, err)
	_, err = orders.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "sku", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "cat", Value: 1}, {Key: "price", Value: -1}}},
		{Keys: bson.D{{Key: "qty", Value: 1}}, Options: options.Index().SetPartialFilterExpression(bson.M{"qty": bson.M{"$gt": 0}})},
		{Keys: bson.D{{Key: "createdAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(3600)},
		{Keys: bson.D{{Key: "name", Value: "text"}}},
	})
	require.NoError(t, err)
}

// physicalIndexes lists a checked-out collection's indexes by name.
func physicalIndexes(t *testing.T, services *walcli.Services, physicalDB, collection string) map[string]bson.M {
	t.Helper()
	cursor, err := services.Client.Database(physicalDB).Collection(collection).Indexes().List(t.Context())
	require.NoError(t, err)
	var specs []bson.M
	require.NoError(t, cursor.All(t.Context(), &specs))
	byName := make(map[string]bson.M, len(specs))
	for _, spec := range specs {
		byName[spec["name"].(string)] = spec
	}
	return byName
}

func indexNames(byName map[string]bson.M) []string {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// waitRecorded waits until the branch's own record for a collection
// satisfies ok; capture and following record index changes asynchronously.
func waitRecorded(t *testing.T, services *walcli.Services, metadata, branchID, collection string, ok func([]string) bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var r struct {
			Indexes []bson.Raw `bson:"indexes"`
		}
		err := services.Client.Database(metadata).Collection(indexspec.CollectionName).
			FindOne(t.Context(), bson.M{"_id": bson.M{"b": branchID, "c": collection}}).Decode(&r)
		if err == nil {
			names := make([]string, len(r.Indexes))
			for i, spec := range r.Indexes {
				names[i] = indexspec.Name(spec)
			}
			if ok(names) {
				return
			}
		}
		require.True(t, time.Now().Before(deadline), "index definitions of %s were not recorded in time", collection)
		time.Sleep(50 * time.Millisecond)
	}
}

func has(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func TestCheckoutIndexes_ImportedDefinitionsAreBuilt(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := t.Context()
		f := newFollowFixture(t, sourceURI)
		seedIndexedSource(t, f)
		f.importOnline(t, walcli.OnlineImport{})
		main := f.main(t)

		info, _, err := f.services.CreateSandbox(ctx, main.ProjectID, main.ID, "agent", time.Hour, false)
		require.NoError(t, err)
		require.Equal(t, 5, info.Indexes)
		require.Equal(t, []string{"index createdAt_1 on orders was built without its TTL expiry"}, info.IndexNotes)

		built := physicalIndexes(t, f.services, info.PhysicalDB, "orders")
		require.Equal(t, []string{"_id_", "cat_1_price_-1", "createdAt_1", "name_text", "qty_1", "sku_1"}, indexNames(built))
		require.Equal(t, true, built["sku_1"]["unique"])
		require.Equal(t, bson.M{"qty": bson.M{"$gt": int32(0)}}, built["qty_1"]["partialFilterExpression"])
		require.NotContains(t, built["createdAt_1"], "expireAfterSeconds")

		// The unique constraint now holds in the sandbox as on the source.
		_, err = f.services.Client.Database(info.PhysicalDB).Collection("orders").InsertOne(ctx, bson.M{"_id": 100, "sku": "sku-03"})
		require.True(t, mongo.IsDuplicateKeyError(err), "duplicate sku: %v", err)
	})
}

func TestCheckoutIndexes_CaptureCarriesIndexChangesToChildren(t *testing.T) {
	ctx := t.Context()
	f := newFollowFixture(t, metadataDeploymentURI)
	seedIndexedSource(t, f)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)

	parent, _, err := f.services.CreateSandbox(ctx, main.ProjectID, main.ID, "parent", time.Hour, false)
	require.NoError(t, err)
	require.NoError(t, f.services.StartCapture(ctx, parent.BranchID, "agent"))
	orders := f.services.Client.Database(parent.PhysicalDB).Collection("orders")
	_, err = orders.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "region", Value: 1}}})
	require.NoError(t, err)
	_, err = orders.Indexes().DropOne(ctx, "cat_1_price_-1")
	require.NoError(t, err)
	waitRecorded(t, f.services, f.metadataName, parent.BranchID, "orders", func(names []string) bool {
		return has(names, "region_1") && !has(names, "cat_1_price_-1")
	})

	child, _, err := f.services.CreateSandbox(ctx, main.ProjectID, parent.BranchID, "child", time.Hour, false)
	require.NoError(t, err)
	require.Equal(t, []string{"_id_", "createdAt_1", "name_text", "qty_1", "region_1", "sku_1"}, indexNames(physicalIndexes(t, f.services, child.PhysicalDB, "orders")))

	sibling, _, err := f.services.CreateSandbox(ctx, main.ProjectID, main.ID, "sibling", time.Hour, false)
	require.NoError(t, err)
	require.Equal(t, []string{"_id_", "cat_1_price_-1", "createdAt_1", "name_text", "qty_1", "sku_1"}, indexNames(physicalIndexes(t, f.services, sibling.PhysicalDB, "orders")),
		"a sandbox's index changes stay on its branch")
}

func TestCheckoutIndexes_UnbuildableIndexIsReported(t *testing.T) {
	ctx := t.Context()
	f := newFollowFixture(t, metadataDeploymentURI)
	seedIndexedSource(t, f)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)

	// A branch written programmatically can hold data the source's unique
	// index would refuse.
	branch, err := f.services.Branches.CreateBranch(main.ProjectID, "dupes", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.services.WAL, f.services.Branches, f.services.Materializer, branch)
	_, err = writer.Put(ctx, "orders", bson.M{"_id": 200, "sku": "sku-05"})
	require.NoError(t, err)

	info, err := f.services.Checkout.Checkout(ctx, branch.ID)
	require.NoError(t, err)
	require.Equal(t, 4, info.Indexes)
	require.Len(t, info.IndexNotes, 2)
	var notBuilt string
	for _, note := range info.IndexNotes {
		if strings.Contains(note, "was not built") {
			notBuilt = note
		}
	}
	require.True(t, strings.HasPrefix(notBuilt, "index sku_1 on orders was not built: "), notBuilt)
	require.Contains(t, notBuilt, "E11000")
	require.NotContains(t, physicalIndexes(t, f.services, info.PhysicalDB, "orders"), "sku_1")
}

func TestSourceIndexes_FollowRecordsSourceIndexChanges(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := t.Context()
		f := newFollowFixture(t, sourceURI)
		seedIndexedSource(t, f)
		f.importOnline(t, walcli.OnlineImport{})
		main := f.main(t)
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})

		orders := f.source.Collection("orders")
		_, err := orders.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "status", Value: 1}}})
		require.NoError(t, err)
		_, err = orders.Indexes().DropOne(ctx, "sku_1")
		require.NoError(t, err)
		waitRecorded(t, f.services, f.metadataName, main.ID, "orders", func(names []string) bool {
			return has(names, "status_1") && !has(names, "sku_1")
		})
		require.NoError(t, stop())

		info, _, err := f.services.CreateSandbox(context.Background(), main.ProjectID, main.ID, "after", time.Hour, false)
		require.NoError(t, err)
		require.Equal(t, []string{"_id_", "cat_1_price_-1", "createdAt_1", "name_text", "qty_1", "status_1"}, indexNames(physicalIndexes(t, f.services, info.PhysicalDB, "orders")))
	})
}

func TestCheckoutIndexes_QuiescedImportRecordsDefinitions(t *testing.T) {
	ctx := t.Context()
	f := newFollowFixture(t, metadataDeploymentURI)
	seedIndexedSource(t, f)
	_, err := f.services.ImportDatabase(ctx, f.sourceURI, f.source.Name(), f.project, false, 1000, true)
	require.NoError(t, err)
	main := f.main(t)
	info, _, err := f.services.CreateSandbox(ctx, main.ProjectID, main.ID, "agent", time.Hour, false)
	require.NoError(t, err)
	require.Equal(t, []string{"_id_", "cat_1_price_-1", "createdAt_1", "name_text", "qty_1", "sku_1"}, indexNames(physicalIndexes(t, f.services, info.PhysicalDB, "orders")))
}
