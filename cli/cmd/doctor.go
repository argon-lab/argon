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
	// On Atlas the free and Flex tiers cap databases, collections and data,
	// and each checkout adds a database; the tier itself cannot be read
	// from the database, so the usage is shown next to the free tier's caps.
	if isAtlasDeployment(hello.Hosts) {
		detail := "usage unavailable"
		if dbs, colls, bytes, usageErr := atlasUsage(ctx, client); usageErr == nil {
			detail = formatAtlasUsage(dbs, colls, bytes)
		} else {
			detail += ": " + usageErr.Error()
		}
		add("atlas_usage", detail, nil)
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
	if isAtlasDeployment(hosts) {
		return fmt.Errorf("%w; on MongoDB Atlas, give the engine's database user the built-in roles readWriteAnyDatabase and dbAdminAnyDatabase (Database & Network Access → Database Users → Edit → Specific Privileges), or Atlas admin; see docs/OPERATIONS.md, Authentication", err)
	}
	return fmt.Errorf("%w; grant the engine's MongoDB user readWriteAnyDatabase and dbAdminAnyDatabase; see docs/OPERATIONS.md, Authentication", err)
}

// isAtlasDeployment reports whether hello's hosts are MongoDB Atlas hosts.
func isAtlasDeployment(hosts []string) bool {
	for _, host := range hosts {
		name := strings.ToLower(host)
		if i := strings.LastIndex(name, ":"); i >= 0 {
			name = name[:i]
		}
		if strings.HasSuffix(name, ".mongodb.net") {
			return true
		}
	}
	return false
}

// atlasUsage counts what Atlas's free-tier limits count: user databases,
// their collections, and data size as Atlas defines it (uncompressed BSON
// plus indexes).
func atlasUsage(ctx context.Context, client *mongo.Client) (databases, collections int, bytes int64, err error) {
	names, err := client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return 0, 0, 0, err
	}
	for _, name := range names {
		if name == "admin" || name == "local" || name == "config" {
			continue
		}
		var stats struct {
			Collections int     `bson:"collections"`
			DataSize    float64 `bson:"dataSize"`
			IndexSize   float64 `bson:"indexSize"`
		}
		if err := client.Database(name).RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&stats); err != nil {
			return 0, 0, 0, fmt.Errorf("dbStats on %s: %w", name, err)
		}
		databases++
		collections += stats.Collections
		bytes += int64(stats.DataSize + stats.IndexSize)
	}
	return databases, collections, bytes, nil
}

func formatAtlasUsage(databases, collections int, bytes int64) string {
	return fmt.Sprintf("%d databases, %d collections, %.1f MB of data and indexes (the free M0 allows 100 databases, 500 collections and 0.5 GB)", databases, collections, float64(bytes)/1e6)
}

// probeCleanupStep bounds each step of the probe's cleanup.
const probeCleanupStep = 5 * time.Second

// dropProbeCollections removes every collection of the probe's own database,
// for a user that is refused dropDatabase.
func dropProbeCollections(ctx context.Context, physical *mongo.Database) error {
	if !strings.HasPrefix(physical.Name(), "argon_doctor_") {
		return fmt.Errorf("%s is not a probe database", physical.Name())
	}
	names, err := physical.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := physical.Collection(name).Drop(ctx); err != nil {
			return err
		}
	}
	return nil
}

// checkCaptureBoundary is a variable so tests can stand in for a deployment
// whose hello reply has no operationTime.
var checkCaptureBoundary = walcli.CheckCaptureBoundary

func probeCapture(ctx context.Context, client *mongo.Client) (probeErr error) {
	suffix := primitive.NewObjectID().Hex()
	physical := client.Database("argon_doctor_" + suffix)
	metadata := client.Database(metadataDatabase()).Collection("doctor_" + suffix)
	// What the probe may have created, so that cleanup neither skips it nor
	// reports something that never existed. Each is set before the command
	// that creates it: a command that failed may still have taken effect.
	physicalMayExist, metadataMayExist := true, false
	defer func() {
		// Each step has its own time, so one that stalls does not take the
		// others' away, and the two drops run side by side, so a deployment
		// that has stopped answering holds the command for one step's time
		// rather than one after another.
		step := func(run func(context.Context) error) error {
			cleanup, cancel := context.WithTimeout(context.Background(), probeCleanupStep)
			defer cancel()
			return run(cleanup)
		}
		metadataDropped := make(chan error, 1)
		if metadataMayExist {
			go func() {
				metadataDropped <- step(func(c context.Context) error { return metadata.Drop(c) })
			}()
		} else {
			metadataDropped <- nil
		}
		var left []string
		if physicalMayExist {
			// Called by name, not passed as a value, so the audit of drop
			// sites (TestSource_DestructiveCallsAreConfined) sees it.
			if err := step(func(c context.Context) error { return physical.Drop(c) }); err != nil {
				if walcli.IsUnauthorized(err) {
					// A user that may not drop databases may still drop the
					// collections it created, and a database with no
					// collections is gone. The refusal is still the probe's
					// result: the engine drops a database for every sandbox
					// and checkout it removes.
					if emptyErr := step(func(c context.Context) error { return dropProbeCollections(c, physical) }); emptyErr != nil {
						left = append(left, fmt.Sprintf("the probe's database %s could not be emptied either (%v): drop it by hand", physical.Name(), emptyErr))
					}
				} else {
					// Not a refusal (a timeout, a lost connection): dropping
					// its collections would fail the same way.
					left = append(left, fmt.Sprintf("the probe's database %s may be left behind (%v): drop it by hand", physical.Name(), err))
				}
				if probeErr == nil {
					probeErr = fmt.Errorf("remove temporary physical database: %w", err)
				}
			}
		}
		if err := <-metadataDropped; err != nil {
			left = append(left, fmt.Sprintf("the probe's metadata collection %s.%s could not be removed (%v): drop it by hand", metadata.Database().Name(), metadata.Name(), err))
			if probeErr == nil {
				probeErr = fmt.Errorf("remove temporary metadata collection: %w", err)
			}
		}
		if len(left) > 0 {
			probeErr = fmt.Errorf("%w; %s", probeErr, strings.Join(left, "; "))
		}
	}()
	if err := physical.RunCommand(ctx, bson.D{{Key: "create", Value: "probe"}, {Key: "changeStreamPreAndPostImages", Value: bson.M{"enabled": true}}}).Err(); err != nil {
		// A refused create made nothing; any other failure may have.
		physicalMayExist = !walcli.IsUnauthorized(err)
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
	defer func() {
		// Bounded like a cleanup step: closing sends killCursors, which a
		// deployment that has stopped answering would hold for the driver's
		// own, much longer, timeout.
		closing, cancel := context.WithTimeout(context.Background(), probeCleanupStep)
		defer cancel()
		_ = stream.Close(closing)
	}()
	// Collections must exist before the transaction on all supported versions.
	metadataMayExist = true
	if _, err := metadata.InsertOne(ctx, bson.M{"_id": "setup"}); err != nil {
		metadataMayExist = !walcli.IsUnauthorized(err)
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
