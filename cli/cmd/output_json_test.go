package cmd

import (
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
)

// The four commands that used to print only text must answer -o json.
func TestGCAndRestoreAcceptJSONOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command *cobra.Command
	}{
		{"gc", gcCmd},
		{"restore preview", restorePreviewCmd},
		{"restore reset", restoreResetCmd},
		{"restore branch", restoreBranchCmd},
	} {
		flag := tc.command.Flags().Lookup("output")
		if flag == nil {
			t.Errorf("%s has no --output flag", tc.name)
			continue
		}
		if flag.Shorthand != "o" {
			t.Errorf("%s --output shorthand is %q, want \"o\"", tc.name, flag.Shorthand)
		}
		if flag.DefValue != "table" {
			t.Errorf("%s --output default is %q, want \"table\"", tc.name, flag.DefValue)
		}
	}
}

func TestGCReportJSONShape(t *testing.T) {
	dry := gcReportJSON(true, 0, []gcBranchJSON{
		{Branch: "main", Cutoffs: map[string]int64{"users": 42}},
	})
	if dry["dry_run"] != true {
		t.Errorf("dry_run: %v", dry["dry_run"])
	}
	encoded, err := json.Marshal(dry)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dry_run", "branches", "entries_removed"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("gc JSON lacks %q: %s", key, encoded)
		}
	}
	branches, ok := decoded["branches"].([]any)
	if !ok || len(branches) != 1 {
		t.Fatalf("branches: %s", encoded)
	}
	first := branches[0].(map[string]any)
	if first["branch"] != "main" {
		t.Errorf("branch name: %v", first["branch"])
	}
	if cutoffs, ok := first["cutoffs"].(map[string]any); !ok || cutoffs["users"] != float64(42) {
		t.Errorf("cutoffs: %v", first["cutoffs"])
	}

	// No branches and no removals still yields a value with an empty list,
	// never null, so a caller can iterate without a guard.
	empty := gcReportJSON(false, 7, nil)
	if empty["entries_removed"] != int64(7) {
		t.Errorf("entries_removed: %v", empty["entries_removed"])
	}
	if _, ok := empty["branches"].([]gcBranchJSON); !ok {
		t.Errorf("branches is not an empty slice: %T", empty["branches"])
	}
}

func TestRestoreJSONShapes(t *testing.T) {
	preview := restorePreviewJSON("main", 120, 100, 3, map[string]int{"users": 2, "orders": 1})
	if preview["branch"] != "main" || preview["current_lsn"] != int64(120) || preview["target_lsn"] != int64(100) {
		t.Errorf("preview: %v", preview)
	}
	if preview["operations_to_discard"] != 3 {
		t.Errorf("discard: %v", preview["operations_to_discard"])
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	affected, ok := decoded["affected_collections"].(map[string]any)
	if !ok || affected["users"] != float64(2) || affected["orders"] != float64(1) {
		t.Errorf("affected_collections: %s", encoded)
	}

	// A nil map is an empty object, not null.
	if got, ok := restorePreviewJSON("main", 1, 2, 0, nil)["affected_collections"].(map[string]int); !ok || got == nil {
		t.Errorf("nil affected collections did not become an empty map")
	}

	withBackup := restoreResetJSON("main", 100, "main-backup")
	if withBackup["backup_branch"] != "main-backup" {
		t.Errorf("backup_branch missing: %v", withBackup)
	}
	withoutBackup := restoreResetJSON("main", 100, "")
	if _, present := withoutBackup["backup_branch"]; present {
		t.Errorf("backup_branch reported though no backup was created: %v", withoutBackup)
	}

	branched := restoreBranchJSON("main-at-100", 100)
	if branched["branch"] != "main-at-100" || branched["target_lsn"] != int64(100) {
		t.Errorf("restore branch: %v", branched)
	}
}
