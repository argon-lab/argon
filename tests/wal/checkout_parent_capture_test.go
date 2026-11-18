package wal_test

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// resumeTokenClusterTime reads the cluster time a change-stream resume token
// stands at. MongoDB documents tokens as opaque; since 4.0.7 their hex form
// has begun with the type byte 0x82 followed by the timestamp, which is all
// this test relies on.
func resumeTokenClusterTime(t *testing.T, token bson.Raw) primitive.Timestamp {
	t.Helper()
	data, ok := token.Lookup("_data").StringValueOK()
	require.True(t, ok, "resume token has a string _data")
	raw, err := hex.DecodeString(data[:18])
	require.NoError(t, err)
	require.Equal(t, byte(0x82), raw[0], "resume token starts with a timestamp")
	return primitive.Timestamp{T: binary.BigEndian.Uint32(raw[1:5]), I: binary.BigEndian.Uint32(raw[5:9])}
}

// A checkout writes the branch's whole state through the oplog. The first
// diff or merge preview of the new branch drains the parent's capture, and a
// parent position from before the copy would make MongoDB scan all of it.
// Checkout therefore leaves a live parent's position after its own copy.
func TestCheckoutLeavesLiveParentCapturePastTheCopy(t *testing.T) {
	ctx := context.Background()
	id := fmt.Sprintf("%d_%d", time.Now().UnixNano(), testDatabaseSeq.Add(1))
	metadataName := "argon_parent_capture_meta_" + id
	services, err := walcli.NewServicesAt(metadataDeploymentURI, metadataName)
	require.NoError(t, err)
	services.SynchronousSnapshots()
	metadata := services.Client.Database(metadataName)
	state := metadata.Collection("wal_ingest_state")
	var branchIDs []string
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, status := range services.Ingest.Statuses() {
			_ = services.Ingest.Cancel(cleanup, status.BranchID)
		}
		for _, branchID := range branchIDs {
			_ = services.Client.Database(checkout.PhysicalDBName(branchID)).Drop(cleanup)
		}
		_ = metadata.Drop(cleanup)
		_ = services.Client.Disconnect(cleanup)
	})

	position := func(branchID string) (token bson.Raw, startAt *primitive.Timestamp, found bool) {
		var doc struct {
			ResumeToken bson.Raw             `bson:"resume_token"`
			StartAt     *primitive.Timestamp `bson:"start_at"`
		}
		err := state.FindOne(ctx, bson.M{"_id": branchID}).Decode(&doc)
		if err != nil {
			return nil, nil, false
		}
		return doc.ResumeToken, doc.StartAt, true
	}

	project, err := services.Projects.CreateProject("parent-capture-" + id)
	require.NoError(t, err)
	main, err := services.Branches.GetBranchByID(project.MainBranchID)
	require.NoError(t, err)
	branchIDs = append(branchIDs, main.ID)

	t.Run("a parent that is not checked out is left alone", func(t *testing.T) {
		child, err := services.Branches.CreateBranch(project.ID, "before-main-is-live", main.ID)
		require.NoError(t, err)
		branchIDs = append(branchIDs, child.ID)
		_, err = services.Checkout.Checkout(ctx, child.ID)
		require.NoError(t, err)
		_, _, found := position(main.ID)
		require.False(t, found, "no capture state is created for a parent with no physical database")
	})

	mainInfo, err := services.Checkout.Checkout(ctx, main.ID)
	require.NoError(t, err)
	mainDB := services.Client.Database(mainInfo.PhysicalDB)
	_, err = mainDB.Collection("items").InsertOne(ctx, bson.M{"_id": "seed", "n": int32(1)})
	require.NoError(t, err)
	// What `argon branches create` does before it forks: drain the parent.
	require.NoError(t, services.SyncBranch(ctx, main.ID))
	before, _, found := position(main.ID)
	require.True(t, found)
	require.NotEmpty(t, before, "draining the parent stores its stream position")

	main, err = services.Branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	child, err := services.Branches.CreateBranch(project.ID, "work", main.ID)
	require.NoError(t, err)
	branchIDs = append(branchIDs, child.ID)
	info, err := services.Checkout.Checkout(ctx, child.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, info.Documents)

	_, boundary, found := position(child.ID)
	require.True(t, found)
	require.NotNil(t, boundary, "checkout stores the new branch's capture boundary after its copy")
	after, _, _ := position(main.ID)
	parentAt := resumeTokenClusterTime(t, after)
	require.False(t, parentAt.Before(*boundary),
		"the parent's stream position (%v) is not behind the end of the child's copy (%v)", parentAt, *boundary)
	require.True(t, resumeTokenClusterTime(t, before).Before(*boundary),
		"the position stored before the checkout was behind the copy, so the assertion above is not vacuous")

	// The parent's own writes are still captured exactly once through all of this.
	_, err = mainDB.Collection("items").UpdateOne(ctx, bson.M{"_id": "seed"}, bson.M{"$set": bson.M{"n": int32(2)}})
	require.NoError(t, err)
	require.NoError(t, services.SyncBranch(ctx, child.ID))
	entries, err := metadata.Collection("wal_log").CountDocuments(ctx, bson.M{"branch_id": main.ID, "collection": "items", "document_id": "seed"})
	require.NoError(t, err)
	require.EqualValues(t, 2, entries, "one entry for the insert and one for the update")
}
