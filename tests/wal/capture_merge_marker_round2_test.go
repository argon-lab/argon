package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// round2DataEntries counts data entries per collection/doc and their actors.
func round2DataEntries(t *testing.T, f *ingestFixture) (counts map[string]int, actors map[string]string, plans map[string]interface{}) {
	t.Helper()
	counts, actors, plans = map[string]int{}, map[string]string{}, map[string]interface{}{}
	for _, e := range reviewAllEntries(t, f) {
		if !e.IsData() {
			continue
		}
		k := e.Collection + "/" + e.DocumentID
		counts[k]++
		actors[k] = e.Actor
		plans[k] = e.Metadata["merge_plan"]
	}
	return
}

// round2BigMerge merges n documents per collection into collections that do
// not exist on main (so the transaction holds create events between data
// events) and returns the plan.
func round2BigMerge(t *testing.T, f *ingestFixture, project string, colls []string, n int) *merge.Plan {
	t.Helper()
	ctx := context.Background()
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch(project, "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	for _, c := range colls {
		for i := 0; i < n; i++ {
			_, err = writer.Put(ctx, c, bson.M{"_id": fmt.Sprintf("d%03d", i), "v": int32(i)})
			require.NoError(t, err)
		}
	}
	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Changes, n*len(colls))
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	return plan
}

// A merge larger than one cursor batch, into new collections, with capture
// stopped at several points and restarted: every merged document is recorded
// exactly once and attributed to the merge.
func TestRound2_LargeMergeIntoNewCollectionsRestartAtManyPoints(t *testing.T) {
	for _, wait := range []time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond, 400 * time.Millisecond, 900 * time.Millisecond, 2 * time.Second} {
		wait := wait
		t.Run(wait.String(), func(t *testing.T) {
			project := fmt.Sprintf("round2-big-%d", time.Now().UnixNano())
			f := newIngestFixture(t, project)
			ctx := context.Background()
			stop := f.startIngester(t)
			_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "app"})
			require.NoError(t, err)
			f.waitForEntries(t, "docs", 1)

			colls := []string{"freshA", "freshB", "freshC"}
			plan := round2BigMerge(t, f, project, colls, 120)
			time.Sleep(wait)
			stop()

			restart := f.startIngesterWith(t, ingest.NewService(f.client, f.metaDB, f.wal, f.branches))
			for _, c := range colls {
				f.waitForEntries(t, c, 120)
			}
			// Give a replay time to add duplicates if it would.
			_, err = f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "after"})
			require.NoError(t, err)
			f.waitForEntries(t, "docs", 2)
			restart()

			counts, actors, plans := round2DataEntries(t, f)
			for _, c := range colls {
				for i := 0; i < 120; i++ {
					k := fmt.Sprintf("%s/d%03d", c, i)
					assert.Equal(t, 1, counts[k], "count %s", k)
					assert.Equal(t, "merge:feature", actors[k], "actor %s", k)
					assert.Equal(t, plan.ID.Hex(), plans[k], "plan %s", k)
				}
			}
			assert.Equal(t, 1, counts["docs/app"])
			assert.Equal(t, 1, counts["docs/after"])
			reviewAssertNoMarkerEntries(t, f)
		})
	}
}

// After a merge into a new collection and a long idle period, the resume
// point is past the merge: a restart replays nothing, and later application
// writes, including a non-merge transaction that creates a collection, are
// captured once under the capture actor.
func TestRound2_IdleAfterMergeDoesNotHoldResumePoint(t *testing.T) {
	project := fmt.Sprintf("round2-idle-%d", time.Now().UnixNano())
	f := newIngestFixture(t, project)
	ctx := context.Background()
	stop := f.startIngester(t)
	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "app"})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)

	round2BigMerge(t, f, project, []string{"fresh"}, 3)
	f.waitForEntries(t, "fresh", 3)
	time.Sleep(2 * time.Second)

	// A non-merge transaction that creates a collection.
	sess, err := f.client.StartSession()
	require.NoError(t, err)
	_, err = sess.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if _, err := f.physical.Collection("apptxn").InsertOne(sc, bson.M{"_id": "t1"}); err != nil {
			return nil, err
		}
		_, err := f.physical.Collection("apptxn").InsertOne(sc, bson.M{"_id": "t2"})
		return nil, err
	})
	sess.EndSession(ctx)
	require.NoError(t, err)
	f.waitForEntries(t, "apptxn", 2)
	time.Sleep(2 * time.Second)
	headBefore, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	stop()

	restart := f.startIngesterWith(t, ingest.NewService(f.client, f.metaDB, f.wal, f.branches))
	time.Sleep(2 * time.Second)
	headAfter, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	assert.Equal(t, headBefore.HeadLSN, headAfter.HeadLSN, "restart replayed events past the resume point")
	_, err = f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "later"})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 2)
	restart()

	counts, actors, _ := round2DataEntries(t, f)
	for k, n := range counts {
		assert.Equal(t, 1, n, "count %s", k)
	}
	assert.Equal(t, "ingest", actors["apptxn/t1"])
	assert.Equal(t, "ingest", actors["docs/later"])
}

// A merge with zero changes opens and closes a marker with nothing between;
// the following application write is captured once under the capture actor,
// and a restart replays nothing.
func TestRound2_EmptyMarkerTransactionThenAppWrites(t *testing.T) {
	project := fmt.Sprintf("round2-empty-%d", time.Now().UnixNano())
	f := newIngestFixture(t, project)
	ctx := context.Background()
	stop := f.startIngester(t)
	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "app"})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)

	// Simulate the zero-change merge's transaction directly: marker in, out.
	sess, err := f.client.StartSession()
	require.NoError(t, err)
	_, err = sess.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		m := f.physical.Collection("__argon_merge_markers")
		if _, err := m.InsertOne(sc, bson.M{"_id": "merge:x", "actor": "merge:x", "plan_id": "x"}); err != nil {
			return nil, err
		}
		_, err := m.DeleteOne(sc, bson.M{"_id": "merge:x"})
		return nil, err
	})
	sess.EndSession(ctx)
	require.NoError(t, err)
	_, err = f.physical.Collection("other").InsertOne(ctx, bson.M{"_id": "o1"})
	require.NoError(t, err)
	f.waitForEntries(t, "other", 1)
	time.Sleep(1500 * time.Millisecond)
	stop()

	restart := f.startIngesterWith(t, ingest.NewService(f.client, f.metaDB, f.wal, f.branches))
	time.Sleep(1500 * time.Millisecond)
	restart()
	counts, actors, plans := round2DataEntries(t, f)
	assert.Equal(t, 1, counts["other/o1"])
	assert.Equal(t, "ingest", actors["other/o1"])
	assert.Nil(t, plans["other/o1"])
}

// A full batch of a non-merge transaction directly followed by a merge into a
// new collection, read in one go after capture was down: the maxBatch split
// flushes the earlier transaction with the resume point before the marker, so
// a second stop at any point still attributes the merge and records nothing
// twice.
func TestRound2_FullTxnBatchThenMergeSplitAndRestart(t *testing.T) {
	for _, wait := range []time.Duration{0, 30 * time.Millisecond, 100 * time.Millisecond, 300 * time.Millisecond, 1 * time.Second} {
		wait := wait
		t.Run(wait.String(), func(t *testing.T) {
			project := fmt.Sprintf("round2-split-%d", time.Now().UnixNano())
			f := newIngestFixture(t, project)
			ctx := context.Background()
			stop := f.startIngester(t)
			_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "app"})
			require.NoError(t, err)
			f.waitForEntries(t, "docs", 1)
			stop()

			sess, err := f.client.StartSession()
			require.NoError(t, err)
			_, err = sess.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
				for i := 0; i < 210; i++ {
					if _, err := f.physical.Collection("docs").InsertOne(sc, bson.M{"_id": fmt.Sprintf("t%03d", i)}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			sess.EndSession(ctx)
			require.NoError(t, err)
			plan := round2BigMerge(t, f, project, []string{"fresh"}, 5)

			first := f.startIngesterWith(t, ingest.NewService(f.client, f.metaDB, f.wal, f.branches))
			time.Sleep(wait)
			first()
			second := f.startIngesterWith(t, ingest.NewService(f.client, f.metaDB, f.wal, f.branches))
			f.waitForEntries(t, "fresh", 5)
			f.waitForEntries(t, "docs", 211)
			_, err = f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "after"})
			require.NoError(t, err)
			f.waitForEntries(t, "docs", 212)
			second()

			counts, actors, plans := round2DataEntries(t, f)
			for k, n := range counts {
				assert.Equal(t, 1, n, "count %s", k)
			}
			for i := 0; i < 5; i++ {
				k := fmt.Sprintf("fresh/d%03d", i)
				assert.Equal(t, "merge:feature", actors[k], "actor %s", k)
				assert.Equal(t, plan.ID.Hex(), plans[k], "plan %s", k)
			}
			assert.Equal(t, "ingest", actors["docs/t000"])
		})
	}
}
