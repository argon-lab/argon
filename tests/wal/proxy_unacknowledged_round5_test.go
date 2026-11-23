package wal_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// Round 5 (4c24144, wireproxy.pump: "if unacknowledged(message) { continue }").
//
// The rows of the case table that the round 4 test does not reach: an
// unacknowledged write the proxy does NOT refuse is still forwarded, with
// its moreToCome bit, and nothing is written back for it; and the commands
// after it are answered in step.
func TestRound5_AnUnacknowledgedWriteThroughAGoodAliasIsForwardedAndNotAnswered(t *testing.T) {
	ctx := context.Background()
	p := newRound4Proxy(t, "w0-forward")
	client := p.connect(t, "&maxPoolSize=1")
	docs := client.Database(p.alias).Collection("w0")
	_, err := docs.InsertMany(ctx, []interface{}{
		bson.M{"_id": "a", "owner": "alice"},
		bson.M{"_id": "b", "owner": "bob"},
	})
	require.NoError(t, err)

	unacknowledged := options.Collection().SetWriteConcern(writeconcern.Unacknowledged())
	_, err = client.Database(p.alias).Collection("w0", unacknowledged).InsertOne(ctx, bson.M{"_id": "fire-and-forget"})
	t.Logf("w:0 insert through the good alias returned: %v", err)

	require.Eventually(t, func() bool {
		n, err := p.physical.Collection("w0").CountDocuments(ctx, bson.M{"_id": "fire-and-forget"})
		return err == nil && n == 1
	}, 5*time.Second, 10*time.Millisecond, "the w:0 insert was forwarded to the branch's own database")

	// Nothing was written to a database named after the alias.
	names, err := p.db.Client().ListDatabaseNames(ctx, bson.M{"name": p.alias})
	require.NoError(t, err)
	assert.Empty(t, names, "no database is named after the alias")

	for _, id := range []string{"a", "b", "a"} {
		var doc bson.M
		require.NoError(t, docs.FindOne(ctx, bson.M{"_id": id}).Decode(&doc))
		assert.Equal(t, id, doc["_id"], "each find gets its own document after the w:0 insert")
	}
}

func round5OpMsg(t *testing.T, requestID uint32, flags uint32, command bson.D) []byte {
	t.Helper()
	doc, err := bson.Marshal(command)
	require.NoError(t, err)
	out := make([]byte, 16, 21+len(doc))
	binary.LittleEndian.PutUint32(out[4:8], requestID)
	binary.LittleEndian.PutUint32(out[12:16], 2013)
	var f [4]byte
	binary.LittleEndian.PutUint32(f[:], flags)
	out = append(out, f[:]...)
	out = append(out, 0)
	out = append(out, doc...)
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(out)))
	return out
}

func round5ReadReply(t *testing.T, conn net.Conn) (responseTo uint32, flags uint32, body bson.Raw) {
	t.Helper()
	header := make([]byte, 16)
	_, err := io.ReadFull(conn, header)
	require.NoError(t, err)
	rest := make([]byte, binary.LittleEndian.Uint32(header[0:4])-16)
	_, err = io.ReadFull(conn, rest)
	require.NoError(t, err)
	require.EqualValues(t, 2013, binary.LittleEndian.Uint32(header[12:16]))
	return binary.LittleEndian.Uint32(header[8:12]), binary.LittleEndian.Uint32(rest[0:4]), bson.Raw(rest[5:])
}

// The same on the wire, frame by frame, for each combination of flag bits a
// refused command can carry. Only moreToCome (bit 1) silences the reply:
// checksumPresent (bit 0) and exhaustAllowed (bit 16) do not.
func TestRound5_OnlyMoreToComeSilencesTheReplyToARefusedCommand(t *testing.T) {
	p := newRound4Proxy(t, "w0-wire")
	missing := p.project + "~no-such-branch"

	const (
		checksumPresent = 1 << 0
		moreToCome      = 1 << 1
		exhaustAllowed  = 1 << 16
	)
	cases := []struct {
		name    string
		flags   uint32
		replied bool
	}{
		{"no flags", 0, true},
		{"exhaustAllowed", exhaustAllowed, true},
		{"moreToCome", moreToCome, false},
		{"moreToCome and exhaustAllowed", moreToCome | exhaustAllowed, false},
		{"checksumPresent", checksumPresent, true},
		{"moreToCome and checksumPresent", moreToCome | checksumPresent, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", p.addr)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

			refused := round5OpMsg(t, 101, tc.flags&^checksumPresent, bson.D{
				{Key: "insert", Value: "w0"},
				{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "x"}}}},
				{Key: "$db", Value: missing},
			})
			if tc.flags&checksumPresent != 0 {
				binary.LittleEndian.PutUint32(refused[16:20], tc.flags)
				refused = append(refused, 0, 0, 0, 0) // the proxy strips it unread
				binary.LittleEndian.PutUint32(refused[0:4], uint32(len(refused)))
			}
			_, err = conn.Write(refused)
			require.NoError(t, err)

			// The next command on the connection: a ping the server answers.
			_, err = conn.Write(round5OpMsg(t, 202, 0, bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}}))
			require.NoError(t, err)

			responseTo, _, body := round5ReadReply(t, conn)
			if tc.replied {
				require.EqualValues(t, 101, responseTo, "the refused command is answered first: %s", body)
				assert.EqualValues(t, 26, body.Lookup("code").AsInt64())
				responseTo, _, body = round5ReadReply(t, conn)
			}
			require.EqualValues(t, 202, responseTo, "the ping's own answer: %s", body)
			assert.EqualValues(t, 1, body.Lookup("ok").AsInt64(), "%s", body)

			// And nothing else is waiting on the connection.
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
			extra := make([]byte, 1)
			n, err := conn.Read(extra)
			assert.Zero(t, n, "an extra reply was written")
			var timeout net.Error
			assert.ErrorAs(t, err, &timeout, "the connection stays open and silent")
		})
	}
}
