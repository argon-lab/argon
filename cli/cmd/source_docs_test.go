package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The built CLI accepts --allow-inexact on `import database`; docs/CLI.md's
// Import section does not mention it.
func TestCLIImportFlagsAreDocumented(t *testing.T) {
	stdout, stderr, err := invokeCLI(t, "mongodb://127.0.0.1:1", "import", "database", "--help")
	if err != nil {
		t.Fatalf("help: %v %s", err, stderr)
	}
	if !strings.Contains(stdout, "--allow-inexact") {
		t.Fatalf("CLI help does not show --allow-inexact; suspicion premise false:\n%s", stdout)
	}
	for _, l := range strings.Split(stdout, "\n") {
		if strings.Contains(l, "--") {
			t.Logf("help: %s", strings.TrimSpace(l))
		}
	}
	raw, err := os.ReadFile("../../docs/CLI.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "## Import (")
	end := strings.Index(doc, "## Follow a live database")
	if start < 0 || end < start {
		t.Fatal("sections not found")
	}
	section := doc[start:end]
	flags := regexp.MustCompile(`--[a-z][a-z-]+`).FindAllString(stdout, -1)
	seen := map[string]bool{}
	for _, f := range flags {
		if seen[f] || f == "--help" {
			continue
		}
		seen[f] = true
		t.Logf("flag %-24s in Import section of docs/CLI.md: %v", f, strings.Contains(section, f))
	}
	if !strings.Contains(section, "--allow-inexact") {
		t.Errorf("import database accepts --allow-inexact but the Import section of docs/CLI.md never mentions it")
	}
}
