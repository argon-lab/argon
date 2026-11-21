package walcli

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/v2/internal/wal"
)

// ProjectMetrics is one project's counts, read from the metadata database.
type ProjectMetrics struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	// Branches counts branches that are not deleted; Sandboxes and
	// CheckedOut count the branches among them that have a TTL and that are
	// checked out to a physical database.
	Branches   int `json:"branches"`
	Sandboxes  int `json:"sandboxes"`
	CheckedOut int `json:"checked_out"`
	// CurrentLSN is the newest LSN the project has allocated. An allocated
	// LSN is not necessarily written yet.
	CurrentLSN int64 `json:"current_lsn"`
}

// Metrics totals ProjectMetrics over every project.
type Metrics struct {
	Projects   []ProjectMetrics `json:"projects"`
	Branches   int              `json:"branches"`
	Sandboxes  int              `json:"sandboxes"`
	CheckedOut int              `json:"checked_out"`
}

// ReadMetrics counts what the metadata database holds now. It keeps no
// counters of its own, so every process reports the same figures.
func (s *Services) ReadMetrics(ctx context.Context) (Metrics, error) {
	projects, err := s.Projects.ListProjectsContext(ctx)
	if err != nil {
		return Metrics{}, fmt.Errorf("failed to list projects: %w", err)
	}
	result := Metrics{Projects: []ProjectMetrics{}}
	for _, project := range projects {
		row := ProjectMetrics{Name: project.Name, ID: project.ID}
		branches, err := s.Branches.ListBranchesContext(ctx, project.ID)
		if err != nil {
			return Metrics{}, fmt.Errorf("failed to list branches of project %q: %w", project.Name, err)
		}
		for _, branch := range branches {
			if branch.IsDeleted {
				continue
			}
			row.Branches++
			if branch.ExpiresAt != nil {
				row.Sandboxes++
			}
			if branch.State == wal.BranchStateLive {
				row.CheckedOut++
			}
		}
		if row.CurrentLSN, err = s.WAL.CurrentLSN(project.ID); err != nil {
			return Metrics{}, err
		}
		result.Projects = append(result.Projects, row)
		result.Branches += row.Branches
		result.Sandboxes += row.Sandboxes
		result.CheckedOut += row.CheckedOut
	}
	return result, nil
}
