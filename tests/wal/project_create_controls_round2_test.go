package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Round 2 controls for CreateProject through the import staging: rows of the
// case table that hold on 88d08c9. They pass.

// Control: between the reservation and the publish no reader sees the
// project, at each of the three steps.
func TestRound2_Control_ReadersDoNotSeeAProjectBeingCreated(t *testing.T) {
	for _, step := range []round2Step{round2BeforeRecord, round2BeforeMain, round2BeforePublish} {
		t.Run(step.command+" "+step.collection, func(t *testing.T) {
			db := setupTestDB(t)
			reader, _ := round2Services(t, db.Name(), nil)
			ctx := context.Background()
			name := fmt.Sprintf("round2-hidden-%d", time.Now().UnixNano())

			release := round2HeldCreate(t, db.Name(), name, step)

			// The project row is inserted last, so until then there is
			// nothing of the name for any reader to find.
			n, err := db.Collection("wal_projects").CountDocuments(ctx, bson.M{"name": name})
			require.NoError(t, err)
			require.Zero(t, n)
			_, err = reader.GetProjectByName(name)
			assert.Error(t, err, "by name")
			_, err = reader.GetProjectIDByName(name)
			assert.Error(t, err, "the proxy's lookup")
			listed, err := reader.ListProjects()
			require.NoError(t, err)
			assert.Empty(t, listed, "listing")

			created, err := release()
			require.NoError(t, err)
			readable, err := reader.GetProjectByName(name)
			require.NoError(t, err)
			require.Equal(t, created.ID, readable.ID)
			require.Equal(t, created.MainBranchID, readable.MainBranchID, "a readable project has its main branch")
		})
	}
}

// Control: what CreateProject returns is what it stored.
func TestRound2_Control_ReturnedProjectMatchesTheStoredOne(t *testing.T) {
	db := setupTestDB(t)
	projects, _ := round2Services(t, db.Name(), nil)
	ctx := context.Background()

	created, err := projects.CreateProject("round2-fields")
	require.NoError(t, err)
	stored, err := projects.GetProjectByName("round2-fields")
	require.NoError(t, err)
	assert.Equal(t, created.ID, stored.ID)
	assert.Equal(t, created.Name, stored.Name)
	assert.Equal(t, created.MainBranchID, stored.MainBranchID)
	assert.NotEmpty(t, stored.MainBranchID)
	assert.True(t, stored.UseWAL)
	assert.False(t, created.Importing)
	assert.WithinDuration(t, created.CreatedAt, stored.CreatedAt, time.Millisecond)

	var raw bson.M
	require.NoError(t, db.Collection("wal_projects").FindOne(ctx, bson.M{"_id": created.ID}).Decode(&raw))
	assert.NotContains(t, raw, "importing", "a published project carries no importing field")
	var main wal.Branch
	require.NoError(t, db.Collection("wal_branches").FindOne(ctx, bson.M{"_id": stored.MainBranchID}).Decode(&main))
	assert.Equal(t, created.ID, main.ProjectID)
	assert.Equal(t, "main", main.Name)
}

// Control: a publish that fails after the main branch was made removes the
// reservation, the branch, both records and the counter, and frees the name.
// The failure is a validator that accepts the reservation and refuses a
// project with a main branch.
func TestRound2_Control_FailedPublishLeavesNothing(t *testing.T) {
	db := setupTestDB(t)
	projects, _ := round2Services(t, db.Name(), nil)
	ctx := context.Background()

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "wal_projects"},
		{Key: "validator", Value: bson.M{"main_branch_id": ""}},
	}).Err())
	_, err := projects.CreateProject("round2-publish-fails")
	require.Error(t, err)
	t.Logf("create with a failing publish: %v", err)
	assert.NotContains(t, err.Error(), "cleanup incomplete")
	for _, collection := range []string{"wal_projects", "wal_branches", "wal_log", "wal_counters"} {
		n, countErr := db.Collection(collection).CountDocuments(ctx, bson.M{})
		require.NoError(t, countErr)
		assert.Zero(t, n, "%s after a failed create", collection)
	}

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "wal_projects"},
		{Key: "validator", Value: bson.M{}},
	}).Err())
	_, err = projects.CreateProject("round2-publish-fails")
	require.NoError(t, err, "the name is free again")
}

// Control: a failed create does not touch a published project.
func TestRound2_Control_FailedCreateKeepsOtherProjects(t *testing.T) {
	db := setupTestDB(t)
	projects, _ := round2Services(t, db.Name(), nil)
	ctx := context.Background()

	kept, err := projects.CreateProject("round2-kept")
	require.NoError(t, err)
	_, err = projects.CreateProject("round2-kept")
	require.ErrorIs(t, err, wal.ErrProjectExists)
	require.Error(t, projects.AbortImport(ctx, kept.ID), "AbortImport refuses a published project")
	require.Error(t, projects.CleanupImport(ctx, "round2-kept"), "CleanupImport refuses a published project")
	stored, err := projects.GetProject(kept.ID)
	require.NoError(t, err)
	require.Equal(t, kept.MainBranchID, stored.MainBranchID)
	n, err := db.Collection("wal_branches").CountDocuments(ctx, bson.M{"project_id": kept.ID, "is_deleted": false})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

// Control: delete a project, then create one of the same name.
func TestRound2_Control_DeleteThenCreateTheSameName(t *testing.T) {
	db := setupTestDB(t)
	projects, _ := round2Services(t, db.Name(), nil)

	first, err := projects.CreateProject("round2-again")
	require.NoError(t, err)
	require.NoError(t, projects.DeleteProject(first.ID))
	second, err := projects.CreateProject("round2-again")
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	require.NotEqual(t, first.MainBranchID, second.MainBranchID)
	stored, err := projects.GetProjectByName("round2-again")
	require.NoError(t, err)
	require.Equal(t, second.ID, stored.ID)
}

// Control: an import and a create of one name. Whichever reserved the name
// first keeps it; the other fails and leaves nothing.
func TestRound2_Control_CreateAgainstAnImportInProgress(t *testing.T) {
	_, projects, meta, _ := importRecoveryFixture(t)
	ctx := context.Background()

	staged, err := projects.BeginImport(ctx, "round2-shared")
	require.NoError(t, err)
	_, err = projects.CreateProject("round2-shared")
	require.Error(t, err, "the import holds the name")
	require.NotContains(t, err.Error(), "E11000")
	require.NotErrorIs(t, err, wal.ErrProjectExists, "no project of the name can be read yet")
	require.Contains(t, err.Error(), "import cleanup", "the error says what holds the name and how to release it")
	n, err := meta.Collection("wal_branches").CountDocuments(ctx, bson.M{"project_id": bson.M{"$ne": staged.ID}})
	require.NoError(t, err)
	require.Zero(t, n, "the losing create made no branch")
	require.NoError(t, projects.CompleteImport(ctx, staged.ID))
	published, err := projects.GetProjectByName("round2-shared")
	require.NoError(t, err)
	require.Equal(t, staged.ID, published.ID)
}

// Control: the same failed publish through the assembled services, whose
// abort hook also forgets the main branch's snapshots and index records.
func TestRound2_Control_FailedPublishThroughServicesLeavesNothing(t *testing.T) {
	db := setupTestDB(t)
	services, err := walcli.NewServicesAt(getTestMongoURI(), db.Name())
	require.NoError(t, err)
	t.Cleanup(func() { _ = services.Client.Disconnect(context.Background()) })
	ctx := context.Background()

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "wal_projects"},
		{Key: "validator", Value: bson.M{"main_branch_id": ""}},
	}).Err())
	_, err = services.Projects.CreateProject("round2-publish-fails")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "cleanup incomplete")
	assert.NotContains(t, err.Error(), "snapshots were not")
	for _, collection := range []string{"wal_projects", "wal_branches", "wal_log", "wal_counters"} {
		n, countErr := db.Collection(collection).CountDocuments(ctx, bson.M{})
		require.NoError(t, countErr)
		assert.Zero(t, n, "%s after a failed create", collection)
	}
}
