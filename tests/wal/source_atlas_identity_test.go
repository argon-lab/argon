package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/stretchr/testify/require"
)

// The write-back fence for each kind of source identity. A plan records the
// binding's WriteIdentity at preview; the claim must find the same binding.
func TestSourceWriteIdentityFence(t *testing.T) {
	ctx := t.Context()
	db := setupTestDB(t)
	registry, err := source.NewRegistry(db)
	require.NoError(t, err)
	hosts := []string{"ac-a-shard-00-00.x.mongodb.net:27017"}

	atlas := &source.Binding{BranchID: "atlas", ProjectID: "p", Database: "shop", Display: "d", SetName: "atlas-x-shard-0", Hosts: hosts, AtlasTLS: true}
	byID := &source.Binding{BranchID: "byid", ProjectID: "p", Database: "shop", Display: "d", SetName: "atlas-x-shard-0", Hosts: hosts, ReplicaSetID: "66aa"}
	byName := &source.Binding{BranchID: "byname", ProjectID: "p", Database: "shop", Display: "d", SetName: "atlas-x-shard-0", Hosts: hosts}
	for _, b := range []*source.Binding{atlas, byID, byName} {
		require.NoError(t, registry.Bind(ctx, b))
	}
	require.Equal(t, "atlas-tls:atlas-x-shard-0", atlas.WriteIdentity())
	require.Equal(t, "66aa", byID.WriteIdentity())
	require.Empty(t, byName.WriteIdentity())

	require.NoError(t, source.FenceWriteBackClaim(ctx, db, "atlas", "p", "shop", atlas.WriteIdentity()))
	require.NoError(t, source.FenceWriteBackClaim(ctx, db, "byid", "p", "shop", byID.WriteIdentity()))
	require.Error(t, source.FenceWriteBackClaim(ctx, db, "atlas", "p", "shop", "atlas-tls:atlas-y-shard-0"), "another set name")
	require.Error(t, source.FenceWriteBackClaim(ctx, db, "byid", "p", "shop", "atlas-tls:atlas-x-shard-0"), "an ID binding is not an Atlas binding")
	require.Error(t, source.FenceWriteBackClaim(ctx, db, "byname", "p", "shop", ""), "no write identity")
	require.Error(t, source.FenceWriteBackClaim(ctx, db, "byname", "p", "shop", "atlas-tls:atlas-x-shard-0"), "a binding without the Atlas record")

	require.NoError(t, registry.Unbind(ctx, "atlas"))
	require.Error(t, source.FenceWriteBackClaim(ctx, db, "atlas", "p", "shop", atlas.WriteIdentity()), "detached")
}
