package walcli

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/source"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// CheckMergeUnique inspects a pending plan without initializing metadata indexes.
func CheckMergeUnique(ctx context.Context, planID primitive.ObjectID, strategy, uri string) (*merge.UniqueCheckResult, error) {
	if err := merge.ValidateStrategy(strategy); err != nil {
		return nil, err
	}
	metadataURI, name := deploymentFromEnv()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(metadataURI))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Argon metadata: %w", source.RedactError(err, metadataURI))
	}
	defer disconnect(client)
	metadata := client.Database(name, options.Database().SetReadPreference(readpref.Primary()))
	plan, err := merge.ReadPlan(ctx, metadata, planID)
	if err != nil {
		return nil, source.RedactError(err, metadataURI)
	}
	if err := plan.RequirePending(); err != nil {
		return nil, err
	}
	binding, err := source.ReadBinding(ctx, metadata, plan.TargetBranchID)
	if err != nil {
		return nil, source.RedactError(err, metadataURI)
	}
	if err := verifyPlanSource(plan, binding); err != nil {
		return nil, err
	}
	db, err := openSource(ctx, binding, uri)
	if err != nil {
		return nil, err
	}
	defer disconnect(db.Client())
	result, err := plan.CheckUniqueIndexes(ctx, db, strategy)
	return result, source.RedactError(err, uri)
}
