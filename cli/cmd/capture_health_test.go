package cmd

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestCLIDeploymentStatusUsesSourceHealth(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to run deployment checks")
	}
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	t.Setenv("ARGON_METADATA_DB", "argon_cli_test_"+primitive.NewObjectID().Hex())
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	metadata := client.Database(metadataDatabase())
	t.Cleanup(func() {
		if err := metadata.Drop(ctx); err != nil {
			t.Error(err)
		}
		if err := client.Disconnect(ctx); err != nil {
			t.Error(err)
		}
	})

	const branchID = "main-branch"
	const projectID = "shop-project"
	_, err = metadata.Collection("wal_projects").InsertOne(ctx, bson.M{"_id": projectID})
	if err != nil {
		t.Fatal(err)
	}
	at := primitive.Timestamp{T: 1700000000, I: 1}
	for _, tt := range []struct {
		name, storedState, state, detail, flag string
		head                                   int64
		physicalDB                             string
		missingToken                           bool
	}{
		{name: "healthy stopped branch", head: 10, storedState: "stopped", state: "stopped"},
		{name: "unfinished resync", head: 10, storedState: "resyncing", state: "resyncing", detail: "has not finished", flag: "resync_unfinished", missingToken: true},
		{name: "checked out branch", head: 10, storedState: "stopped", state: "degraded", detail: "checked out", physicalDB: "older_checkout"},
		{name: "missing position", head: 10, storedState: "stopped", state: "degraded", detail: "saved source position", missingToken: true},
		{name: "expired lease", head: 10, storedState: "running", state: "stopped", detail: "last renewed its lease", flag: "lease_expired"},
		{name: "unexpected head change", head: 11, storedState: "stopped", state: "degraded", detail: "without the follower"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := metadata.Collection("wal_branches").ReplaceOne(ctx, bson.M{"_id": branchID}, bson.M{
				"_id": branchID, "project_id": projectID, "name": "main", "head_lsn": tt.head, "is_deleted": false, "follows": "shop", "physical_db": tt.physicalDB,
			}, options.Replace().SetUpsert(true))
			if err != nil {
				t.Fatal(err)
			}
			state := bson.M{"database": "shop", "mode": "exact", "position": at,
				"consistent": bson.M{"lsn": int64(10), "cluster_time": at}}
			stored := bson.M{"branch_id": branchID, "state": tt.storedState, "head_lsn": int64(10), "source": state}
			doc := bson.M{"_id": branchID, "capture_status": stored}
			if !tt.missingToken {
				doc["resume_token"] = bson.M{"_data": "826553F10000000001"}
			}
			if tt.flag == "resync_unfinished" {
				now := time.Now()
				state["mode"] = "catch_up"
				state["resync"] = bson.M{"started_at": now}
				stored["owner"] = bson.M{"kind": "resync", "host": "current-owner", "pid": 92, "heartbeat_at": now}
			}
			if tt.flag == "lease_expired" {
				stored["owner"] = bson.M{"kind": "follow", "host": "previous-owner", "pid": 91, "heartbeat_at": time.Unix(1, 0)}
			}
			_, err = metadata.Collection("wal_ingest_state").ReplaceOne(ctx, bson.M{"_id": branchID}, doc, options.Replace().SetUpsert(true))
			if err != nil {
				t.Fatal(err)
			}

			commands := []string{"status"}
			if tt.flag == "resync_unfinished" {
				commands = append(commands, "doctor")
			}
			for _, command := range commands {
				stdout, stderr, err := invokeCLI(t, uri, command, "--output", "json")
				var report deploymentReport
				if decodeErr := json.Unmarshal([]byte(stdout), &report); decodeErr != nil {
					t.Fatalf("%s printed invalid JSON: %v (%s)", command, decodeErr, stderr)
				}
				if len(report.Capture) != 1 {
					t.Fatalf("%s returned %d capture records (%s)", command, len(report.Capture), stderr)
				}
				capture := report.Capture[0]
				if capture["branch_id"] != branchID || capture["state"] != tt.state {
					t.Errorf("%s expected capture state %q for %s, got %v", command, tt.state, branchID, capture)
				}
				if tt.flag != "" && capture[tt.flag] != true {
					t.Errorf("%s omitted %s: %v", command, tt.flag, capture)
				}
				wantHealthy := tt.detail == ""
				if report.Healthy != wantHealthy || (err == nil) != wantHealthy {
					t.Errorf("%s expected healthy=%t, got healthy=%t and exit %v (%s)", command, wantHealthy, report.Healthy, err, stderr)
				}
				if detail, _ := capture["error"].(string); !strings.Contains(detail, tt.detail) {
					t.Errorf("%s expected error containing %q, got %q", command, tt.detail, detail)
				}
			}
		})
	}
	t.Run("ordinary capture during an import", func(t *testing.T) {
		_, err := metadata.Collection("wal_projects").UpdateByID(ctx, projectID, bson.M{"$set": bson.M{"importing": true}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = metadata.Collection("wal_branches").UpdateByID(ctx, branchID, bson.M{"$unset": bson.M{"follows": ""}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = metadata.Collection("wal_ingest_state").UpdateByID(ctx, branchID, bson.M{"$set": bson.M{"capture_status.state": "degraded"}})
		if err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := invokeCLI(t, uri, "status", "--output", "json")
		var report deploymentReport
		if err != nil {
			t.Fatalf("status while importing: %v (%s)", err, stderr)
		}
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatal(err)
		}
		if !report.Healthy || len(report.Capture) != 0 {
			t.Fatalf("unpublished capture affected deployment health: %+v", report)
		}
	})

}
