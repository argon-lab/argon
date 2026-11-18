package wal_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// The test deployments run without enableTestCommands, so the failCommand
// fail point is not available. This dialer does the same thing from the
// client side: a command is sent unchanged and applied, then its reply is
// replaced with ok:1 plus writeConcernError 50 (MaxTimeMSExpired). For
// commitTransaction the driver labels that UnknownTransactionCommitResult
// and WithTransaction returns it without retrying.
type lostAckDialer struct {
	command         string
	acknowledgments atomic.Int32
}

func (d *lostAckDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &lostAckConn{Conn: conn, dialer: d}, nil
}

type lostAckConn struct {
	net.Conn
	dialer  *lostAckDialer
	mu      sync.Mutex
	rewrite bool
	pending bytes.Buffer
}

func (c *lostAckConn) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(c.dialer.command+"\x00")) {
		c.mu.Lock()
		c.rewrite = true
		c.mu.Unlock()
	}
	return c.Conn.Write(p)
}

func (c *lostAckConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending.Len() > 0 {
		return c.pending.Read(p)
	}
	if !c.rewrite {
		return c.Conn.Read(p)
	}
	c.rewrite = false
	// Read the server's whole reply: the command has been applied by now.
	header := make([]byte, 16)
	if _, err := io.ReadFull(c.Conn, header); err != nil {
		return 0, err
	}
	body := make([]byte, int(binary.LittleEndian.Uint32(header[0:4]))-16)
	if _, err := io.ReadFull(c.Conn, body); err != nil {
		return 0, err
	}
	// OP_MSG: flagBits(4) kind(1) document.
	if binary.LittleEndian.Uint32(header[12:16]) != 2013 || body[4] != 0 {
		c.pending.Write(header)
		c.pending.Write(body)
		return c.pending.Read(p)
	}
	var reply bson.D
	if err := bson.Unmarshal(body[5:], &reply); err != nil {
		return 0, err
	}
	ok := false
	for _, e := range reply {
		if e.Key == "ok" {
			switch v := e.Value.(type) {
			case float64:
				ok = v == 1
			case int32:
				ok = v == 1
			}
		}
	}
	if ok {
		c.dialer.acknowledgments.Add(1)
		reply = append(reply, bson.E{Key: "writeConcernError", Value: bson.D{
			{Key: "code", Value: int32(50)},
			{Key: "codeName", Value: "MaxTimeMSExpired"},
			{Key: "errmsg", Value: "operation exceeded time limit while waiting for replication"},
		}})
	}
	doc, err := bson.Marshal(reply)
	if err != nil {
		return 0, err
	}
	binary.LittleEndian.PutUint32(header[0:4], uint32(16+5+len(doc)))
	c.pending.Write(header)
	c.pending.Write(body[:5])
	c.pending.Write(doc)
	return c.pending.Read(p)
}

// The source transaction commits and the acknowledgement does not report an
// outcome. The plan must stay claimed with its session, must not be written
// again, and following must settle it as applied and attribute the entries.
func TestWriteBack_UnknownCommitOutcomeKeepsThePlanClaimed(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 4)
	before := f.sourceState(t, nil)

	binding, err := f.services.Sources.Get(ctx, plan.TargetBranchID)
	require.NoError(t, err)
	require.NotNil(t, binding)
	dialer := &lostAckDialer{command: "commitTransaction"}
	faulty, err := mongo.Connect(ctx, options.Client().ApplyURI(f.sourceURI).SetDialer(dialer))
	require.NoError(t, err)
	defer func() { _ = faulty.Disconnect(context.Background()) }()

	_, err = f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{
		Database: faulty.Database(f.source.Name()), Display: binding.Display,
	})
	t.Logf("ApplyToSource returned: %v", err)
	require.EqualValues(t, 1, dialer.acknowledgments.Load(), "the server acknowledged exactly one commitTransaction with ok:1")

	// The transaction did reach the source.
	written := f.sourceState(t, nil)
	require.NotEqual(t, before, written, "the source was written")
	catalog := f.source.Collection("catalog")
	require.ErrorIs(t, catalog.FindOne(ctx, bson.M{"_id": "item-03"}).Err(), mongo.ErrNoDocuments)
	require.NoError(t, catalog.FindOne(ctx, bson.M{"_id": "item-new"}).Err())

	claimed := f.plan(t, plan)
	t.Logf("plan after the lost acknowledgement: status=%s write_back=%v", claimed.Status, claimed.WriteBack != nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "did not report an outcome")
	require.Equal(t, merge.StatusWritingBack, claimed.Status, "a plan written to the source must not be reported pending")
	require.NotNil(t, claimed.WriteBack, "the session record is what lets the follower attribute the events")
	require.NotEmpty(t, claimed.WriteBack.Session.Data)

	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "will not be written again")
	require.Equal(t, written, f.sourceState(t, nil))

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	settled := f.plan(t, plan)
	require.Equal(t, merge.StatusApplied, settled.Status)
	require.NotNil(t, settled.WriteBack.SettledAt)
	require.Nil(t, settled.WriteBack.CommittedAt)
	f.requireMainEqualsSource(t, 0, nil, "after following settled the write-back")

	main := f.main(t)
	entries, err := f.services.WAL.GetEntries(bson.M{"branch_id": main.ID, "txn_id": "merge:" + plan.ID.Hex()}, options.Find().SetSort(bson.M{"lsn": 1}))
	require.NoError(t, err)
	require.Len(t, entries, 5, "four documents and the merge record")
	for _, entry := range entries[:4] {
		require.Equal(t, "merge:agent-run", entry.Actor)
	}
	require.Equal(t, wal.OpMerge, entries[4].Operation)
}

// Two processes write the same plan back at once. Exactly one may claim it.
func TestWriteBack_ConcurrentCallsClaimOnce(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)

	const callers = 4
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		t.Logf("caller %d: %v", i, err)
		if err == nil {
			succeeded++
			continue
		}
		msg := err.Error()
		refused := bytes.Contains([]byte(msg), []byte("no longer pending")) ||
			bytes.Contains([]byte(msg), []byte("not pending")) ||
			bytes.Contains([]byte(msg), []byte("will not be written again"))
		require.True(t, refused, "a caller that lost the claim must be refused before it opens a transaction, got: %v", err)
	}
	require.Equal(t, 1, succeeded)
	final := f.plan(t, plan)
	require.Equal(t, merge.StatusApplied, final.Status)
	require.NotNil(t, final.WriteBack)
	require.NotNil(t, final.WriteBack.CommittedAt)
}
