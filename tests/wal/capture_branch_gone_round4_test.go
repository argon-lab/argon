package wal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Round 4 (0e6aae9, branchGone: "matches ... the driver's
// mongo.ErrNoDocuments, which is what lookups by ID returned as it was and
// what existing callers test for").
//
// It matches through errors.Is. One existing caller does not use errors.Is:
// ingest.runStream reads the branch with GetBranchByIDContext and tests
//
//	if err == mongo.ErrNoDocuments {
//		return &ConfigurationError{"branch ... not found"}
//	}
//	return fmt.Errorf("branch %s not found: %w", branchID, err)
//
// The comparison is false for a branchGone, so a branch that is gone is no
// longer a ConfigurationError. That type is what tells the capture
// supervisor (ingest.execute) that retrying cannot help. Without it the
// supervisor reports "retrying" and starts over, for ever: RunCapture
// (`argon watch`) on a branch that is gone by the time capture reads it used
// to end with "branch <id> not found" and now runs until it is killed,
// writing a capture_status row for a branch that does not exist every few
// seconds. StartCapture (the REST server, argon_sandbox_create, argon
// sandbox create) waits out its whole deadline and answers "waiting for
// capture readiness: context deadline exceeded" instead of saying the branch
// is gone.
//
// (A watch whose branch is discarded while its stream is open still ends:
// the drop of the database reaches it first, as a CaptureError. This is the
// path of a capture that starts, or starts over after a transient error,
// once the branch is gone.)
func TestRound4_CaptureOfABranchThatIsGoneStopsInsteadOfRetryingForEver(t *testing.T) {
	f := newIngestFixture(t, "round4-capture-gone")
	gone := primitive.NewObjectID().Hex()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	err := f.ingest.Run(ctx, gone)
	took := time.Since(started)
	t.Logf("Run on a branch that does not exist returned after %s: %v", took.Round(10*time.Millisecond), err)

	var doc struct {
		Status ingest.Status `bson:"capture_status"`
	}
	_ = f.metaDB.Collection("wal_ingest_state").FindOne(context.Background(), bson.M{"_id": gone}).Decode(&doc)
	t.Logf("capture_status stored for it: state=%q error=%q", doc.Status.State, doc.Status.Error)

	var configuration *ingest.ConfigurationError
	require.True(t, errors.As(err, &configuration),
		"capture of a branch that does not exist must end with a ConfigurationError; "+
			"it ran until the test's deadline (%s) and returned %v, last reporting state %q: %q",
		took.Round(10*time.Millisecond), err, doc.Status.State, doc.Status.Error)
	assert.Contains(t, configuration.Error(), "not found")
	assert.Less(t, took, 2*time.Second, "nothing to wait for")
}

// The same from the managed entry point, which is what the REST server and
// the MCP tools call (walcli.StartCapture): the caller's deadline is spent
// retrying, and the answer does not say the branch is gone.
func TestRound4_StartingCaptureOfABranchThatIsGoneSaysSo(t *testing.T) {
	f := newIngestFixture(t, "round4-capture-start-gone")
	gone := primitive.NewObjectID().Hex()
	t.Cleanup(func() { _ = f.ingest.Stop(context.Background(), gone) })

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	err := f.ingest.Start(ctx, gone)
	took := time.Since(started)
	t.Logf("Start on a branch that does not exist returned after %s: %v", took.Round(10*time.Millisecond), err)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found",
		"the branch does not exist; after %s the caller is told: %v", took.Round(10*time.Millisecond), err)
	assert.Less(t, took, 2*time.Second, "nothing to wait for")
}
