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
