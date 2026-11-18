package ingest

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"hash"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

var updateSourceHealth = flag.Bool("update", false, "regenerate the source health golden file")

// An omitted column matches every value. The two fully specified healthy rows
// are disjoint from all failures. Overlapping failures are in remedy order.
var sourceDecisionTable = []struct {
	when   string
	kind   VerdictKind
	reason Reason
}{
	{"project=published checked_out=no head=equal unfinished_resync=no position_present=yes mode=exact open_range=no owner=live unverified=no state=running stored_error=no exact_point_at_head=yes", "healthy", "exact"},
	{"project=published checked_out=no head=equal unfinished_resync=no position_present=yes mode=exact open_range=no owner=none|live unverified=no state=stopped stored_error=no exact_point_at_head=yes", "healthy", "exact"},

	{"project=importing", "importing", "importing"},
	{"project=missing", "unhealthy", "project_missing"},
	{"checked_out=yes", "unhealthy", "checked_out"},
	{"head=different", "unhealthy", "head_changed"},
	{"unfinished_resync=yes", "unhealthy", "resync_unfinished"},
	{"position_present=no", "unhealthy", "position_missing"},
	{"head=missing", "unhealthy", "head_missing"},
	{"mode=absent|unknown", "unhealthy", "mode_unknown"},
	{"mode=exact open_range=yes", "unhealthy", "range_inconsistent"},
	{"mode=exact exact_point_at_head=no", "unhealthy", "checkpoint_unverified"},
	{"state=absent|unknown", "unhealthy", "state_unknown"},

	// The owner can die before it replaces any state, even a clean stop.
	{"owner=expired", "unhealthy", "owner_expired"},
	{"state=degraded", "unhealthy", "degraded"},
	{"unverified=yes", "unhealthy", "unverified"},
	{"owner=none state=starting|running|catching_up|retrying|resyncing", "unhealthy", "owner_missing"},
	{"state=retrying", "unhealthy", "retrying"},

	// Stopping preserves both terminal failures and an open catch-up range.
	{"state=stopped stored_error=yes", "unhealthy", "capture_error"},
	{"stored_error=yes", "unhealthy", "capture_error"},
	{"state=stopped mode=catch_up", "unhealthy", "catch_up"},
	{"mode=catch_up", "unhealthy", "catch_up"},
	{"state=catching_up", "unhealthy", "catch_up"},

	{"state=starting", "unhealthy", "starting"},
	{"state=resyncing", "unhealthy", "resyncing"},
}

type healthAxis struct {
	name   string
	values string // value names used in the decision table
	codes  string // one byte per value, in lexical order
	set    func(*Facts, string)
}

func healthBoolean(name string, set func(*Facts, bool)) healthAxis {
	return healthAxis{name, "no yes", "01", func(f *Facts, v string) { set(f, v == "yes") }}
}

var healthAxes = []healthAxis{
	{"project", "importing missing published", "imp", func(f *Facts, v string) {
		f.Project = map[string]ProjectState{"importing": ProjectImporting, "missing": ProjectMissing, "published": ProjectPublished}[v]
	}},
	healthBoolean("checked_out", func(f *Facts, b bool) { f.CheckedOut = b }),
	{"head", "different equal missing", "dem", func(f *Facts, v string) {
		f.Head = map[string]HeadState{"different": HeadDifferent, "equal": HeadEqual, "missing": HeadMissing}[v]
	}},
	healthBoolean("unfinished_resync", func(f *Facts, b bool) { f.UnfinishedResync = b }),
	healthBoolean("position_present", func(f *Facts, b bool) { f.PositionPresent = b }),
	{"mode", "absent unknown catch_up exact", "-?ce", func(f *Facts, v string) { f.Mode = checkpointMode(storedHealthValue(v)) }},
	healthBoolean("open_range", func(f *Facts, b bool) { f.OpenRange = b }),
	{"owner", "none expired live", "-el", func(f *Facts, v string) {
		f.Owner = map[string]OwnerState{"none": OwnerNone, "expired": OwnerExpired, "live": OwnerLive}[v]
	}},
	healthBoolean("unverified", func(f *Facts, b bool) { f.Unverified = b }),
	{"state", "absent unknown starting catching_up degraded running stopped retrying resyncing", "-?bcdrsty", func(f *Facts, v string) { f.State = followerState(storedHealthValue(v)) }},
	healthBoolean("stored_error", func(f *Facts, b bool) { f.HasError = b }),
	healthBoolean("exact_point_at_head", func(f *Facts, b bool) { f.ExactPointAtHead = b }),
}

func storedHealthValue(v string) string {
	if v == "absent" {
		return ""
	}
	if v == "unknown" {
		return "unknown_future_value"
	}
	return v
}

// Each pattern lists the allowed codes per column. An empty column is a wildcard.
type healthPattern []string

func compileHealthPattern(t *testing.T, spec string) healthPattern {
	t.Helper()
	pattern := make(healthPattern, len(healthAxes))
	for _, field := range strings.Fields(spec) {
		name, values, ok := strings.Cut(field, "=")
		column := slices.IndexFunc(healthAxes, func(a healthAxis) bool { return a.name == name })
		if !ok || column < 0 || pattern[column] != "" {
			t.Fatalf("invalid decision-table field %q", field)
		}
		a := healthAxes[column]
		for _, value := range strings.Split(values, "|") {
			i := slices.Index(strings.Fields(a.values), value)
			if i < 0 {
				t.Fatalf("unknown %s in decision-table field %q", value, field)
			}
			pattern[column] += a.codes[i : i+1]
		}
	}
	return pattern
}

func (p healthPattern) matches(key string) bool {
	for col, allowed := range p {
		if allowed != "" && strings.IndexByte(allowed, key[col]) < 0 {
			return false
		}
	}
	return true
}

// healthyBySourceRule states the checkpoint rule independently of reason precedence.
func healthyBySourceRule(f Facts) bool {
	running := f.State == FollowerRunning && f.Owner == OwnerLive
	stopped := f.State == FollowerStopped && (f.Owner == OwnerNone || f.Owner == OwnerLive)
	return f.Project == ProjectPublished && !f.CheckedOut && f.Head == HeadEqual &&
		!f.UnfinishedResync && f.PositionPresent && f.Mode == CheckpointModeExact &&
		!f.OpenRange && f.ExactPointAtHead && !f.HasError && !f.Unverified && (running || stopped)
}

func TestSourceHealthDecisionTable(t *testing.T) {
	type reasonSummary struct {
		count  int
		digest hash.Hash
	}
	byReason := make(map[Reason]*reasonSummary)
	healthyByValue := make(map[string]int)
	var healthyCombinations []string
	patterns := make([]healthPattern, len(sourceDecisionTable))
	hits := make([]int, len(sourceDecisionTable))
	for row, d := range sourceDecisionTable {
		patterns[row] = compileHealthPattern(t, d.when)
	}
	for _, a := range healthAxes {
		values := strings.Fields(a.values)
		if len(values) != len(a.codes) || !slices.IsSorted([]byte(a.codes)) {
			t.Fatalf("invalid codes for %s: %q", a.name, a.codes)
		}
	}

	var combinations int
	var enumerate func(Facts, string)
	enumerate = func(f Facts, key string) {
		if len(key) < len(healthAxes) {
			a := healthAxes[len(key)]
			for i, value := range strings.Fields(a.values) {
				a.set(&f, value)
				enumerate(f, key+a.codes[i:i+1])
			}
			return
		}
		row := slices.IndexFunc(patterns, func(p healthPattern) bool { return p.matches(key) })
		if row < 0 {
			t.Fatalf("no decision-table row matches %s: %+v", key, f)
		}
		hits[row]++
		want := sourceDecisionTable[row]
		v := Evaluate(f)
		healthy := v.Kind == SourceHealthy
		if expected := healthyBySourceRule(f); healthy != expected {
			t.Fatalf("facts %s: %+v\ncheckpoint rule wants healthy=%t, got %+v", key, f, expected, v)
		}
		if v.Kind != want.kind || v.Reason != want.reason {
			t.Fatalf("facts %s: %+v\nrow %d (%s) wants %s/%s, got %+v", key, f, row+1, want.when, want.kind, want.reason, v)
		}
		if v.Message == "" || v.NextAction == "" || (f.HasError && !strings.Contains(v.Message, f.StoredError)) {
			t.Fatalf("missing operator detail for facts %s: %+v", key, v)
		}
		if (v.HealthError() == nil) != (want.kind == SourceHealthy) || (v.WriteBackAdmission() == nil) != (want.kind == SourceHealthy) {
			t.Fatalf("exit or write-back disagrees with %s for %s", want.kind, key)
		}
		reason := byReason[v.Reason]
		if reason == nil {
			reason = &reasonSummary{digest: sha256.New()}
			byReason[v.Reason] = reason
		}
		reason.count++
		// A count alone would hide a swap between reasons or a changed next action.
		fmt.Fprintf(reason.digest, "%s %q %q %q %t\n", key, v.Kind, v.Message, v.NextAction, v.LeaseExpired)
		if healthy {
			fields := make([]string, len(healthAxes))
			for column, axis := range healthAxes {
				value := strings.Fields(axis.values)[strings.IndexByte(axis.codes, key[column])]
				fields[column] = axis.name + "=" + value
				healthyByValue[fields[column]]++
			}
			healthyCombinations = append(healthyCombinations, strings.Join(fields, " "))
		}
		combinations++
	}
	enumerate(Facts{
		BranchName: "main", StoredError: "stored stream error",
		OwnerLoss: "recorded owner loss", HeadMismatch: "saved and actual head differ",
	}, "")
	for row, matches := range hits {
		if matches == 0 {
			t.Fatalf("decision-table row %d (%s) is unreachable", row+1, sourceDecisionTable[row].when)
		}
	}
	if combinations != 124416 {
		t.Fatalf("enumerated %d combinations, want 124416", combinations)
	}

	var got bytes.Buffer
	fmt.Fprintln(&got, "# Combinations by reason (count and SHA-256 of cases and verdicts)")
	for _, code := range slices.Sorted(maps.Keys(byReason)) {
		summary := byReason[code]
		fmt.Fprintf(&got, "%s %d %x\n", code, summary.count, summary.digest.Sum(nil))
	}
	fmt.Fprintln(&got, "\n# Healthy combinations by axis value")
	for _, axis := range healthAxes {
		for _, value := range strings.Fields(axis.values) {
			field := axis.name + "=" + value
			fmt.Fprintf(&got, "%s %d\n", field, healthyByValue[field])
		}
	}
	fmt.Fprintln(&got, "\n# Healthy combinations")
	for _, combination := range healthyCombinations {
		fmt.Fprintln(&got, combination)
	}

	const file = "testdata/source_health.golden"
	if *updateSourceHealth {
		if err := os.WriteFile(file, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(golden, got.Bytes()) {
		t.Fatalf("%s changed; inspect the decision table and regenerate with -update", file)
	}
}

func TestSourceHealthNormalizesStoredValues(t *testing.T) {
	for _, raw := range []string{"", "a future state", "starting", "running", "catching_up", "retrying", "resyncing", "stopped", "degraded"} {
		state := followerState(raw)
		if state.expectsOwner() != slices.Contains(sourceLiveStates, raw) {
			t.Errorf("ownership check for %q differs from lease acquisition", raw)
		}
	}
	if followerState("") == followerState("a future state") || checkpointMode("") == checkpointMode("a future mode") {
		t.Fatal("empty and unfamiliar fields must retain distinct values")
	}
}

func TestSourceHealthRequiresResyncForUnprovenExactHead(t *testing.T) {
	f := Facts{
		Project: ProjectPublished, Head: HeadEqual, PositionPresent: true,
		Mode: CheckpointModeExact, State: FollowerStopped, Owner: OwnerExpired, HasError: true,
	}
	for _, takeover := range []bool{false, true} {
		f.Unverified = takeover
		v := Evaluate(f)
		probe, err := v.ResyncAdmission(false)
		if v.Reason != ReasonCheckpointUnverified || v.FollowAdmission() == nil || probe || err != nil {
			t.Fatalf("unproven exact head admitted for replay: %+v, probe=%t, resync stored_error=%v", v, probe, err)
		}
	}
}
