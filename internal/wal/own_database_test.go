package wal

import (
	"strings"
	"testing"
)

// Everything that writes to, alters or drops a branch's physical database
// asks the branch for it. Only the database Argon generated for that branch
// is ever returned.
func TestOwnDatabaseAcceptsOnlyTheGeneratedName(t *testing.T) {
	own := &Branch{ID: "6afd13a0894eacff7fdeab7d", Name: "sandbox-1", State: BranchStateLive, PhysicalDB: "argon_br_6afd13a0894eacff7fdeab7d"}
	name, err := own.OwnDatabase()
	if err != nil || name != PhysicalDBName(own.ID) {
		t.Fatalf("a branch's own database was refused: %q %v", name, err)
	}
	for label, branch := range map[string]*Branch{
		"an application database":          {ID: "b1", Name: "x", State: BranchStateLive, PhysicalDB: "production"},
		"another branch's database":        {ID: "b1", Name: "x", State: BranchStateLive, PhysicalDB: "argon_br_b2"},
		"the metadata database":            {ID: "b1", Name: "x", State: BranchStateLive, PhysicalDB: "argon_wal"},
		"no database":                      {ID: "b1", Name: "x", State: BranchStateLive},
		"a follower with a generated name": {ID: "b1", Name: "main", Follows: "shop", PhysicalDB: "argon_br_b1"},
		"a follower claiming its source":   {ID: "b1", Name: "main", Follows: "shop", PhysicalDB: "shop", State: BranchStateLive},
		"a prefix of the generated name":   {ID: "b1", Name: "x", State: BranchStateLive, PhysicalDB: "argon_br_b"},
		"the generated name with a suffix": {ID: "b1", Name: "x", State: BranchStateLive, PhysicalDB: "argon_br_b1.other"},
		"the generated name in other case": {ID: "b1", Name: "x", State: BranchStateLive, PhysicalDB: "ARGON_BR_b1"},
	} {
		if name, err := branch.OwnDatabase(); err == nil {
			t.Fatalf("%s was accepted as the branch's own database: %q", label, name)
		} else if !strings.Contains(err.Error(), "physical database") {
			t.Fatalf("%s: unhelpful refusal: %v", label, err)
		}
	}
}
