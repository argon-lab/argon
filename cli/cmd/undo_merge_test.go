package cmd

import (
	"strings"
	"testing"
)

// --merge selects the merge's own changes; a range or actor next to it would
// silently mean something else, so the combination is refused before any
// connection is made.
func TestUndoMergeRefusesARangeOrActor(t *testing.T) {
	const unreachable = "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100"
	for _, extra := range [][]string{{"--actor", "agent"}, {"--from-lsn", "3"}, {"--to-lsn", "9"}, {"--branch", "main"}} {
		args := append([]string{"undo", "--merge", "6a0000000000000000000000"}, extra...)
		_, stderr, err := invokeCLI(t, unreachable, args...)
		if err == nil || !strings.Contains(stderr, "cannot be combined") {
			t.Fatalf("%v: want a refusal naming the combination, got %v %q", extra, err, stderr)
		}
	}
}
