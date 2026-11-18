package wal_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"testing"

	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// startLookalikeSourceProxy forwards to the mongod at upstream and rewrites
// only what that deployment says about itself in hello replies: its set name
// and its member host names. Both replacements keep their length. It stands
// in for a second deployment that was initiated with the same set name and
// the same member host name as the real source (rs0 / localhost:27017 from
// the README, a port-forward, or equal StatefulSet names in two clusters).
// replSetGetConfig returns the upstream deployment's own ID.
func startLookalikeSourceProxy(t *testing.T, upstream, fromSet, toSet, fromHost, toHost string) string {
	t.Helper()
	require.Equal(t, len(fromSet), len(toSet))
	require.Equal(t, len(fromHost), len(toHost))
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
				go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
				header := make([]byte, 4)
				for {
					if _, err := io.ReadFull(server, header); err != nil {
						return
					}
					size := int(binary.LittleEndian.Uint32(header))
					if size < 4 || size > 64<<20 {
						return
					}
					body := make([]byte, size-4)
					if _, err := io.ReadFull(server, body); err != nil {
						return
					}
					if bytes.Contains(body, []byte("setName\x00")) {
						body = bytes.ReplaceAll(body, []byte(fromSet+"\x00"), []byte(toSet+"\x00"))
						body = bytes.ReplaceAll(body, []byte(fromHost), []byte(toHost))
					}
					if _, err := client.Write(append(append([]byte{}, header...), body...)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// A privileged connection to a deployment that matches the weak identity
// leaves the recorded binding unchanged, whether the resync runs or is refused.
func TestResync_LookalikeSourceCannotChangeWeakBinding(t *testing.T) {
	otherURI := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI")
	if otherURI == "" {
		t.Skip("needs ARGON_TEST_SOURCE_MONGODB_URI (a second replica set)")
	}
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	realURI := f.sourceURI
	f.seedSmall(t)

	blindAddr, blocked := startUnprivilegedProxy(t, "localhost:27017")
	f.sourceURI = fmt.Sprintf("mongodb://%s/?directConnection=true", blindAddr)
	f.importOnline(t, walcli.OnlineImport{})
	f.sourceURI = realURI
	require.Positive(t, blocked.Load(), "the import could not read the replica-set ID")
	main := f.main(t)
	binding, err := f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	require.Empty(t, binding.ReplicaSetID)
	require.Equal(t, "rs0", binding.SetName)
	require.Equal(t, []string{"localhost:27017"}, binding.Hosts)

	realIdentity, err := source.Identify(ctx, f.sourceClient)
	require.NoError(t, err)
	otherClient, err := mongo.Connect(ctx, options.Client().ApplyURI(otherURI))
	require.NoError(t, err)
	otherDB := otherClient.Database(f.source.Name())
	t.Cleanup(func() {
		_ = otherDB.Drop(context.Background())
		_ = otherClient.Disconnect(context.Background())
	})
	otherIdentity, err := source.Identify(ctx, otherClient)
	require.NoError(t, err)
	require.NotEmpty(t, otherIdentity.ReplicaSetID)
	require.NotEqual(t, realIdentity.ReplicaSetID, otherIdentity.ReplicaSetID)
	cursor, err := f.source.Collection("catalog").Find(ctx, bson.D{})
	require.NoError(t, err)
	var docs []bson.Raw
	require.NoError(t, cursor.All(ctx, &docs))
	for _, doc := range docs {
		_, err := otherDB.Collection("catalog").InsertOne(ctx, doc)
		require.NoError(t, err)
	}

	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: otherURI, Force: true})
	require.ErrorContains(t, err, `replica set "rs1"`)

	lookalike := fmt.Sprintf("mongodb://%s/?directConnection=true",
		startLookalikeSourceProxy(t, "localhost:27018", "rs1", "rs0", "localhost:27018", "localhost:27017"))
	lookClient, err := mongo.Connect(ctx, options.Client().ApplyURI(lookalike))
	require.NoError(t, err)
	t.Cleanup(func() { _ = lookClient.Disconnect(context.Background()) })
	lookIdentity, err := source.Identify(ctx, lookClient)
	require.NoError(t, err)
	require.Equal(t, binding.SetName, lookIdentity.SetName)
	require.Equal(t, binding.Hosts, lookIdentity.Hosts)
	require.Equal(t, otherIdentity.ReplicaSetID, lookIdentity.ReplicaSetID)

	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: lookalike})
	require.ErrorContains(t, err, "has not ended as degraded")
	after, err := f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	require.Equal(t, binding, after)

	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: lookalike, Force: true})
	require.NoError(t, err)
	after, err = f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	require.Equal(t, binding, after)
	require.Equal(t, "replica-set name and member hosts", f.status(t).Identity)
	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: realURI, Force: true})
	require.NoError(t, err)
}
