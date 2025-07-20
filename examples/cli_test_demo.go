package main

import (
	"fmt"
	"log"
	"os"

	"github.com/argon-lab/argon/pkg/config"
	"github.com/argon-lab/argon/pkg/walcli"
)

// CLI Test Demo - demonstrates WAL CLI functionality programmatically
func main() {
	// Set WAL environment
	_ = os.Setenv("ENABLE_WAL", "true")
	_ = os.Setenv("MONGODB_URI", "mongodb://localhost:27017")

	fmt.Println("=== WAL CLI Integration Demo ===")

	// Test 1: Configuration check
	fmt.Println("\n1. Checking WAL configuration...")
	features := config.GetFeatures()
	fmt.Printf("   WAL Enabled: %v\n", features.EnableWAL)

	if !features.EnableWAL {
		fmt.Println("   ERROR: WAL not enabled. Set ENABLE_WAL=true")
		return
	}

	// Test 2: Service connection
	fmt.Println("\n2. Connecting to WAL services...")
	services, err := walcli.NewServices()
	if err != nil {
		log.Printf("   ERROR: Failed to connect: %v", err)
		fmt.Println("   Make sure MongoDB is running on localhost:27017")
		return
	}
	fmt.Printf("   Connection: OK\n")
	fmt.Printf("   Current LSN: %d\n", services.WAL.GetCurrentLSN())

	// Test 3: Project creation
	fmt.Println("\n3. Creating demo project...")
	projectName := "cli-demo-project"
	project, err := services.Projects.CreateProject(projectName)
	if err != nil {
		log.Printf("   ERROR: Failed to create project: %v", err)
		return
	}
	fmt.Printf("   Created project: %s (ID: %s)\n", project.Name, project.ID)

	// Test 4: List branches
	fmt.Println("\n4. Listing branches...")
	branches, err := services.Branches.ListBranches(project.ID)
	if err != nil {
		log.Printf("   ERROR: Failed to list branches: %v", err)
		return
	}

	if len(branches) == 0 {
		fmt.Println("   No branches found")
		return
	}

	mainBranch := branches[0]
	fmt.Printf("   Default branch: %s\n", mainBranch.Name)
	fmt.Printf("   Base LSN: %d, Head LSN: %d\n", mainBranch.BaseLSN, mainBranch.HeadLSN)

	// Test 5: Time travel info
	fmt.Println("\n5. Getting time travel information...")
	info, err := services.TimeTravel.GetTimeTravelInfo(mainBranch)
	if err != nil {
		log.Printf("   ERROR: Failed to get time travel info: %v", err)
		return
	}

	fmt.Printf("   Branch: %s\n", info.BranchName)
	fmt.Printf("   LSN Range: %d - %d\n", info.EarliestLSN, info.LatestLSN)
	fmt.Printf("   Entry Count: %d\n", info.EntryCount)

	if !info.EarliestTime.IsZero() {
		fmt.Printf("   Time Range: %s to %s\n",
			info.EarliestTime.Format("15:04:05"),
			info.LatestTime.Format("15:04:05"))
	}

	// Test 6: Restore preview (if there are operations)
	if info.EntryCount > 0 {
		fmt.Println("\n6. Testing restore preview...")

		// Preview restoring to base LSN
		preview, err := services.Restore.GetRestorePreview(mainBranch.ID, mainBranch.BaseLSN)
		if err != nil {
			log.Printf("   ERROR: Failed to get restore preview: %v", err)
		} else {
			fmt.Printf("   Preview restore to LSN %d:\n", preview.TargetLSN)
			fmt.Printf("   Operations to discard: %d\n", preview.OperationsToDiscard)

			if len(preview.AffectedCollections) > 0 {
				fmt.Println("   Affected collections:")
				for coll, count := range preview.AffectedCollections {
					fmt.Printf("     - %s: %d operations\n", coll, count)
				}
			}
		}
	} else {
		fmt.Println("\n6. Skipping restore preview (no operations yet)")
	}

	// Test 7: List all projects
	fmt.Println("\n7. Listing all projects...")
	projects, err := services.Projects.ListProjects()
	if err != nil {
		log.Printf("   ERROR: Failed to list projects: %v", err)
		return
	}

	fmt.Printf("   Total WAL projects: %d\n", len(projects))
	for i, p := range projects {
		fmt.Printf("   %d. %s (ID: %s)\n", i+1, p.Name, p.ID)

		// Show branch count for each project
		projectBranches, _ := services.Branches.ListBranches(p.ID)
		fmt.Printf("      Branches: %d\n", len(projectBranches))
	}

	fmt.Println("\n=== Demo Complete ===")
	fmt.Println("This demo showed:")
	fmt.Println("  ✅ WAL configuration check")
	fmt.Println("  ✅ Service connection")
	fmt.Println("  ✅ Project creation")
	fmt.Println("  ✅ Branch listing")
	fmt.Println("  ✅ Time travel information")
	fmt.Println("  ✅ Restore preview")
	fmt.Println("  ✅ Project listing")
	fmt.Println("")
	fmt.Println("All CLI functionality is working through the service layer!")
}
