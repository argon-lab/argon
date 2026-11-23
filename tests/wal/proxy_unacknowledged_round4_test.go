package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// Round 4, the proxy's error replies and an unacknowledged write.
//
// A write with w:0 is sent with OP_MSG's moreToCome bit: the client does not
// read a reply to it, and a server sends none. The proxy answers a command it
// will not forward with a reply of its own (pump: "if errReply != nil {
// client.Write(errReply)") whatever the flag bits say. Nobody reads that
// reply; it sits in the socket, and the next command on the connection reads
// it as its own answer. From then on every command on that connection is
// answered with the reply to the command before it, for as long as the
// connection lives: the Go driver does not compare responseTo.
//
// The reply has been written without a look at the flag bits since the proxy
// was added (the same on master, with NamespaceNotFound for an alias that
// names nothing: the first subtest uses nothing of this PR). 0e6aae9 keeps
// that, and its HostUnreachable makes the outcome quieter: the driver
// retries the read that got the stale HostUnreachable, the retry reads the
// first attempt's reply, and the application sees no error at all before it
// is handed another query's documents.
func TestRound4_AReplyToAnUnacknowledgedWriteAnswersTheNextCommand(t *testing.T) {
	ctx := context.Background()
	unacknowledged := options.Collection().SetWriteConcern(writeconcern.Unacknowledged())

	setup := func(t *testing.T, label string) (*round4Proxy, *mongo.Client, func(id string) (bson.M, error)) {
		p := newRound4Proxy(t, label)
		// One connection, so that the commands below share it; in a larger
		// pool the same connection is handed out again sooner or later.
		client := p.connect(t, "&maxPoolSize=1")
		docs := client.Database(p.alias).Collection("w0")
		_, err := docs.InsertMany(ctx, []interface{}{
			bson.M{"_id": "a", "owner": "alice"},
			bson.M{"_id": "b", "owner": "bob"},
		})
		require.NoError(t, err)
		find := func(id string) (bson.M, error) {
			var doc bson.M
			err := docs.FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
			return doc, err
		}
		for _, id := range []string{"a", "b"} {
			doc, err := find(id)
			require.NoError(t, err)
			require.Equal(t, id, doc["_id"], "the control: each find gets its own document")
		}
		return p, client, find
	}

	t.Run("w:0 to an alias that names nothing (NamespaceNotFound, as on master)", func(t *testing.T) {
		p, client, find := setup(t, "w0-missing")

		before := p.lookups.byBranch.Load()
		_, err := client.Database(p.project+"~no-such-branch").Collection("w0", unacknowledged).InsertOne(ctx, bson.M{"_id": "fire-and-forget"})
		t.Logf("w:0 insert through an alias with no branch returned: %v", err)
		require.Eventually(t, func() bool { return p.lookups.byBranch.Load() > before }, 5*time.Second, 5*time.Millisecond,
			"the control: the proxy looked the alias up")
		time.Sleep(50 * time.Millisecond)

		a, errA := find("a")
		t.Logf(`find {_id: "a"} returned: doc=%v err=%v`, a, errA)
		b, errB := find("b")
		t.Logf(`find {_id: "b"} returned: doc=%v err=%v`, b, errB)

		assert.NoError(t, errA, "a find through the good alias was answered with the reply to the w:0 insert")
		require.NoError(t, errB)
		assert.Equal(t, "b", b["_id"], `the find of "b" was answered with the reply to the find of "a"`)
	})

	t.Run("w:0 while one metadata read fails (HostUnreachable)", func(t *testing.T) {
		p, client, find := setup(t, "w0-blip")

		p.lookups.failNext.Store(1)
		_, err := client.Database(p.alias).Collection("w0", unacknowledged).InsertOne(ctx, bson.M{"_id": "fire-and-forget"})
		t.Logf("w:0 insert over a blip returned: %v", err)
		require.Eventually(t, func() bool { return p.lookups.failed.Load() == 1 }, 5*time.Second, 5*time.Millisecond,
			"the control: the w:0 insert is the command the proxy refused")
		time.Sleep(50 * time.Millisecond)

		a, errA := find("a")
		t.Logf(`find {_id: "a"} returned: doc=%v err=%v`, a, errA)
		b, errB := find("b")
		t.Logf(`find {_id: "b"} returned: doc=%v err=%v`, b, errB)

		assert.NoError(t, errA)
		require.NoError(t, errB)
		assert.Equal(t, "b", b["_id"], `the application asked for "b" and was handed, without an error, the reply to the find of "a"`)
	})
}
