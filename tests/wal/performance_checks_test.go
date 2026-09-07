package wal_test

import (
	"os"
	"testing"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Absolute performance floors describe a calibrated machine/topology, not a
// correctness contract. Keep their original values for explicit performance
// runs; ordinary CI always exercises the workload and validates its history.
// MongoDB journal/majority latency alone can exceed the historical floors even
// on the unchanged engine. Use the external benchmarks suite for distributions.
func performanceGreater(t *testing.T, actual, minimum interface{}, message ...interface{}) {
	t.Helper()
	if os.Getenv("ARGON_PERF_ASSERT") == "1" {
		assert.Greater(t, actual, minimum, message...)
		return
	}
	t.Logf("absolute performance floor recorded (set ARGON_PERF_ASSERT=1 to enforce): actual=%v minimum=%v", actual, minimum)
}
func performanceLess(t *testing.T, actual, maximum interface{}, message ...interface{}) {
	t.Helper()
	if os.Getenv("ARGON_PERF_ASSERT") == "1" {
		assert.Less(t, actual, maximum, message...)
		return
	}
	t.Logf("absolute performance ceiling recorded (set ARGON_PERF_ASSERT=1 to enforce): actual=%v maximum=%v", actual, maximum)
}

func requireWriterHistory(t *testing.T, branches *branchwal.BranchService, log *wal.Service, branchID, collection string, count int) {
	t.Helper()
	fresh, err := branches.GetBranchByID(branchID)
	require.NoError(t, err)
	entries, err := log.GetBranchEntries(branchID, collection, 0, fresh.HeadLSN)
	require.NoError(t, err)
	require.Len(t, entries, count, "every acknowledged write must be visible below the published head")
	ids := make(map[string]bool, count)
	var previous int64
	for _, entry := range entries {
		require.Greater(t, entry.LSN, previous, "history must have unique ordered LSNs")
		require.NotEmpty(t, entry.PostImage, "every put must retain its complete post-image")
		require.False(t, ids[entry.DocumentID], "unique fixture IDs must not be duplicated")
		ids[entry.DocumentID] = true
		previous = entry.LSN
	}
	require.Equal(t, previous, fresh.HeadLSN, "published head must include the final acknowledged write")
}
