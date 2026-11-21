package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
)

func TestCLISourceStatusShowsLeaseOwner(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	info := &walcli.SourceInfo{Owner: &walcli.SourceOwner{
		Kind: "follow", Host: "worker", PID: 42,
		StartedAt:   time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
		HeartbeatAt: time.Date(2026, 11, 18, 12, 0, 10, 0, time.UTC),
	}}
	printSourceStatus(cmd, info)
	if !strings.Contains(out.String(), "follow on worker (PID 42, last heartbeat 2026-11-18T12:00:10Z)") {
		t.Fatalf("table output missing lease owner:\n%s", out.String())
	}
	out.Reset()
	if err := writeJSON(cmd, info); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"owner": {`, `"kind": "follow"`, `"host": "worker"`, `"pid": 42`, `"heartbeat_at": "2026-11-18T12:00:10Z"`} {
		if !strings.Contains(out.String(), field) {
			t.Fatalf("JSON output missing %s:\n%s", field, out.String())
		}
	}
}

func TestCLISourceStatusShowsWeakIdentityWriteBackRestriction(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	info := &walcli.SourceInfo{
		Identity:                   "replica-set name and member hosts",
		WriteBackUnavailableReason: "requires a replica-set ID recorded at import",
	}
	printSourceStatus(cmd, info)
	for _, field := range []string{info.Identity, "unavailable (" + info.WriteBackUnavailableReason + ")"} {
		if !strings.Contains(out.String(), field) {
			t.Fatalf("table output missing %s:\n%s", field, out.String())
		}
	}
	out.Reset()
	if err := writeJSON(cmd, info); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"identity": "replica-set name and member hosts"`, `"write_back_unavailable_reason": "requires a replica-set ID recorded at import"`} {
		if !strings.Contains(out.String(), field) {
			t.Fatalf("JSON output missing %s:\n%s", field, out.String())
		}
	}
}

func TestCLISourceStatusShowsOmittedInexactRanges(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printSourceStatus(cmd, &walcli.SourceInfo{Mode: "exact", InexactRangesDropped: 3})
	if !strings.Contains(out.String(), "3 inexact range(s) omitted") || strings.Contains(out.String(), "exact outside the ranges below") {
		t.Fatalf("table output should acknowledge omitted ranges:\n%s", out.String())
	}
}

func TestCLISourceStatusPrintsVerdictAndNextAction(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	info := &walcli.SourceInfo{State: "stopped"}
	info.Health.Kind = "unhealthy"
	info.Health.Reason = "catch_up"
	info.Health.Message = "The inexact range is still open."
	info.Health.NextAction = "Start the follower."

	printSourceStatus(cmd, info)
	for _, want := range []string{"unhealthy (catch_up)", info.Health.Message, info.Health.NextAction} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table is missing %q:\n%s", want, out.String())
		}
	}
}

func TestCLISourceStatusShowsChangeWaitingForNextWrite(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printSourceStatus(cmd, &walcli.SourceInfo{Mode: "exact"})
	if strings.Contains(out.String(), "Waiting:") {
		t.Fatalf("nothing waits, yet the table says so:\n%s", out.String())
	}
	out.Reset()
	since := time.Date(2026, 11, 21, 22, 6, 3, 0, time.UTC)
	info := &walcli.SourceInfo{Mode: "exact", WaitingSince: &since}
	printSourceStatus(cmd, info)
	if !strings.Contains(out.String(), "Waiting:     a change made at 2026-11-21T22:06:03Z has been read and is stored when the source writes again") {
		t.Fatalf("table output missing the waiting change:\n%s", out.String())
	}
	out.Reset()
	if err := writeJSON(cmd, info); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"waiting_since": "2026-11-21T22:06:03Z"`) {
		t.Fatalf("JSON output missing waiting_since:\n%s", out.String())
	}
}
