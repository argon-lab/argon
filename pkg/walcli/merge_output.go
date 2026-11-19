package walcli

import (
	"context"
	"regexp"
	"strings"

	"github.com/argon-lab/argon/v2/internal/merge"
)

// FollowedMergeTargets returns the live database names used by merge displays.
// A plan may outlive its target's source binding.
func (s *Services) FollowedMergeTargets(ctx context.Context, projectID string) (map[string]string, error) {
	branches, err := s.Branches.ListBranchesContext(ctx, projectID)
	if err != nil {
		return nil, err
	}
	followed := make(map[string]string)
	for _, branch := range branches {
		if branch.IsFollower() {
			followed[branch.ID] = branch.Follows
		}
	}
	return followed, nil
}

var simpleProjectArgument = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// WriteBackReviewAdvice directs operators to the saved images for this project.
func WriteBackReviewAdvice(project string) string {
	switch {
	case project == "":
		project = "<project>"
	case !simpleProjectArgument.MatchString(project):
		project = "'" + strings.ReplaceAll(project, "'", "'\\''") + "'"
	}
	return "For write-back, inspect every image with argon merge list -p " + project + " --output json and use the approval_digest for this plan."
}

// MergePlanView is the summary returned by the REST plan endpoints.
type MergePlanView struct {
	*merge.Plan
	WriteBackGuidance string `json:"write_back_guidance,omitempty"`
}

func ViewMergePlan(plan *merge.Plan, project string, followed bool) MergePlanView {
	view := MergePlanView{Plan: plan}
	if followed {
		view.WriteBackGuidance = WriteBackReviewAdvice(project)
	}
	return view
}
