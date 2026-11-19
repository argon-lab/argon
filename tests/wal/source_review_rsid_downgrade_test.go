package wal_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/source"
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
	addr = startSourceCommandProxy(t, upstream, func(message []byte) ([]byte, bool) {
		if i := bytes.Index(message, []byte("replSetGetConfig\x00")); i >= 0 {
			copy(message[i:], "replSetGetConfiX")
			blocked.Add(1)
		}
		return nil, false
	})
	return addr, blocked
}

// inspect may modify the request, supply a reply, or close the connection.
func startSourceCommandProxy(t *testing.T, upstream string, inspect func([]byte) (reply []byte, closeConn bool)) string {
	t.Helper()
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
				var mu sync.Mutex
				writeReply := func(reply []byte) error {
					mu.Lock()
					defer mu.Unlock()
					_, err := client.Write(reply)
					return err
				}
				go func() {
					defer func() { _ = client.Close() }()
					for {
						message, err := readSourceWireMessage(server)
						if err != nil || writeReply(message) != nil {
							return
						}
					}
				}()
				for {
					message, err := readSourceWireMessage(client)
					if err != nil {
						return
					}
					reply, closeConn := inspect(message)
					if closeConn {
						return
					}
					if reply != nil {
						err = writeReply(reply)
					} else {
						_, err = server.Write(message)
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func readSourceWireMessage(r io.Reader) ([]byte, error) {
	var header [16]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := int(binary.LittleEndian.Uint32(header[:]))
	if size < len(header) || size > 64<<20 {
		return nil, fmt.Errorf("invalid MongoDB message length %d", size)
	}
	message := make([]byte, size)
	copy(message, header[:])
	_, err := io.ReadFull(r, message[len(header):])
	return message, err
}

func TestSource_IdentifyFailsWhenReplicaSetConfigConnectionDrops(t *testing.T) {
	var blocked atomic.Int64
	addr := startSourceCommandProxy(t, "localhost:27017", func(message []byte) ([]byte, bool) {
		if sourceWireCommand(message).Lookup("replSetGetConfig").Type == 0 {
			return nil, false
		}
		blocked.Add(1)
		return nil, true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := source.Connect(ctx, "mongodb://"+addr+"/?directConnection=true")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	_, err = source.Identify(ctx, client)
	require.Positive(t, blocked.Load(), "the proxy closed the connection for replSetGetConfig")
	require.ErrorContains(t, err, "failed to read the source's replica-set configuration")
}

// The binding recorded a replica-set ID. A connection that leads to a
// deployment with the same set NAME but a different ID is refused when the
// ID can be read, and must also be refused when it cannot.
func TestWriteBack_RefusedWhenTheReplicaSetIDCannotBeRead(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
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
	// Approve the simulated import binding so both calls exercise verification
	// of the connected deployment.
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 4)
	require.Equal(t, otherID, plan.FollowedReplicaSetID)

	catalog := f.source.Collection("catalog")
	countNew := func() int64 {
		n, err := catalog.CountDocuments(ctx, bson.M{"_id": "item-new"})
		require.NoError(t, err)
		return n
	}

	// Control: with a connection that can read the ID, the mismatch is seen.
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", sourceURI, approvalFor(t, plan))
	require.ErrorContains(t, err, "different deployment")
	t.Logf("privileged connection: refused: %v", err)
	require.EqualValues(t, 0, countNew())
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)

	// Same deployment, same mismatch, but replSetGetConfig is denied.
	addr, blocked := startUnprivilegedProxy(t, "localhost:27018")
	blindURI := fmt.Sprintf("mongodb://%s/?directConnection=true", addr)
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", blindURI, approvalFor(t, plan))
	t.Logf("unprivileged connection: replSetGetConfig denied %d time(s); result=%+v err=%v", blocked.Load(), result, err)
	require.Positive(t, blocked.Load(), "the proxy denied replSetGetConfig")

	applied := f.plan(t, plan)
	t.Logf("plan status=%s write_back=%+v; item-new in the unverified deployment=%d", applied.Status, applied.WriteBack, countNew())
	require.Error(t, err, "write-back must refuse a deployment whose replica-set ID it cannot prove when the binding recorded one")
	require.EqualValues(t, 0, countNew(), "nothing may be written to a deployment the binding does not name")
}
