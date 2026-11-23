package wal_test

import (
	"context"
	"errors"
	"testing"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Round 5 (4c24144: "Leave branch lookups by ID returning the driver's
// error, which capture compares against", and the sentinels by name).
//
// The contract of each lookup for a branch or project that is not there, in
// one place. The kept tests reach these through their callers (capture, the
// proxy, removeFailed); none looks at GetBranchByIDAny, whose error no
// caller tests today, or at the identity of what the lookups by ID return:
// callers compare with ==, so a type that only matches through errors.Is
// (0e6aae9's branchGone) is not the same answer.
func TestRound5_WhatEachLookupReturnsForABranchOrProjectThatIsNotThere(t *testing.T) {
	db := setupTestDB(t)
	process := newReviewProcess(t, db)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	projects, err := projectwal.NewProjectService(db, walService, process.branches)
	require.NoError(t, err)
	gone := primitive.NewObjectID().Hex()

	t.Run("branch by ID: the driver's error itself", func(t *testing.T) {
		_, err := process.branches.GetBranchByID(gone)
		assert.True(t, err == mongo.ErrNoDocuments, "GetBranchByID returned %T %v", err, err)
		assert.False(t, errors.Is(err, wal.ErrBranchNotFound), "the lookup by ID is not the sentinel: callers such as the proxy's cache check tell them apart")

		_, err = process.branches.GetBranchByIDContext(context.Background(), gone)
		assert.True(t, err == mongo.ErrNoDocuments, "GetBranchByIDContext returned %T %v", err, err)
	})

	t.Run("branch by ID, deleted or not: the driver's error itself", func(t *testing.T) {
		_, err := process.branches.GetBranchByIDAny(gone)
		assert.True(t, err == mongo.ErrNoDocuments, "GetBranchByIDAny returned %T %v", err, err)

		_, err = process.branches.GetBranchByIDAnyContext(context.Background(), gone)
		assert.True(t, err == mongo.ErrNoDocuments, "GetBranchByIDAnyContext returned %T %v", err, err)
	})

	t.Run("branch by name: the sentinel itself", func(t *testing.T) {
		_, err := process.branches.GetBranch("round5-no-project", "no-such-branch")
		assert.True(t, err == wal.ErrBranchNotFound, "GetBranch returned %T %v", err, err)
		assert.True(t, err == branchwal.ErrBranchNotFound)
		assert.False(t, errors.Is(err, mongo.ErrNoDocuments))
		assert.EqualError(t, err, "branch not found", "the text master returned")
	})

	t.Run("delete by ID: the sentinel", func(t *testing.T) {
		err := process.branches.DeleteBranchByID(gone)
		assert.True(t, err == wal.ErrBranchNotFound, "DeleteBranchByID returned %T %v", err, err)
	})

	t.Run("NotFound maps only the driver's error", func(t *testing.T) {
		assert.True(t, branchwal.NotFound(mongo.ErrNoDocuments) == wal.ErrBranchNotFound)
		other := errors.New("server selection error")
		assert.True(t, branchwal.NotFound(other) == other)
		assert.NoError(t, branchwal.NotFound(nil))
	})

	t.Run("project by name and by ID: the sentinel itself", func(t *testing.T) {
		_, err := projects.GetProjectByName("round5-no-such-project")
		assert.True(t, err == wal.ErrProjectNotFound, "GetProjectByName returned %T %v", err, err)
		assert.EqualError(t, err, "project not found", "the text master returned")

		_, err = projects.GetProjectIDByName("round5-no-such-project")
		assert.True(t, err == wal.ErrProjectNotFound, "GetProjectIDByName returned %T %v", err, err)

		_, err = projects.GetProject(gone)
		assert.True(t, err == wal.ErrProjectNotFound, "GetProject returned %T %v", err, err)
	})
}
