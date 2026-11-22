package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type deploymentCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type deploymentReport struct {
	Version string            `json:"argon_version"`
	Healthy bool              `json:"healthy"`
	Checks  []deploymentCheck `json:"checks"`
	Capture []bson.M          `json:"capture_last_reported,omitempty"`
}

// Deployment checks never print a URI: it may contain service credentials.
func inspectDeployment(ctx context.Context, probe bool) (report deploymentReport) {
	report.Version = rootCmd.Version
	add := func(name, detail string, err error) bool {
		if err != nil {
			detail = err.Error()
		}
		report.Checks = append(report.Checks, deploymentCheck{name, err == nil, detail})
		return err == nil
	}
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetServerSelectionTimeout(5*time.Second))
	if err != nil {
		err = fmt.Errorf("invalid MongoDB connection settings; check MONGODB_URI")
	}
	if !add("connection", "MongoDB reachable", err) {
		return
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	if err := client.Ping(ctx, nil); err != nil {
		report.Checks[0] = deploymentCheck{"connection", false, "MongoDB unreachable or authentication failed; check MONGODB_URI and server availability"}
		return
	}
	var build struct {
		Version string `bson:"version"`
	}
	err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build)
	if err == nil {
		major, parseErr := strconv.Atoi(strings.Split(build.Version, ".")[0])
		if parseErr != nil || major < 6 {
			err = fmt.Errorf("MongoDB 6+ is required for exact change-stream images; found %s", build.Version)
		}
	}
	if !add("version", "MongoDB "+build.Version, err) {
		return
	}
	var hello struct {
		SetName string   `bson:"setName"`
		Primary bool     `bson:"isWritablePrimary"`
		Hosts   []string `bson:"hosts"`
	}
	err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
	if err == nil && (hello.SetName == "" || !hello.Primary) {
		err = fmt.Errorf("connect to a writable replica-set primary; initialize rs0 for local development")
	}
	if !add("replica_set", hello.SetName+" primary", err) {
		return
	}
	if probe {
		err = explainProbeError(probeCapture(ctx, client), hello.Hosts)
		if !add("capture_permissions", "temporary collections, exact images, capture boundary, change stream and transaction verified", err) {
			return
		}
	}
	report.Capture, report.Healthy, err = walcli.InspectCaptureHealth(ctx, client.Database(metadataDatabase()))
	if err != nil {
		add("capture_status", "", err)
	}
	return
}

// explainProbeError adds what to grant when MongoDB refused the capture probe
// for lack of a privilege. The engine's user needs readWriteAnyDatabase and
// dbAdminAnyDatabase (the deployment tests use exactly these): sandboxes are
// databases it creates and drops, and exact images need collMod. On MongoDB
// Atlas, readWriteAnyDatabase alone cannot drop a database.
func explainProbeError(err error, hosts []string) error {
	if err == nil || !walcli.IsUnauthorized(err) {
		return err
	}
	for _, host := range hosts {
		name := strings.ToLower(host)
		if i := strings.LastIndex(name, ":"); i >= 0 {
			name = name[:i]
		}
		if strings.HasSuffix(name, ".mongodb.net") {
			return fmt.Errorf("%w; on MongoDB Atlas, give the engine's database user the built-in roles readWriteAnyDatabase and dbAdminAnyDatabase (Database & Network Access → Database Users → Edit → Specific Privileges), or Atlas admin; see docs/OPERATIONS.md, Authentication", err)
		}
	}
	return fmt.Errorf("%w; grant the engine's MongoDB user readWriteAnyDatabase and dbAdminAnyDatabase; see docs/OPERATIONS.md, Authentication", err)
}

// checkCaptureBoundary is a variable so tests can stand in for a deployment
// whose hello reply has no operationTime.
var checkCaptureBoundary = walcli.CheckCaptureBoundary

func probeCapture(ctx context.Context, client *mongo.Client) (probeErr error) {
	suffix := primitive.NewObjectID().Hex()
	physical := client.Database("argon_doctor_" + suffix)
	metadata := client.Database(metadataDatabase()).Collection("doctor_" + suffix)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := physical.Drop(cleanup); err != nil && probeErr == nil {
			probeErr = fmt.Errorf("remove temporary physical database: %w", err)
		}
		if err := metadata.Drop(cleanup); err != nil && probeErr == nil {
			probeErr = fmt.Errorf("remove temporary metadata collection: %w", err)
		}
	}()
	if err := physical.RunCommand(ctx, bson.D{{Key: "create", Value: "probe"}, {Key: "changeStreamPreAndPostImages", Value: bson.M{"enabled": true}}}).Err(); err != nil {
		return fmt.Errorf("create collection with exact images: %w", err)
	}
	// Checkout stores an operationTime from hello as the point its writes are
	// captured from; a deployment that does not return one passes every other
	// check and then fails at the first checkout.
	if err := checkCaptureBoundary(ctx, physical); err != nil {
		return fmt.Errorf("capture boundary: %w", err)
	}
	stream, err := physical.Watch(ctx, mongo.Pipeline{}, options.ChangeStream().SetFullDocument(options.WhenAvailable).SetFullDocumentBeforeChange(options.WhenAvailable).SetMaxAwaitTime(100*time.Millisecond))
	if err != nil {
		return fmt.Errorf("open change stream: %w", err)
	}
	defer func() { _ = stream.Close(context.Background()) }()
	// Collections must exist before the transaction on all supported versions.
	if _, err := metadata.InsertOne(ctx, bson.M{"_id": "setup"}); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	session, err := client.StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (any, error) {
		if _, err := physical.Collection("probe").InsertOne(sc, bson.M{"_id": "probe", "value": 1}); err != nil {
			return nil, err
		}
		_, err := metadata.InsertOne(sc, bson.M{"_id": "transaction"})
		return nil, err
	})
	if err != nil {
		return fmt.Errorf("cross-database transaction: %w", err)
	}
	if _, err := physical.Collection("probe").UpdateOne(ctx, bson.M{"_id": "probe"}, bson.M{"$set": bson.M{"value": 2}}); err != nil {
		return err
	}
	for stream.Next(ctx) {
		var event struct {
			Operation string   `bson:"operationType"`
			Before    bson.Raw `bson:"fullDocumentBeforeChange"`
			After     bson.Raw `bson:"fullDocument"`
		}
		if err := stream.Decode(&event); err != nil {
			return err
		}
		if event.Operation == "update" {
			if len(event.Before) == 0 || len(event.After) == 0 {
				return fmt.Errorf("exact update images unavailable")
			}
			return nil
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	return fmt.Errorf("capture probe did not observe its update")
}

func runDeploymentCommand(cmd *cobra.Command, probe bool) error {
	duration := 10 * time.Second
	if cmd.Flags().Lookup("timeout") != nil {
		duration, _ = cmd.Flags().GetDuration("timeout")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), duration)
	defer cancel()
	report := inspectDeployment(ctx, probe)
	if jsonOutput(cmd) {
		if err := writeJSON(cmd, report); err != nil {
			return err
		}
	} else {
		for _, check := range report.Checks {
			state := "PASS"
			if !check.OK {
				state = "FAIL"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %-20s %s\n", state, check.Name, check.Detail)
		}
		for _, status := range report.Capture {
			fmt.Fprintf(cmd.OutOrStdout(), "Capture: %s %s", status["branch_id"], status["state"])
			if detail, ok := status["error"].(string); ok && detail != "" {
				fmt.Fprintf(cmd.OutOrStdout(), " (%s)", detail)
			}
			fmt.Fprintln(cmd.OutOrStdout())
		}
	}
	if !report.Healthy {
		return fmt.Errorf("deployment checks failed; resolve the reported issue before writing")
	}
	return nil
}

var doctorCmd = &cobra.Command{
	Use: "doctor", Short: "Verify MongoDB readiness and capture permissions",
	Long: "Check MongoDB version, replica set, exact images, transactions and capture health. Creates and removes uniquely named temporary probe collections. Use status for read-only checks.",
	RunE: func(cmd *cobra.Command, args []string) error { return runDeploymentCommand(cmd, true) },
}

func init() {
	doctorCmd.Flags().Duration("timeout", 10*time.Second, "Maximum time for deployment checks")
	rootCmd.AddCommand(doctorCmd)
}

func metadataDatabase() string {
	if name := os.Getenv("ARGON_METADATA_DB"); name != "" {
		return name
	}
	return "argon_wal"
}
