package wal_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// startUnprivilegedProxy forwards to a mongod but makes replSetGetConfig fail
// (the command name is rewritten so the server answers CommandNotFound), the
// way it fails with Unauthorized for a readWrite-only user. Everything else
// passes through untouched.
func startUnprivilegedProxy(t *testing.T, upstream string) (addr string, blocked *atomic.Int64) {
	t.Helper()
	blocked = &atomic.Int64{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(client net.Conn) {
				defer func() { _ = client.Close() }()
				server, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
				header := make([]byte, 4)
				for {
					if _, err := io.ReadFull(client, header); err != nil {
						return
					}
					size := int(binary.LittleEndian.Uint32(header))
					if size < 4 || size > 64<<20 {
						return
					}
					body := make([]byte, size-4)
					if _, err := io.ReadFull(client, body); err != nil {
						return
					}
					if i := bytes.Index(body, []byte("replSetGetConfig\x00")); i >= 0 {
						copy(body[i:], "replSetGetConfiX")
						blocked.Add(1)
					}
					if _, err := server.Write(append(append([]byte{}, header...), body...)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), blocked
}

// The binding recorded a replica-set ID. A connection that leads to a
// deployment with the same set NAME but a different ID is refused when the
// ID can be read, and must also be refused when it cannot.
func TestWriteBack_RefusedWhenTheReplicaSetIDCannotBeRead(t *testing.T) {
	sourceURI := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI")
	if sourceURI == "" {
		t.Skip("needs ARGON_TEST_SOURCE_MONGODB_URI")
	}
	ctx := context.Background()
	f := newFollowFixture(t, sourceURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 4)
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	main := f.main(t)
	binding, err := f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	require.NotEmpty(t, binding.ReplicaSetID, "the privileged import recorded the replica-set ID")
	t.Logf("binding: set_name=%s replica_set_id=%s", binding.SetName, binding.ReplicaSetID)

	// Stand-in for "the project was imported from another deployment that is
	// also called rs1": the recorded ID is one this deployment does not have.
	// From here on the deployment at sourceURI is, by the binding's own
	// record, NOT the followed source.
	otherID := primitive.NewObjectID().Hex()
	res, err := f.services.Client.Database(f.metadataName).Collection("wal_sources").
		UpdateOne(ctx, bson.M{"_id": main.ID}, bson.M{"$set": bson.M{"replica_set_id": otherID}})
	require.NoError(t, err)
	require.EqualValues(t, 1, res.ModifiedCount)

	catalog := f.source.Collection("catalog")
	countNew := func() int64 {
		n, err := catalog.CountDocuments(ctx, bson.M{"_id": "item-new"})
		require.NoError(t, err)
		return n
	}

	// Control: with a connection that can read the ID, the mismatch is seen.
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", sourceURI)
	require.ErrorContains(t, err, "different deployment")
	t.Logf("privileged connection: refused: %v", err)
	require.EqualValues(t, 0, countNew())
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)

	// Same deployment, same mismatch, but replSetGetConfig is denied.
	addr, blocked := startUnprivilegedProxy(t, "localhost:27018")
	blindURI := fmt.Sprintf("mongodb://%s/?directConnection=true", addr)
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", blindURI)
	t.Logf("unprivileged connection: replSetGetConfig denied %d time(s); result=%+v err=%v", blocked.Load(), result, err)
	require.Positive(t, blocked.Load(), "the proxy denied replSetGetConfig")

	applied := f.plan(t, plan)
	t.Logf("plan status=%s write_back=%+v; item-new in the unverified deployment=%d", applied.Status, applied.WriteBack, countNew())
	require.Error(t, err, "write-back must refuse a deployment whose replica-set ID it cannot prove when the binding recorded one")
	require.EqualValues(t, 0, countNew(), "nothing may be written to a deployment the binding does not name")
}
