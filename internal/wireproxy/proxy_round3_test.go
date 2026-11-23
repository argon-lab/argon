package wireproxy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 3 (what 6aa0f7a left of the round 2 finding "a metadata read error
// is not reported as a missing project"). The fix covers the read by ID that
// checks a cached alias. The other two metadata reads of resolve(), the ones
// by name in lookup(), still turn any error into `project "shop" not found`
// (or `branch "main" not found in project "shop"`) and resolve() caches that
// answer for a second. An alias whose five-second cache entry runs out while
// the metadata store cannot be reached, which every alias in use does within
// five seconds of an outage starting, is answered NamespaceNotFound "project
// not found", and still is for a second after the store is back.
//
// The same on master (lookup has always reported it so): pre-existing.
func TestRound3_AMetadataErrorOnALookupByNameIsNotAMissingProject(t *testing.T) {
	store := &round2Store{}
	proxy := New("127.0.0.1:1", store, store)

	physical, err := proxy.resolve("shop~main")
	require.NoError(t, err)
	require.Equal(t, "argon_br_b1", physical)

	// Five seconds pass: the cached target has run out.
	proxy.cacheMu.Lock()
	hit := proxy.cache["shop~main"]
	hit.expires = time.Now().Add(-time.Millisecond)
	proxy.cache["shop~main"] = hit
	proxy.cacheMu.Unlock()

	// The metadata store cannot be reached for one command.
	store.down = true
	_, during := proxy.resolve("shop~main")
	store.down = false
	require.Error(t, during)
	assert.NotContains(t, during.Error(), "not found",
		"the store could not be reached; the client is told its project does not exist")

	// The store is back: the next command, sent at once, goes through.
	physical, after := proxy.resolve("shop~main")
	assert.NoError(t, after, "the store is reachable again, and the alias still answers with the cached failure")
	assert.Equal(t, "argon_br_b1", physical)
}
