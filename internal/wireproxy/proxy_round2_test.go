package wireproxy

import (
	"errors"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// round2Store is a metadata store with one live branch, "shop~main", that
// can be made to fail every read the way a deployment does while it cannot
// be reached: with an error that says nothing about the branch.
type round2Store struct {
	down  bool
	reads int
}

var errRound2Unreachable = errors.New("server selection error: server selection timeout")

func (s *round2Store) GetProjectIDByName(name string) (string, error) {
	s.reads++
	if s.down {
		return "", errRound2Unreachable
	}
	if name == "shop" {
		return "project-1", nil
	}
	return "", errors.New("project not found")
}

func (s *round2Store) GetBranch(projectID, name string) (*wal.Branch, error) {
	s.reads++
	if s.down {
		return nil, errRound2Unreachable
	}
	if projectID == "project-1" && name == "main" {
		return &wal.Branch{ID: "b1", Name: "main", State: wal.BranchStateLive, PhysicalDB: "argon_br_b1"}, nil
	}
	return nil, errors.New("branch not found")
}

func (s *round2Store) GetBranchByID(id string) (*wal.Branch, error) {
	s.reads++
	if s.down {
		return nil, errRound2Unreachable
	}
	if id == "b1" {
		return &wal.Branch{ID: "b1", Name: "main", State: wal.BranchStateLive, PhysicalDB: "argon_br_b1"}, nil
	}
	return nil, errors.New("branch not found")
}

// Round 2: resolve() now reads the branch by ID on every use of a cached
// alias and takes ANY error from that read as "the branch is gone or was
// released: resolve the name afresh". When the read failed because the
// metadata store could not be reached, the fresh lookup fails the same way,
// lookup() reports it as `project "shop" not found`, and that answer is
// cached for a second. So one failed metadata read turns an alias that
// resolved a moment ago into NamespaceNotFound "project not found" for every
// command on it, and keeps it there for a second after the store is back.
//
// Before 88d08c9 a resolved alias was served from the cache for five seconds
// without a metadata read, so the same failure was not seen at all here.
// Either answer would do: serve the alias it resolved a moment ago, or fail
// the command with what happened and try again on the next one.
func TestRound2_AMetadataReadErrorIsNotReportedAsAMissingProject(t *testing.T) {
	store := &round2Store{}
	proxy := New("127.0.0.1:1", store, store)

	physical, err := proxy.resolve("shop~main")
	require.NoError(t, err)
	require.Equal(t, "argon_br_b1", physical)

	// The metadata store cannot be reached for one command.
	store.down = true
	_, during := proxy.resolve("shop~main")
	store.down = false
	if during != nil {
		assert.NotContains(t, during.Error(), "not found",
			"the store could not be reached; the client is told its project does not exist")
	}

	// The store is back: the next command, sent at once, goes through.
	physical, after := proxy.resolve("shop~main")
	require.NoError(t, after, "the store is reachable again, and the alias still answers with the cached failure")
	assert.Equal(t, "argon_br_b1", physical)
}
