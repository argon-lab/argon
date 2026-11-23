package wal_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Compression requested through a legacy handshake must be negotiated away,
// so successful native writes reach the physical branch database.
func TestProxy_CompressedClientsWriteToBranch(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, label, params string, more ...*options.ClientOptions) (inBranch int64, strays []string, insertErr error) {
		p := newRound4Proxy(t, label)
		t.Cleanup(func() { _ = p.db.Client().Database(p.alias).Drop(context.Background()) })

		client := p.connect(t, params, more...)
		_, insertErr = client.Database(p.alias).Collection("zdocs").InsertOne(ctx, bson.M{"_id": "compressed"})
		t.Logf("insert through the alias returned: %v", insertErr)

		inBranch, err := p.physical.Collection("zdocs").CountDocuments(ctx, bson.M{"_id": "compressed"})
		require.NoError(t, err)
		strays, err = p.db.Client().ListDatabaseNames(ctx, bson.M{"name": p.alias})
		require.NoError(t, err)
		t.Logf("documents in the branch's database %s: %d; databases named after the alias: %v", p.physical.Name(), inBranch, strays)
		return inBranch, strays, insertErr
	}

	t.Run("control: no compression asked for", func(t *testing.T) {
		n, strays, err := run(t, "plain", "")
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)
		assert.Empty(t, strays)
	})

	t.Run("control: compressors=zlib with a server API version (the handshake is an OP_MSG, which the proxy strips)", func(t *testing.T) {
		api := options.Client().SetServerAPIOptions(options.ServerAPI(options.ServerAPIVersion1))
		n, strays, err := run(t, "zlib-api", "&compressors=zlib", api)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)
		assert.Empty(t, strays)
	})

	for _, compressor := range []string{"zlib", "zstd", "snappy"} {
		t.Run("compressors="+compressor, func(t *testing.T) {
			n, strays, err := run(t, compressor, "&compressors="+compressor)
			require.NoError(t, err)
			assert.Empty(t, strays, "the insert must not create a database named after the alias")
			assert.EqualValues(t, 1, n, "the acknowledged insert must reach the branch's database")
		})
	}
}
