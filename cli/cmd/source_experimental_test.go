package cmd

import (
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
)

func TestCLIExperimentalSourceCommandsRequireOptIn(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "")
	want := walcli.RequireExperimentalSource()
	if want == nil {
		t.Fatal("experimental source commands must be off by default")
	}
	for _, args := range [][]string{
		{"import", "database", "--online", "--database", "source", "--project", "target", "--uri", "invalid", "--yes"},
		{"import", "database", "--online", "--dry-run"},
		{"source"},
		{"source", "follow", "--project", "p", "--uri", "invalid", "--enable-source-images"},
		{"source", "status", "--project", "p", "--output", "invalid"},
		{"source", "resync", "--project", "p", "--uri", "invalid"},
		{"source", "detach", "--project", "p", "--yes"},
		{"merge", "apply", "000000000000000000000001", "--write-back", "--uri", "invalid"},
		{"merge", "apply", "--write-back"},
	} {
		stdout, stderr, err := invokeCLI(t, "invalid", args...)
		if err == nil || stdout != "" || strings.TrimSpace(stderr) != "Error: "+want.Error() {
			t.Errorf("%v: expected the experimental error before running the command, got %v (stdout %q, stderr %q)", args, err, stdout, stderr)
		}
	}
}

func TestCLIExperimentalSourceHelp(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "")
	for _, args := range [][]string{
		{"import", "database", "--help"}, {"merge", "apply", "--help"},
		{"source", "--help"}, {"source", "follow", "--help"}, {"source", "status", "--help"},
		{"source", "resync", "--help"}, {"source", "detach", "--help"},
	} {
		stdout, stderr, err := invokeCLI(t, "invalid", args...)
		if err != nil || !strings.Contains(strings.ToLower(stdout), "experimental") || !strings.Contains(stdout, "ARGON_EXPERIMENTAL_SOURCE=1") {
			t.Errorf("%v: help must document the opt-in and remain available: %v (stdout %q, stderr %q)", args, err, stdout, stderr)
		}
	}
}
