package wal_test

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
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

func checkoutParentLiveWrites(t *testing.T, secondWatcher bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id := fmt.Sprintf("%d_%d", time.Now().UnixNano(), testDatabaseSeq.Add(1))
	metadataName := "argon_parent_live_meta_" + id
	services, err := walcli.NewServicesAt(metadataDeploymentURI, metadataName)
	require.NoError(t, err)
	services.SynchronousSnapshots()
	metadata := services.Client.Database(metadataName)
	state := metadata.Collection("wal_ingest_state")
	var branchIDs []string
	var others []*walcli.Services
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, s := range append([]*walcli.Services{services}, others...) {
			for _, status := range s.Ingest.Statuses() {
				_ = s.Ingest.Cancel(cleanup, status.BranchID)
			}
		}
		for _, branchID := range branchIDs {
			_ = services.Client.Database(checkout.PhysicalDBName(branchID)).Drop(cleanup)
		}
		_ = metadata.Drop(cleanup)
		for _, s := range others {
			_ = s.Client.Disconnect(cleanup)
		}
		_ = services.Client.Disconnect(cleanup)
	})

	project, err := services.Projects.CreateProject("parent-live-" + id)
	require.NoError(t, err)
	main, err := services.Branches.GetBranchByID(project.MainBranchID)
	require.NoError(t, err)
	branchIDs = append(branchIDs, main.ID)
	mainInfo, err := services.Checkout.Checkout(ctx, main.ID)
	require.NoError(t, err)
	mainDB := services.Client.Database(mainInfo.PhysicalDB)
	require.NoError(t, services.PrepareCollection(ctx, main.ID, "live"))
	require.NoError(t, services.PrepareCollection(ctx, main.ID, "bulk"))

	// Enough to copy that the child's load takes a while.
	const bulkDocs = 20000
	pad := strings.Repeat("x", 700)
	for start := 0; start < bulkDocs; start += 1000 {
		docs := make([]interface{}, 0, 1000)
		for i := start; i < start+1000; i++ {
			docs = append(docs, bson.M{"_id": fmt.Sprintf("b-%06d", i), "pad": pad})
		}
		_, err := mainDB.Collection("bulk").InsertMany(ctx, docs)
		require.NoError(t, err)
	}
	_, err = mainDB.Collection("live").InsertOne(ctx, bson.M{"_id": "counter", "n": int32(0)})
	require.NoError(t, err)
	require.NoError(t, services.Ingest.Drain(ctx, main.ID))

	if secondWatcher {
		second, err := walcli.NewServicesAt(metadataDeploymentURI, metadataName)
		require.NoError(t, err)
		others = append(others, second)
		watchCtx, stopWatch := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		var watchErr error
		go func() {
			defer close(watchDone)
			watchErr = second.Ingest.Run(watchCtx, main.ID)
		}()
		t.Cleanup(func() {
			stopWatch()
			select {
			case <-watchDone:
				require.NoError(t, watchErr)
			case <-time.After(30 * time.Second):
				t.Error("second watcher did not stop")
			}
		})
		require.Eventually(t, func() bool {
			for _, status := range second.Ingest.Statuses() {
				if status.BranchID == main.ID && status.State == "running" {
					return true
				}
			}
			return false
		}, 15*time.Second, 20*time.Millisecond, "second watcher opened the parent's stream")
		defer func() {
			select {
			case <-watchDone:
				t.Errorf("second watcher stopped before the scenario finished: %v", watchErr)
			default:
			}
		}()
	}

	main, err = services.Branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	child, err := services.Branches.CreateBranch(project.ID, "work", main.ID)
	require.NoError(t, err)
	branchIDs = append(branchIDs, child.ID)

	// The application: keeps writing to the parent until told to stop.
	type write struct {
		id string
		at time.Time
	}
	var (
		inserted []write
		updates  int
		writeErr error
	)
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		live := mainDB.Collection("live")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%4 == 3 {
				a, b := fmt.Sprintf("t-%06d-a", i), fmt.Sprintf("t-%06d-b", i)
				session, err := services.Client.StartSession()
				if err == nil {
					_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
						if _, err := live.InsertOne(sc, bson.M{"_id": a}); err != nil {
							return nil, err
						}
						if _, err := live.InsertOne(sc, bson.M{"_id": b}); err != nil {
							return nil, err
						}
						_, err := live.UpdateOne(sc, bson.M{"_id": "counter"}, bson.M{"$inc": bson.M{"n": 1}})
						return nil, err
					})
					session.EndSession(ctx)
				}
				if err != nil {
					writeErr = err
					return
				}
				now := time.Now()
				inserted = append(inserted, write{a, now}, write{b, now})
				updates++
			} else {
				docID := fmt.Sprintf("s-%06d", i)
				_, err := live.InsertOne(ctx, bson.M{"_id": docID})
				if err != nil {
					writeErr = err
					return
				}
				inserted = append(inserted, write{docID, time.Now()})
			}
			// Checkout inserts stored images without decoding them, so the
			// copy is short. Write often enough that many writes land in it.
			time.Sleep(time.Millisecond)
		}
	}()
	stopWriter := sync.OnceFunc(func() {
		close(stop)
		<-writerDone
	})
	defer stopWriter()
	time.Sleep(200 * time.Millisecond)

	began := time.Now()
	info, err := services.Checkout.Checkout(ctx, child.ID)
	ended := time.Now()
	require.NoError(t, err)
	require.EqualValues(t, bulkDocs+1, info.Documents, "checkout copied the state the child forked from")
	time.Sleep(200 * time.Millisecond)
	stopWriter()
	require.NoError(t, writeErr, "the application's writes to the parent succeed throughout")

	// The drain that ends the parent's part of a checkout swallows its own
	// 30-second timeout. A barrier that a competing watcher consumed and did
	// not durably acknowledge shows up only as a checkout that waits it out.
	require.Less(t, ended.Sub(began), 20*time.Second,
		"the checkout did not wait out the parent's drain timeout")

	during, transactions := 0, 0
	for _, w := range inserted {
		if w.at.After(began) && w.at.Before(ended) {
			during++
			if strings.HasPrefix(w.id, "t-") && strings.HasSuffix(w.id, "-a") {
				transactions++
			}
		}
	}
	require.Greater(t, during, 20, "the scenario needs writes that land while the copy runs")
	require.Positive(t, transactions, "a transaction committed while the copy ran")

	// The parent's position is at or past the end of the child's copy.
	var childState struct {
		StartAt *primitive.Timestamp `bson:"start_at"`
	}
	require.NoError(t, state.FindOne(ctx, bson.M{"_id": child.ID}).Decode(&childState))
	require.NotNil(t, childState.StartAt)
	var parentState struct {
		ResumeToken bson.Raw `bson:"resume_token"`
	}
	require.NoError(t, state.FindOne(ctx, bson.M{"_id": main.ID}).Decode(&parentState))
	parentAt := resumeTokenClusterTime(t, parentState.ResumeToken)
	require.False(t, parentAt.Before(*childState.StartAt),
		"the parent's stream position (%v) is not behind the end of the child's copy (%v)", parentAt, *childState.StartAt)

	// Everything the application wrote is in the parent's WAL exactly once.
	require.NoError(t, services.SyncBranch(ctx, child.ID))
	cursor, err := metadata.Collection("wal_log").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"branch_id": main.ID, "collection": "live"}}},
		{{Key: "$group", Value: bson.M{"_id": "$document_id", "n": bson.M{"$sum": 1}}}},
	})
	require.NoError(t, err)
	var groups []struct {
		ID string `bson:"_id"`
		N  int    `bson:"n"`
	}
	require.NoError(t, cursor.All(ctx, &groups))
	counts := make(map[string]int, len(groups))
	for _, g := range groups {
		counts[g.ID] = g.N
	}
	var missing, duplicated []string
	for _, w := range inserted {
		switch n := counts[w.id]; {
		case n == 0:
			missing = append(missing, w.id)
		case n > 1:
			duplicated = append(duplicated, fmt.Sprintf("%s x%d", w.id, n))
		}
	}
	if len(missing) > 10 {
		missing = append(missing[:10], fmt.Sprintf("... %d in all", len(missing)))
	}
	if len(duplicated) > 10 {
		duplicated = append(duplicated[:10], fmt.Sprintf("... %d in all", len(duplicated)))
	}
	require.Empty(t, missing, "writes to the parent that never reached its WAL")
	require.Empty(t, duplicated, "writes to the parent captured more than once")
	require.Equal(t, 1+updates, counts["counter"], "one entry for the counter's insert and one per transaction")
	require.Len(t, counts, len(inserted)+1, "no entries for documents the application did not write")

	// The bulk documents were captured once each as well, and nothing of the
	// child's copy leaked into the parent's log.
	bulk, err := metadata.Collection("wal_log").CountDocuments(ctx, bson.M{"branch_id": main.ID, "collection": "bulk"})
	require.NoError(t, err)
	require.EqualValues(t, bulkDocs, bulk)

	// The parent's head covers every entry: nothing was appended behind it.
	main, err = services.Branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	var last struct {
		LSN int64 `bson:"lsn"`
	}
	cur, err := metadata.Collection("wal_log").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"branch_id": main.ID}}},
		{{Key: "$group", Value: bson.M{"_id": nil, "lsn": bson.M{"$max": "$lsn"}}}},
	})
	require.NoError(t, err)
	require.True(t, cur.Next(ctx))
	require.NoError(t, cur.Decode(&last))
	_ = cur.Close(ctx)
	require.Equal(t, last.LSN, main.HeadLSN, "the parent's head is its last WAL entry")
}

func TestCheckoutCapturesParentWritesDuringCopy(t *testing.T) {
	checkoutParentLiveWrites(t, false)
}

func TestCheckoutCapturesParentWritesWithSecondWatcher(t *testing.T) {
	checkoutParentLiveWrites(t, true)
}
