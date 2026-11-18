package walcli

import (
	"context"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// HealthError reports a problem with the source position or its owner.
func (info *SourceInfo) HealthError() error {
	return info.Health.HealthError()
}

// InspectCaptureHealth reads saved capture health without initializing metadata.
func InspectCaptureHealth(ctx context.Context, metadata *mongo.Database) ([]bson.M, bool, error) {
	reports, err := ingest.ReadCaptureReports(ctx, metadata)
	if err != nil {
		return nil, false, err
	}
	var capture []bson.M
	healthy := true
	for _, report := range reports {
		status, fields := report.Status, report.Fields
		fields["branch_id"], fields["state"] = status.BranchID, status.State
		unfinished := status.ResyncUnfinished()
		if unfinished {
			fields["resync_unfinished"] = true
		}
		if status.SourceLeaseExpired {
			fields["lease_expired"] = true
		}
		healthy = healthy && report.Healthy
		if status.Health != nil {
			fields["health"] = *status.Health
		}
		if status.Error != "" {
			fields["error"] = status.Error
		}
		capture = append(capture, fields)
	}
	return capture, healthy, nil
}
