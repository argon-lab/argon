package main

import (
	"context"
	"fmt"
	"os"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/config"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	// Check if WAL is enabled
	if !config.IsWALEnabled() {
		fmt.Println("WAL is not enabled. Set ENABLE_WAL=true to test WAL features")
		fmt.Println("\nTo enable WAL, run:")
		fmt.Println("  export ENABLE_WAL=true")
		os.Exit(1)
	}

	// Connect to MongoDB
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		fmt.Printf("Failed to connect to MongoDB: %v\n", err)
		os.Exit(1)
	}
	defer client.Disconnect(ctx)

	// Use test database
	db := client.Database("argon_wal_test")

	// Create services
	walService, err := wal.NewService(db)
	if err != nil {
		fmt.Printf("Failed to create WAL service: %v\n", err)
		os.Exit(1)
	}

	branchService, err := branchwal.NewBranchService(db, walService)
	if err != nil {
		fmt.Printf("Failed to create branch service: %v\n", err)
		os.Exit(1)
	}

	projectService, err := projectwal.NewProjectService(db, walService, branchService)
	if err != nil {
		fmt.Printf("Failed to create project service: %v\n", err)
		os.Exit(1)
	}

	// Test WAL functionality
	fmt.Println("Testing WAL functionality...")
	fmt.Println("========================")

	// 1. Create a project
	project, err := projectService.CreateProject("test-wal-project")
	if err != nil {
		fmt.Printf("Failed to create project: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Created project: %s (ID: %s)\n", project.Name, project.ID)

	// 2. List branches
	branches, err := branchService.ListBranches(project.ID)
	if err != nil {
		fmt.Printf("Failed to list branches: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Project has %d branch(es)\n", len(branches))
	for _, branch := range branches {
		fmt.Printf("  - %s (Head LSN: %d, Base LSN: %d)\n", 
			branch.Name, branch.HeadLSN, branch.BaseLSN)
	}

	// 3. Create a feature branch
	featureBranch, err := branchService.CreateBranch(project.ID, "feature-test", branches[0].ID)
	if err != nil {
		fmt.Printf("Failed to create feature branch: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Created feature branch: %s (Head LSN: %d)\n", 
		featureBranch.Name, featureBranch.HeadLSN)

	// 4. Show current WAL status
	currentLSN := walService.GetCurrentLSN()
	fmt.Printf("\n✓ Current WAL LSN: %d\n", currentLSN)

	// 5. Delete feature branch
	err = branchService.DeleteBranch(project.ID, "feature-test")
	if err != nil {
		fmt.Printf("Failed to delete branch: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ Deleted feature branch")

	// 6. Show final status
	fmt.Printf("\nWAL test completed successfully!\n")
	fmt.Printf("Final LSN: %d\n", walService.GetCurrentLSN())

	// Clean up
	fmt.Println("\nCleaning up test database...")
	if err := db.Drop(ctx); err != nil {
		fmt.Printf("Warning: Failed to drop test database: %v\n", err)
	}
}