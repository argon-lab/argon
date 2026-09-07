package ingest

import (
	"context"
	"errors"
	"fmt"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"testing"
	"time"
)

func TestImageSetupIndexBuildIsRetryable(t *testing.T) {
	busy := mongo.CommandError{Name: "BackgroundOperationInProgressForNamespace", Code: 12587, Message: "index build running"}
	err := imageSetupError(fmt.Errorf("enable exact images: %w", busy))
	var gap *CaptureError
	if errors.As(err, &gap) {
		t.Fatal("temporary index build incorrectly marks permanent capture gap")
	}
	var command mongo.CommandError
	if !errors.As(err, &command) {
		t.Fatal("retry lost original MongoDB error")
	}
	denied := imageSetupError(mongo.CommandError{Name: "Unauthorized", Code: 13})
	if !errors.As(denied, &gap) {
		t.Fatal("permission failure must be visible as degraded capture")
	}
}

func TestCancellationKeepsOnlyCompleteTransactionPrefix(t *testing.T) {
	last := bson.Raw{2}
	before := bson.Raw{1}
	batch := []*wal.Entry{{TxnID: "finished"}, {TxnID: "finished"}, {TxnID: "pending"}, {TxnID: "pending"}}
	complete, token := completePrefix(batch, last, before)
	if len(complete) != 2 || token[0] != 1 {
		t.Fatalf("partial transaction would be checkpointed: %d %v", len(complete), token)
	}
	complete, token = completePrefix([]*wal.Entry{{}}, last, before)
	if len(complete) != 1 || token[0] != 2 {
		t.Fatal("non-transactional write must remain durable")
	}
}

func TestStartWaitsForPersistedActorResolution(t *testing.T) {
	run := &running{ready: make(chan struct{}), done: make(chan struct{})}
	service := &Service{runs: map[string]*running{"branch": run}, statuses: map[string]Status{"branch": {State: "starting"}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- service.start(ctx, "branch", WithActor("agent:existing")) }()
	select {
	case err := <-result:
		t.Fatalf("returned before stored actor was known: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	service.mu.Lock()
	run.actor = "agent:existing"
	service.statuses["branch"] = Status{State: "running", Actor: run.actor}
	close(run.ready)
	service.mu.Unlock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
