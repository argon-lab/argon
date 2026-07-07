package wal_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putEntry builds a minimal valid put entry for sequencer tests.
func putEntry(projectID, branchID, collection, docID string) *wal.Entry {
	return &wal.Entry{
		ProjectID:  projectID,
		BranchID:   branchID,
		Operation:  wal.OpPut,
		Collection: collection,
		DocumentID: docID,
		PostImage:  mustMarshalBSON(map[string]interface{}{"_id": docID}),
	}
}

// TestSequencer_ConcurrentMultiInstance verifies that LSN allocation is safe
// when multiple independent Service instances (simulating separate processes)
// append to the same project concurrently. This is the scenario the old
// in-memory atomic counter could not handle.
func TestSequencer_ConcurrentMultiInstance(t *testing.T) {
	db := setupTestDB(t)

	const (
		numInstances  = 4
		numGoroutines = 4
		numAppends    = 25
	)

	services := make([]*wal.Service, numInstances)
	for i := range services {
		svc, err := wal.NewService(db)
		require.NoError(t, err)
		services[i] = svc
	}

	var wg sync.WaitGroup
	lsnCh := make(chan int64, numInstances*numGoroutines*numAppends)
	errCh := make(chan error, numInstances*numGoroutines*numAppends)

	for i, svc := range services {
		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(svc *wal.Service, instance, goroutine int) {
				defer wg.Done()
				for k := 0; k < numAppends; k++ {
					docID := fmt.Sprintf("doc-%d-%d-%d", instance, goroutine, k)
					entry := &wal.Entry{
						ProjectID:  "multi-instance",
						BranchID:   "main",
						Operation:  wal.OpPut,
						Collection: "items",
						DocumentID: docID,
						PostImage:  mustMarshalBSON(map[string]interface{}{"_id": docID}),
					}
					lsn, err := svc.Append(entry)
					if err != nil {
						errCh <- err
						return
					}
					lsnCh <- lsn
				}
			}(svc, i, g)
		}
	}

	wg.Wait()
	close(lsnCh)
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent append failed: %v", err)
	}

	total := numInstances * numGoroutines * numAppends
	seen := make(map[int64]bool, total)
	var maxLSN int64
	count := 0
	for lsn := range lsnCh {
		assert.False(t, seen[lsn], "LSN %d was allocated twice", lsn)
		seen[lsn] = true
		if lsn > maxLSN {
			maxLSN = lsn
		}
		count++
	}

	assert.Equal(t, total, count, "every append should have produced an LSN")
	// No append failed, so the sequence must be dense: exactly `total` LSNs
	// starting at 1.
	assert.Equal(t, int64(total), maxLSN, "sequence should be dense when no append fails")
	assert.Equal(t, int64(total), services[0].GetCurrentLSN("multi-instance"))
}

// TestSequencer_PerProjectIsolation verifies that each project gets its own
// independent LSN sequence starting at 1.
func TestSequencer_PerProjectIsolation(t *testing.T) {
	db := setupTestDB(t)

	svc, err := wal.NewService(db)
	require.NoError(t, err)

	lsnA, err := svc.Append(putEntry("project-a", "main", "items", "a1"))
	require.NoError(t, err)

	lsnB, err := svc.Append(putEntry("project-b", "main", "items", "b1"))
	require.NoError(t, err)

	assert.Equal(t, int64(1), lsnA, "first LSN of project-a should be 1")
	assert.Equal(t, int64(1), lsnB, "first LSN of project-b should be 1 regardless of other projects")

	lsnA2, err := svc.Append(putEntry("project-a", "main", "items", "a2"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), lsnA2, "project-a sequence should continue independently")

	assert.Equal(t, int64(2), svc.GetCurrentLSN("project-a"))
	assert.Equal(t, int64(1), svc.GetCurrentLSN("project-b"))
	assert.Equal(t, int64(0), svc.GetCurrentLSN("project-without-entries"))
}

// TestSequencer_BatchAllocation verifies that batches get contiguous LSN
// ranges and that mixed-project batches are rejected.
func TestSequencer_BatchAllocation(t *testing.T) {
	db := setupTestDB(t)

	svc, err := wal.NewService(db)
	require.NoError(t, err)

	entries := make([]*wal.Entry, 10)
	for i := range entries {
		entries[i] = putEntry("batch-test", "main", "items", fmt.Sprintf("doc-%d", i))
	}

	lsns, err := svc.AppendBatch(entries)
	require.NoError(t, err)
	require.Len(t, lsns, 10)
	for i := 1; i < len(lsns); i++ {
		assert.Equal(t, lsns[i-1]+1, lsns[i], "batch LSNs must be contiguous")
	}

	// A batch spanning two projects cannot be allocated a single contiguous
	// per-project range and must be rejected.
	mixed := []*wal.Entry{
		putEntry("batch-test", "main", "items", "m1"),
		putEntry("other-project", "main", "items", "m2"),
	}
	_, err = svc.AppendBatch(mixed)
	assert.Error(t, err, "mixed-project batches must be rejected")
}

// TestSequencer_EmptyProjectID verifies that entries without a project ID are
// rejected instead of being silently sequenced into a shared bucket.
func TestSequencer_EmptyProjectID(t *testing.T) {
	db := setupTestDB(t)

	svc, err := wal.NewService(db)
	require.NoError(t, err)

	_, err = svc.Append(putEntry("", "main", "items", "x1"))
	assert.Error(t, err, "append without project ID must fail")
}
