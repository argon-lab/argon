package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// toolHandler executes one tool call and returns the text result.
type toolHandler func(ctx context.Context, s *Server, args map[string]interface{}) (string, error)

var toolHandlers = map[string]toolHandler{
	"argon_sandbox_create":  toolSandboxCreate,
	"argon_sandbox_discard": toolSandboxDiscard,
	"argon_sandbox_keep":    toolSandboxKeep,
	"argon_branch_list":     toolBranchList,
	"argon_connect":         toolConnect,
	"argon_diff":            toolDiff,
	"argon_merge_preview":   toolMergePreview,
	"argon_merge_apply":     toolMergeApply,
	"argon_undo":            toolUndo,
	"argon_snapshot_create": toolSnapshotCreate,
}

// schema builds the JSON schema for a tool's arguments.
func schema(required []string, props map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func str(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func num(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "number", "description": desc}
}

func boolean(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

// toolDescriptors lists the tools for tools/list.
func toolDescriptors() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name": "argon_sandbox_create",
			"description": "Fork an isolated, disposable copy of a MongoDB branch and get a connection string. " +
				"Work against it with any MongoDB driver; every write is captured as versioned history. " +
				"Merge the result back, undo it, or let the TTL reclaim it.",
			"inputSchema": schema([]string{"project"}, map[string]interface{}{
				"project":     str("Project name"),
				"from":        str("Parent branch to fork (default: main)"),
				"name":        str("Sandbox name (default: generated)"),
				"ttl_minutes": num("Minutes until the sandbox is reclaimed (default: 60)"),
			}),
		},
		{
			"name":        "argon_sandbox_discard",
			"description": "Delete a sandbox immediately and reclaim its storage. Unmerged changes are lost.",
			"inputSchema": schema([]string{"project", "branch"}, map[string]interface{}{
				"project": str("Project name"),
				"branch":  str("Sandbox branch name"),
			}),
		},
		{
			"name":        "argon_sandbox_keep",
			"description": "Remove a sandbox's TTL, keeping it as a permanent branch.",
			"inputSchema": schema([]string{"project", "branch"}, map[string]interface{}{
				"project": str("Project name"),
				"branch":  str("Sandbox branch name"),
			}),
		},
		{
			"name":        "argon_branch_list",
			"description": "List a project's branches with their heads, checkout state and TTLs.",
			"inputSchema": schema([]string{"project"}, map[string]interface{}{
				"project": str("Project name"),
			}),
		},
		{
			"name": "argon_connect",
			"description": "Get a MongoDB connection string for a branch, checking it out if needed. " +
				"Writes through that connection become versioned history.",
			"inputSchema": schema([]string{"project", "branch"}, map[string]interface{}{
				"project": str("Project name"),
				"branch":  str("Branch name"),
			}),
		},
		{
			"name":        "argon_diff",
			"description": "Show what merging a branch into its parent would change: adopted documents and conflicts.",
			"inputSchema": schema([]string{"project", "branch"}, map[string]interface{}{
				"project": str("Project name"),
				"branch":  str("Branch to compare against its parent"),
			}),
		},
		{
			"name":        "argon_merge_preview",
			"description": "Compute and persist a reviewable merge plan (a data pull request). Returns the plan ID.",
			"inputSchema": schema([]string{"project", "branch"}, map[string]interface{}{
				"project": str("Project name"),
				"branch":  str("Branch to merge into its parent"),
			}),
		},
		{
			"name":        "argon_merge_apply",
			"description": "Apply a pending merge plan. Conflicted plans need a strategy: theirs (take the branch) or ours (keep the target).",
			"inputSchema": schema([]string{"plan_id"}, map[string]interface{}{
				"plan_id":  str("Plan ID from argon_merge_preview"),
				"strategy": str("Conflict resolution: theirs or ours"),
			}),
		},
		{
			"name": "argon_undo",
			"description": "Revert a range of a branch's history: every touched document returns to its state " +
				"before the range. Optionally restricted to one actor's writes. Use dry_run to preview.",
			"inputSchema": schema([]string{"project", "branch", "from_lsn"}, map[string]interface{}{
				"project":  str("Project name"),
				"branch":   str("Branch name"),
				"from_lsn": num("Start of the range to revert"),
				"to_lsn":   num("End of the range (default: branch head)"),
				"actor":    str("Only revert writes by this actor"),
				"dry_run":  boolean("Preview without applying"),
			}),
		},
		{
			"name":        "argon_snapshot_create",
			"description": "Snapshot a branch at its current head so reads replay only the delta above it.",
			"inputSchema": schema([]string{"project", "branch"}, map[string]interface{}{
				"project": str("Project name"),
				"branch":  str("Branch name"),
			}),
		},
	}
}

// --- argument helpers ---

func argString(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argNumber(args map[string]interface{}, key string) (float64, bool) {
	v, ok := args[key].(float64)
	return v, ok
}

func argBool(args map[string]interface{}, key string) bool {
	v, _ := args[key].(bool)
	return v
}

func (s *Server) resolveBranchID(project, branch string) (projectID, branchID string, err error) {
	p, err := s.services.Projects.GetProjectByName(project)
	if err != nil {
		return "", "", fmt.Errorf("project %q not found", project)
	}
	if branch == "" {
		branch = "main"
	}
	b, err := s.services.Branches.GetBranch(p.ID, branch)
	if err != nil {
		return "", "", fmt.Errorf("branch %q not found in project %q", branch, project)
	}
	return p.ID, b.ID, nil
}

// --- tool implementations ---

func toolSandboxCreate(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	project := argString(args, "project")
	projectID, parentID, err := s.resolveBranchID(project, argString(args, "from"))
	if err != nil {
		return "", err
	}

	ttl := time.Hour
	if minutes, ok := argNumber(args, "ttl_minutes"); ok && minutes > 0 {
		ttl = time.Duration(minutes) * time.Minute
	}

	info, err := s.services.Sandbox.Create(ctx, projectID, parentID, argString(args, "name"), ttl)
	if err != nil {
		return "", fmt.Errorf("sandbox creation failed: %w", err)
	}
	s.startIngester(info.BranchID)

	return fmt.Sprintf(
		"Sandbox %q created (forked from %s at LSN %d).\n"+
			"Connection string: %s\n"+
			"Expires: %s\n"+
			"Writes through this connection are captured as versioned history. "+
			"Merge back with argon_merge_preview/apply, revert with argon_undo, or discard with argon_sandbox_discard.",
		info.BranchName, info.ForkedFrom, info.ForkLSN,
		s.services.BranchConnectionString(info.PhysicalDB),
		info.ExpiresAt.Format(time.RFC3339),
	), nil
}

func toolSandboxDiscard(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	_, branchID, err := s.resolveBranchID(argString(args, "project"), argString(args, "branch"))
	if err != nil {
		return "", err
	}
	s.stopIngester(branchID)
	if err := s.services.Sandbox.Discard(ctx, branchID); err != nil {
		return "", fmt.Errorf("discard failed: %w", err)
	}
	return "Sandbox discarded; storage reclaimed.", nil
}

func toolSandboxKeep(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	_, branchID, err := s.resolveBranchID(argString(args, "project"), argString(args, "branch"))
	if err != nil {
		return "", err
	}
	if err := s.services.Sandbox.Keep(ctx, branchID); err != nil {
		return "", err
	}
	return "TTL removed; the branch is permanent now.", nil
}

func toolBranchList(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	project, err := s.services.Projects.GetProjectByName(argString(args, "project"))
	if err != nil {
		return "", fmt.Errorf("project %q not found", argString(args, "project"))
	}
	branches, err := s.services.Branches.ListBranches(project.ID)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, b := range branches {
		fmt.Fprintf(&sb, "%s: head LSN %d", b.Name, b.HeadLSN)
		if b.IsLive() {
			fmt.Fprintf(&sb, ", checked out as %s", b.PhysicalDB)
		}
		if b.ExpiresAt != nil {
			fmt.Fprintf(&sb, ", expires %s", b.ExpiresAt.Format(time.RFC3339))
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

func toolConnect(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	_, branchID, err := s.resolveBranchID(argString(args, "project"), argString(args, "branch"))
	if err != nil {
		return "", err
	}
	branch, err := s.services.Branches.GetBranchByID(branchID)
	if err != nil {
		return "", err
	}
	if !branch.IsLive() {
		info, err := s.services.Checkout.Checkout(ctx, branchID)
		if err != nil {
			return "", fmt.Errorf("checkout failed: %w", err)
		}
		branch.PhysicalDB = info.PhysicalDB
	}
	s.startIngester(branchID)
	return fmt.Sprintf("Connection string: %s\nWrites through this connection are captured as versioned history.",
		s.services.BranchConnectionString(branch.PhysicalDB)), nil
}

func formatPlanSummary(sb *strings.Builder, changes, conflicts int) {
	fmt.Fprintf(sb, "%d change(s), %d conflict(s)\n", changes, conflicts)
}

func toolDiff(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	_, branchID, err := s.resolveBranchID(argString(args, "project"), argString(args, "branch"))
	if err != nil {
		return "", err
	}
	plan, err := s.services.Merge.Compute(branchID)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Merging %s into %s would:\n", plan.SourceBranch, plan.TargetBranch)
	for _, c := range plan.Changes {
		action := "put"
		if c.Delete {
			action = "delete"
		}
		fmt.Fprintf(&sb, "  %s %s/%s\n", action, c.Collection, c.DocumentID)
	}
	for _, c := range plan.Conflicts {
		fmt.Fprintf(&sb, "  CONFLICT %s/%s (both sides changed since the fork)\n", c.Collection, c.DocumentID)
	}
	formatPlanSummary(&sb, len(plan.Changes), len(plan.Conflicts))
	return sb.String(), nil
}

func toolMergePreview(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	_, branchID, err := s.resolveBranchID(argString(args, "project"), argString(args, "branch"))
	if err != nil {
		return "", err
	}
	plan, err := s.services.Merge.Preview(ctx, branchID)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Merge plan %s (%s into %s): ", plan.ID.Hex(), plan.SourceBranch, plan.TargetBranch)
	formatPlanSummary(&sb, len(plan.Changes), len(plan.Conflicts))
	if len(plan.Conflicts) > 0 {
		sb.WriteString("Apply with argon_merge_apply and strategy theirs or ours.")
	} else {
		sb.WriteString("Apply with argon_merge_apply.")
	}
	return sb.String(), nil
}

func toolMergeApply(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	planID, err := primitive.ObjectIDFromHex(argString(args, "plan_id"))
	if err != nil {
		return "", fmt.Errorf("invalid plan_id")
	}
	result, err := s.services.Merge.Apply(ctx, planID, argString(args, "strategy"))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Merged: %d change(s) applied, %d conflict(s) resolved.", result.Applied, result.ConflictsResolved), nil
}

func toolUndo(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	_, branchID, err := s.resolveBranchID(argString(args, "project"), argString(args, "branch"))
	if err != nil {
		return "", err
	}
	fromLSN, ok := argNumber(args, "from_lsn")
	if !ok {
		return "", fmt.Errorf("from_lsn is required")
	}
	toLSN, _ := argNumber(args, "to_lsn")

	plan, err := s.services.BuildUndoPlan(branchID, int64(fromLSN), int64(toLSN), argString(args, "actor"))
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Undo [%d, %d]: %d document(s) to revert, %d conflict(s), %d unrecoverable.\n",
		plan.FromLSN, plan.ToLSN, len(plan.Compensations), len(plan.Conflicts), len(plan.Unrecoverable))

	if argBool(args, "dry_run") {
		sb.WriteString("Dry run: nothing applied.")
		return sb.String(), nil
	}

	restored, deleted, err := s.services.ApplyUndoPlan(ctx, branchID, plan)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&sb, "Done: %d restored, %d deleted.", restored, deleted)
	return sb.String(), nil
}

func toolSnapshotCreate(ctx context.Context, s *Server, args map[string]interface{}) (string, error) {
	_, branchID, err := s.resolveBranchID(argString(args, "project"), argString(args, "branch"))
	if err != nil {
		return "", err
	}
	branch, err := s.services.Branches.GetBranchByID(branchID)
	if err != nil {
		return "", err
	}
	snaps, err := s.services.Snapshots.CreateSnapshot(ctx, branchID, branch.HeadLSN)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Snapshotted %d collection(s) at LSN %d.", len(snaps), branch.HeadLSN), nil
}
