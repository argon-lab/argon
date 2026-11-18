package ingest

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type claimLedger struct {
	WriteBackLedger
	claim *WriteBack
}

func (l *claimLedger) Lookup(context.Context, string, primitive.Binary) (*WriteBack, error) {
	return l.claim, nil
}

func TestFollowerSeesClaimAfterMiss(t *testing.T) {
	ledger := &claimLedger{}
	f := follower{branch: &wal.Branch{ID: "main"}, opts: FollowOptions{WriteBacks: ledger}, writeBacks: map[string]*WriteBack{}}
	id, err := bson.Marshal(bson.M{"id": primitive.Binary{Subtype: 4, Data: make([]byte, 16)}})
	require.NoError(t, err)
	event := changeEvent{LSID: id}
	got, err := f.writeBack(t.Context(), &event)
	require.NoError(t, err)
	require.Nil(t, got)
	ledger.claim = &WriteBack{PlanID: "new-claim"}
	// The rest of an unclaimed transaction needs no further lookup.
	got, err = f.writeBack(t.Context(), &event)
	require.NoError(t, err)
	require.Nil(t, got)
	event.TxnNumber++
	got, err = f.writeBack(t.Context(), &event)
	require.NoError(t, err)
	require.Equal(t, ledger.claim, got)
}
