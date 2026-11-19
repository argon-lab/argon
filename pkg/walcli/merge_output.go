package walcli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/argon-lab/argon/v2/internal/merge"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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

// MarshalJSON keeps the ordinary REST representation for finite numbers.
// A non-finite double needs an Extended JSON wrapper, including in arrays,
// ordered compound keys and JavaScript scopes.
func (view MergePlanView) MarshalJSON() ([]byte, error) {
	type plainView MergePlanView
	data, err := json.Marshal(plainView(view))
	var unsupported *json.UnsupportedValueError
	if !errors.As(err, &unsupported) || unsupported.Value.Kind() != reflect.Float64 {
		return data, err
	}
	plan := *view.Plan
	plan.Changes = slices.Clone(plan.Changes)
	for i := range plan.Changes {
		change := &plan.Changes[i]
		change.ID = restBSONValue(change.ID)
		change.Before = restDocument(change.Before)
		change.Document = restDocument(change.Document)
	}
	plan.Conflicts = slices.Clone(plan.Conflicts)
	for i := range plan.Conflicts {
		conflict := &plan.Conflicts[i]
		conflict.Base = restDocument(conflict.Base)
		conflict.Ours = restDocument(conflict.Ours)
		conflict.Theirs = restDocument(conflict.Theirs)
	}
	view.Plan = &plan
	return json.Marshal(plainView(view))
}

func restDocument(doc bson.M) bson.M {
	cloned := maps.Clone(doc)
	for key, val := range doc {
		cloned[key] = restBSONValue(val)
	}
	return cloned
}

func restBSONValue(value any) any {
	switch v := value.(type) {
	case float64:
		switch {
		case math.IsNaN(v):
			return bson.M{"$numberDouble": "NaN"}
		case math.IsInf(v, 1):
			return bson.M{"$numberDouble": "Infinity"}
		case math.IsInf(v, -1):
			return bson.M{"$numberDouble": "-Infinity"}
		}
	case bson.M:
		return restDocument(v)
	case bson.A:
		v = slices.Clone(v)
		for i := range v {
			v[i] = restBSONValue(v[i])
		}
		return v
	case bson.D:
		v = slices.Clone(v)
		for i := range v {
			v[i].Value = restBSONValue(v[i].Value)
		}
		return v
	case primitive.CodeWithScope:
		v.Scope = restBSONValue(v.Scope)
		return v
	}
	return value
}
