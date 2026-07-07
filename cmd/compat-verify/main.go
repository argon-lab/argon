// compat-verify checks that a branch's WAL materialization converges to
// exactly the state of its physical database — the acceptance check behind
// the driver-compatibility harness: whatever an unmodified driver did to
// the branch database, the versioned history must reproduce it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/argon-lab/argon/internal/mongoexpr"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/argon-lab/argon/pkg/walcli"
	"go.mongodb.org/mongo-driver/bson"
)

func main() {
	project := flag.String("project", "", "project name")
	branch := flag.String("branch", "main", "branch name")
	timeout := flag.Duration("timeout", 60*time.Second, "how long to wait for convergence")
	mode := flag.String("mode", "match", "match: WAL ≡ physical · empty: physical has no documents · head: print head LSN")
	flag.Parse()
	if *project == "" {
		fmt.Fprintln(os.Stderr, "--project is required")
		os.Exit(2)
	}

	services, err := walcli.NewServices()
	if err != nil {
		fatal("connect: %v", err)
	}
	p, err := services.Projects.GetProjectByName(*project)
	if err != nil {
		fatal("project %q: %v", *project, err)
	}
	b, err := services.Branches.GetBranch(p.ID, *branch)
	if err != nil {
		fatal("branch %q: %v", *branch, err)
	}
	if !b.IsLive() {
		fatal("branch %q is not checked out", *branch)
	}

	if *mode == "head" {
		fmt.Println(b.HeadLSN)
		return
	}

	ctx := context.Background()
	deadline := time.Now().Add(*timeout)
	var lastDiff string
	for {
		var diff string
		var err error
		switch *mode {
		case "match":
			diff, err = compare(ctx, services, b.ID)
		case "empty":
			diff, err = physicalEmpty(ctx, services, b.ID)
		default:
			fatal("unknown --mode %q", *mode)
		}
		if err != nil {
			fatal("%s: %v", *mode, err)
		}
		if diff == "" {
			fresh, _ := services.Branches.GetBranchByID(b.ID)
			if *mode == "empty" {
				fmt.Printf("CONVERGED: physical database is empty (branch head LSN %d)\n", fresh.HeadLSN)
			} else {
				fmt.Printf("CONVERGED: WAL state equals the physical database (branch head LSN %d)\n", fresh.HeadLSN)
			}
			return
		}
		lastDiff = diff
		if time.Now().After(deadline) {
			fatal("did not converge within %v; last difference: %s", *timeout, lastDiff)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// physicalEmpty returns "" when every collection in the branch's physical
// database has zero documents — the expected end state after undoing an
// entire driver session.
func physicalEmpty(ctx context.Context, services *walcli.Services, branchID string) (string, error) {
	branch, err := services.Branches.GetBranchByID(branchID)
	if err != nil {
		return "", err
	}
	physical := services.Client.Database(branch.PhysicalDB)
	names, err := physical.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return "", err
	}
	for _, name := range names {
		count, err := physical.Collection(name).CountDocuments(ctx, bson.M{})
		if err != nil {
			return "", err
		}
		if count != 0 {
			return fmt.Sprintf("collection %s still has %d documents", name, count), nil
		}
	}
	return "", nil
}

// compare returns "" when the WAL materialization equals the physical
// database, or a description of the first difference found.
func compare(ctx context.Context, services *walcli.Services, branchID string) (string, error) {
	branch, err := services.Branches.GetBranchByID(branchID)
	if err != nil {
		return "", err
	}
	walState, err := services.Materializer.MaterializeBranch(branch)
	if err != nil {
		return "", err
	}

	physical := services.Client.Database(branch.PhysicalDB)
	names, err := physical.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return "", err
	}

	physState := make(map[string]map[string]bson.M)
	for _, name := range names {
		cursor, err := physical.Collection(name).Find(ctx, bson.M{})
		if err != nil {
			return "", err
		}
		var docs []bson.M
		if err := cursor.All(ctx, &docs); err != nil {
			return "", err
		}
		collState := make(map[string]bson.M, len(docs))
		for _, doc := range docs {
			collState[wal.DocumentIDString(doc["_id"])] = doc
		}
		physState[name] = collState
	}

	for name, phys := range physState {
		walColl := walState[name]
		if len(walColl) != len(phys) {
			return fmt.Sprintf("collection %s: %d docs in WAL vs %d physical", name, len(walColl), len(phys)), nil
		}
		for id, physDoc := range phys {
			walDoc, ok := walColl[id]
			if !ok {
				return fmt.Sprintf("collection %s: document %s missing from WAL", name, id), nil
			}
			equal, err := mongoexpr.CanonicalEqual(walDoc, physDoc)
			if err != nil {
				return "", err
			}
			if !equal {
				return fmt.Sprintf("collection %s: document %s differs", name, id), nil
			}
		}
	}
	for name, walColl := range walState {
		if _, ok := physState[name]; !ok && len(walColl) > 0 {
			return fmt.Sprintf("collection %s exists in WAL but not physically", name), nil
		}
	}
	return "", nil
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "compat-verify: "+format+"\n", args...)
	os.Exit(1)
}
