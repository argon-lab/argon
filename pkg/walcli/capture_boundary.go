package walcli

import (
	"context"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"go.mongodb.org/mongo-driver/mongo"
)

// CheckCaptureBoundary verifies that a checkout in db could take its capture
// boundary: the hello reply must carry an operationTime. It runs the same call
// as checkout and changes nothing.
func CheckCaptureBoundary(ctx context.Context, db *mongo.Database) error {
	_, err := checkout.ReadCaptureBoundary(ctx, db)
	return err
}
