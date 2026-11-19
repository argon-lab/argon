package wal_test

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
)

func TestSource_OldBinaryRecoveryCommands(t *testing.T) {
	cli := buildSourceCLI(t)
	command := regexp.MustCompile("`argon ([^`]+)`")
	for _, cause := range []error{&wal.AmbiguousOldBinaryError{}, &wal.ReclaimedOldBinaryError{}} {
		var names []string
		for _, match := range command.FindAllStringSubmatch(cause.Error(), -1) {
			args := strings.Fields(match[1])
			name, _, _ := strings.Cut(match[1], " -")
			output, err := exec.CommandContext(t.Context(), cli, append(args, "--help")...).CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Contains(t, string(output), "Usage:\n  argon "+name+" [flags]")
			names = append(names, name)
		}
		require.ElementsMatch(t, []string{"source resync", "time-travel query", "restore branch", "restore reset", "import database"}, names)
		require.Contains(t, cause.Error(), "new project")
	}
}
