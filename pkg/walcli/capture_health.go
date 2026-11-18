package walcli

import (
	"context"
	"errors"
	"fmt"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// HealthError reports a problem with the source position or its owner.
func (info *SourceInfo) HealthError() error {
	return captureHealthError(info.Project, info.State, info.Error, info.ResyncUnfinished, info.LeaseExpired)
}

func captureHealthError(project, state, detail string, unfinished, expired bool) error {
	switch {
	case unfinished && project != "":
		return fmt.Errorf("a resync of main has not finished. If it was interrupted, run `argon source resync -p %s` again", project)
	case unfinished:
		return errors.New("a resync of the followed branch has not finished. If it was interrupted, run `argon source resync` again")
	case state == "degraded":
		if detail == "" {
			return errors.New("capture is degraded")
		}
		return fmt.Errorf("following is degraded: %s", detail)
	case expired:
		return fmt.Errorf("following is stopped: %s", detail)
	default:
		return nil
	}
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
		detail := status.Error
		if problem := captureHealthError("", status.State, detail, unfinished, status.SourceLeaseExpired); problem != nil {
			healthy = false
			if detail == "" {
				detail = problem.Error()
			}
		}
		if detail != "" {
			fields["error"] = detail
		}
		capture = append(capture, fields)
	}
	return capture, healthy, nil
}
