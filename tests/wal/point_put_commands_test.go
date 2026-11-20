package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
)

func TestSource_TypedPutReclaimGateCommands(t *testing.T) {
	for _, scenario := range []string{"no_floor", "floor_in_another_collection", "floor_below_retained_row"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			h := newHistoryFixture(t, "put-commands")
			const collection = "items"
			older, err := h.writer.Put(ctx, collection, bson.M{"_id": "older"})
			require.NoError(t, err)
			_, err = h.writer.Put(ctx, collection, bson.M{"_id": "target", "value": 1})
			require.NoError(t, err)
			newer, err := h.writer.Put(ctx, "other", bson.M{"_id": "other"})
			require.NoError(t, err)

			switch scenario {
			case "floor_in_another_collection":
				require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "other", newer))
			case "floor_below_retained_row":
				require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, collection, older))
			}

			var commands []string
			monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
				if commands == nil {
					return
				}
				target, ok := e.Command.Lookup(e.CommandName).StringValueOK()
				if !ok {
					target = e.DatabaseName
				}
				commands = append(commands, e.CommandName+" "+target)
			}}
			client := electionClient(t, metadataDeploymentURI, monitor)
			f := newSnapshotFixture(t, client.Database(h.db.Name()))
			w := walwriter.New(f.wal, f.branches, f.mat, h.main)
			commands = []string{}
			_, err = w.Put(ctx, collection, bson.M{"_id": "target", "value": 2})
			require.NoError(t, err)
			// Recorded on a git archive of v2.8.1 for this overwrite in all three cases.
			require.Equal(t, []string{
				"find wal_branches", "find wal_log", "update wal_branches",
				"findAndModify wal_counters", "insert wal_log", "update wal_branches",
				"commitTransaction admin",
			}, commands)
		})
	}
}
