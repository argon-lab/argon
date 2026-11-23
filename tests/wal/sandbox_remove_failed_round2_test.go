package wal_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// round2Link is a TCP path to the test deployment that can be cut: every
// connection through it is closed and no new one is accepted. An engine
// process connected through it loses its deployment the way it does when the
// network between them fails.
type round2Link struct {
	listener net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	cut      bool
}

func startRound2Link(t *testing.T, upstream string) *round2Link {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	link := &round2Link{listener: listener}
	t.Cleanup(link.Cut)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				continue
			}
			link.mu.Lock()
			if link.cut {
				link.mu.Unlock()
				_ = client.Close()
				_ = server.Close()
				continue
			}
			link.conns = append(link.conns, client, server)
			link.mu.Unlock()
			go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
			go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
		}
	}()
	return link
}

func (l *round2Link) Cut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cut {
		return
	}
	l.cut = true
	_ = l.listener.Close()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
}

// Round 2: removeFailed now asks GetBranchByID whether the failed create's
// branch is still there, and takes ANY error from it as "already removed;
// nothing of this create is left". GetBranchByID returns the driver's error
// as it is, so a deployment that cannot be reached reads the same as a branch
// that was deleted: removeFailed returns the cause alone and says nothing of
// the branch it left behind.
//
// Before 88d08c9 the same failure was reported: "...; the sandbox branch
// "run-1" was left behind and could not be removed (...): remove it with
// argon sandbox discard -b run-1". That sentence is what tells the caller
// why the retry under the same name answers "branch already exists".
func TestRound2_FailedCreateSaysWhenItCouldNotRemoveItsBranch(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("round2-remove-failed-%d", time.Now().UnixNano())

	// The engine process reaches the deployment through a link that fails
	// while its create is copying the sandbox's data.
	link := startRound2Link(t, "localhost:27017")
	uri := fmt.Sprintf("mongodb://%s/?directConnection=true&serverSelectionTimeoutMS=500&connectTimeoutMS=500", link.listener.Addr())
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	engine := newReviewProcess(t, client.Database(db.Name()))

	main, err := engine.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	observer := newReviewProcess(t, db) // a healthy connection, to see what is left
	writer := walwriter.New(walService, observer.branches, materializer.NewService(walService, observer.branches), main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "seed"})
	require.NoError(t, err)
	main, err = observer.branches.GetBranchByID(main.ID)
	require.NoError(t, err)

	engine.checkout.SetDuringLoad(func(context.Context, string) func(context.Context) {
		link.Cut()
		return nil
	})

	_, createErr := engine.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
	require.Error(t, createErr, "the create cannot finish without its deployment")
	t.Logf("create returned: %v", createErr)

	left, err := observer.branches.GetBranch(project, "run-1")
	require.NoError(t, err, "the control: the failed create's branch is still there")
	t.Cleanup(func() { _ = db.Client().Database(wal.PhysicalDBName(left.ID)).Drop(context.Background()) })
	_, retryErr := observer.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
	require.Error(t, retryErr, "the control: the name is still taken, so the retry is refused")

	assert.Contains(t, createErr.Error(), "argon sandbox discard -b run-1",
		"the create left branch %s (%q) behind and its error does not say so", left.ID, left.Name)
}
