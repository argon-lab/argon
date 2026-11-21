package cmd

import (
	"encoding/json"
	"testing"
)

func TestMigrateReportJSONShapeLocal(t *testing.T) {
	report := migrateReportJSON("demo", "proj-1", 2, 3, 1)
	for _, key := range []string{"project", "project_id", "branches_visited", "entries_rewritten", "entries_removed"} {
		if _, ok := report[key]; !ok {
			t.Errorf("migrate JSON lacks %q: %v", key, report)
		}
	}
	if report["project"] != "demo" || report["project_id"] != "proj-1" {
		t.Errorf("project fields: %v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
}
