package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
)

func TestCLISourceCommandsWithoutOptIn(t *testing.T) {
	const env = "ARGON_EXPERIMENTAL_SOURCE"
	t.Setenv(env, "")
	if err := os.Unsetenv(env); err != nil {
		t.Fatal(err)
	}

	// A malformed metadata URI fails immediately if the command reaches the driver.
	for _, args := range [][]string{
		{"import", "database", "--online", "--database", "source", "--project", "target", "--uri", "invalid", "--yes"},
		{"import", "database", "--online", "--database", "source", "--project", "target", "--uri", "invalid", "--dry-run"},
		{"source", "follow", "--project", "p", "--uri", "invalid"},
		{"source", "status", "--project", "p"},
		{"source", "resync", "--project", "p", "--uri", "invalid"},
		{"source", "detach", "--project", "p", "--yes"},
	} {
		_, stderr, err := invokeCLI(t, "invalid", args...)
		if err == nil || !strings.Contains(stderr, "failed to connect") || strings.Contains(stderr, env) {
			t.Errorf("%v: expected a connection error with the switch unset, got %v (stderr %q)", args, err, stderr)
		}
	}

	if _, stderr, err := invokeCLI(t, "invalid", "source"); err != nil {
		t.Errorf("source help: %v (stderr %q)", err, stderr)
	}

	for _, tc := range []struct {
		args []string
		want error
	}{
		{[]string{"merge", "apply", "000000000000000000000001", "--write-back", "--uri", "invalid"}, walcli.RequireExperimentalWriteBack()},
		{[]string{"merge", "apply", "--write-back"}, walcli.RequireExperimentalWriteBack()},
		{[]string{"source", "follow", "--auto-resync"}, walcli.RequireExperimentalAutoResync()},
		{[]string{"source", "follow", "--enable-source-images"}, walcli.RequireExperimentalSourceImages()},
		{[]string{"import", "database", "--online", "--enable-source-images"}, walcli.RequireExperimentalSourceImages()},
	} {
		if tc.want == nil {
			t.Fatalf("%v: expected an opt-in error", tc.args)
		}
		stdout, stderr, err := invokeCLI(t, "invalid", tc.args...)
		if err == nil || stdout != "" || strings.TrimSpace(stderr) != "Error: "+tc.want.Error() {
			t.Errorf("%v: expected %q before running the command, got %v (stdout %q, stderr %q)", tc.args, tc.want, err, stdout, stderr)
		}
	}
}

func TestCLIExperimentalSourceHelp(t *testing.T) {
	for _, tc := range []struct {
		command []string
		flags   map[string]bool // true for flags requiring the experimental switch
	}{
		{[]string{"import", "database"}, map[string]bool{"--online": false, "--enable-source-images": true}},
		{[]string{"source", "follow"}, map[string]bool{"--enable-source-images": true, "--auto-resync": true}},
		{[]string{"merge", "apply"}, map[string]bool{"--write-back": true}},
	} {
		stdout, stderr, err := invokeCLI(t, "invalid", append(tc.command, "--help")...)
		if err != nil {
			t.Fatalf("%v --help: %v (stderr %q)", tc.command, err, stderr)
		}
		_, flags, _ := strings.Cut(stdout, "Flags:\n")
		for name, gated := range tc.flags {
			var usage string
			for line := range strings.SplitSeq(flags, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), name+" ") {
					usage = line
					break
				}
			}
			if usage == "" || strings.Contains(strings.ToLower(usage), "experimental") != gated || strings.Contains(usage, "ARGON_EXPERIMENTAL_SOURCE=1") != gated {
				t.Errorf("%v: %s must describe gated=%t, got %q", tc.command, name, gated, usage)
			}
		}
	}
}
