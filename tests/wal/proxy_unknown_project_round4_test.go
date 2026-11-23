package wal_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/wireproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Round 4 (0e6aae9, wireproxy.lookup: "if errors.Is(err,
// wal.ErrProjectNotFound) { project %q not found }", everything else is
// errMetadataUnavailable).
//
// The proxy's project lookup is ProjectService.GetProjectIDByName, which is
// GetProjectByName. 0e6aae9 gave wal.ErrProjectNotFound to GetProject (the
// lookup by ID) and left GetProjectByName returning its own
// errors.New("project not found"). Only the proxy's test double was changed
// to return the sentinel. So with the real service a project that does not
// exist never matches, and the proxy answers
//
//	(HostUnreachable) argon proxy: argon's metadata is unavailable: cannot
//	look up project "nosuch": project not found
//
// where 6aa0f7a and master answer (NamespaceNotFound) project "nosuch" not
// found. A mistyped project name, or a project that was deleted, is reported
// to the client as an outage of Argon's metadata, with a code drivers retry,
// and the answer is never remembered, so every such command (and the
// driver's retry of it) reads the metadata store again.
func TestRound4_AProjectThatDoesNotExistIsNotAMetadataOutage(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	walService, err := wal.NewService(db)
	require.NoError(t, err)
	process := newReviewProcess(t, db)
	projects, err := projectwal.NewProjectService(db, walService, process.branches)
	require.NoError(t, err)

	// The control: a project that exists, so the lookup itself works.
	name := fmt.Sprintf("round4-proxy-%d", time.Now().UnixNano())
	_, err = projects.CreateProject(name)
	require.NoError(t, err)

	addr := startProxy(t, wireproxy.New("localhost:27017", projects, process.branches))
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(fmt.Sprintf("mongodb://%s/?directConnection=true", addr)))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(context.Background()) }()

	// The metadata store is up and answering. The project is simply not there.
	alias := name + "-nosuch~main"
	findErr := client.Database(alias).Collection("docs").FindOne(ctx, bson.M{}).Err()
	require.Error(t, findErr)
	t.Logf("FindOne on a project that does not exist: %v", findErr)

	var command mongo.CommandError
	require.True(t, errors.As(findErr, &command), "want a command error, got %T: %v", findErr, findErr)
	assert.Equal(t, "NamespaceNotFound", command.Name,
		"the project does not exist and the metadata store answered; the client is told (code %d) %s", command.Code, command.Message)
	assert.NotContains(t, command.Message, "metadata is unavailable",
		"nothing was unavailable")

	// A branch that does not exist in a project that does is still told apart.
	branchErr := client.Database(name+"~nosuch").Collection("docs").FindOne(ctx, bson.M{}).Err()
	require.True(t, errors.As(branchErr, &command), "want a command error, got %T: %v", branchErr, branchErr)
	assert.Equal(t, "NamespaceNotFound", command.Name, "the control: a missing branch, %s", command.Message)
}

// The cause, at the service: the lookup by name that the proxy calls does not
// return the sentinel the proxy tests for. (GetProject, by ID, does.)
func TestRound4_TheProjectLookupByNameReturnsTheSentinelTheProxyTestsFor(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	process := newReviewProcess(t, db)
	projects, err := projectwal.NewProjectService(db, walService, process.branches)
	require.NoError(t, err)

	_, byID := projects.GetProject("round4-nosuch-id")
	assert.ErrorIs(t, byID, wal.ErrProjectNotFound, "the control: the lookup by ID")

	_, byName := projects.GetProjectByName("round4-nosuch-name")
	require.Error(t, byName)
	assert.ErrorIs(t, byName, wal.ErrProjectNotFound, "GetProjectByName")

	_, idByName := projects.GetProjectIDByName("round4-nosuch-name")
	require.Error(t, idByName)
	assert.ErrorIs(t, idByName, wal.ErrProjectNotFound,
		"GetProjectIDByName is wireproxy.ProjectLookup: the proxy tells a missing project from a failed read by this sentinel")
}
