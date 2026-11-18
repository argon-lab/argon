package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func sourceStreamOptions() *options.ChangeStreamOptions {
	return options.ChangeStream().
		SetBatchSize(maxBatch).
		SetFullDocument(options.WhenAvailable).
		SetFullDocumentBeforeChange(options.WhenAvailable).
		SetShowExpandedEvents(true).
		SetMaxAwaitTime(500 * time.Millisecond)
}

func watchSource(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
	return src.Watch(ctx, mongo.Pipeline{}, opts)
}

// ProbeStream checks that a source can open and read the stream that will
// replay changes made during the copy.
func ProbeStream(ctx context.Context, src *mongo.Database, startAt primitive.Timestamp) error {
	return probeStream(ctx, src, startAt, watchSource)
}

func probeStream(ctx context.Context, src *mongo.Database, startAt primitive.Timestamp, watch func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error)) error {
	if watch == nil {
		watch = watchSource
	}
	stream, err := watch(ctx, src, sourceStreamOptions().SetStartAtOperationTime(&startAt))
	if err != nil {
		if unsupportedSourceStreamOptions(err) {
			return fmt.Errorf("the source's change stream cannot be opened: the source's MongoDB version does not support the change-stream options Argon follows with: %w", err)
		}
		return fmt.Errorf("the source's change stream cannot be opened (the source user needs the changeStream action on database %s): %w", src.Name(), err)
	}
	defer func() { _ = stream.Close(context.Background()) }()

	now, err := primaryTime(ctx, src)
	if err != nil {
		return err
	}
	if err := validateSourceStreamToken(stream, now); err != nil {
		return err
	}
	stream.TryNext(ctx)
	if err := stream.Err(); err != nil {
		return fmt.Errorf("the source's change stream cannot be read: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return validateSourceStreamToken(stream, now)
}

func unsupportedSourceStreamOptions(err error) bool {
	var command mongo.CommandError
	return errors.As(err, &command) && (command.Code == 40415 || command.Code == 9)
}

func validateSourceStreamToken(stream *mongo.ChangeStream, now primitive.Timestamp) error {
	// Check that this server's tokens describe the stream position before
	// relying on them. The initial token may be absent until the first read.
	token := stream.ResumeToken()
	if at, ok := tokenTime(token); len(token) > 0 && (!ok || at.T < 1_000_000_000 || at.T > now.T+600) {
		return &ConfigurationError{"this MongoDB version's change-stream resume token is not in the format Argon reads the stream position from; following is not supported on it"}
	}
	return nil
}
