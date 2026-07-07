// Package main serves Argon's REST API — the control plane for language
// SDKs (the Python agent adapters foremost): projects, branches, sandboxes,
// checkout/connection strings, diff/merge, undo and time travel. The data
// plane stays native MongoDB — clients write to branch databases through
// their own drivers; this server supervises a change-stream ingester for
// every branch it checks out, so those writes become versioned history.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/argon-lab/argon/pkg/walcli"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Router wraps the gin engine with ingester supervision.
type Router struct {
	*gin.Engine
	services *walcli.Services

	ingestMu sync.Mutex
	ingest   map[string]context.CancelFunc
}

// NewRouter builds the API over the given services.
func NewRouter(services *walcli.Services) *Router {
	r := &Router{
		Engine:   gin.New(),
		services: services,
		ingest:   make(map[string]context.CancelFunc),
	}
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	{
		v1.GET("/projects", r.listProjects)
		v1.POST("/projects", r.createProject)

		v1.GET("/projects/:project/branches", r.listBranches)
		v1.POST("/projects/:project/branches", r.createBranch)
		v1.GET("/projects/:project/branches/:branch", r.getBranch)
		v1.DELETE("/projects/:project/branches/:branch", r.deleteBranch)

		v1.POST("/projects/:project/branches/:branch/checkout", r.checkoutBranch)
		v1.POST("/projects/:project/branches/:branch/release", r.releaseBranch)

		v1.POST("/projects/:project/sandboxes", r.createSandbox)

		v1.GET("/projects/:project/branches/:branch/diff", r.diffBranch)
		v1.POST("/projects/:project/branches/:branch/merge-preview", r.mergePreview)
		v1.POST("/merge-plans/:id/apply", r.mergeApply)

		v1.POST("/projects/:project/branches/:branch/undo", r.undoRange)
		v1.GET("/projects/:project/branches/:branch/time-travel", r.timeTravelInfo)
		v1.POST("/projects/:project/branches/:branch/snapshots", r.createSnapshot)
	}
	return r
}

// Shutdown stops every supervised ingester.
func (r *Router) Shutdown() {
	r.ingestMu.Lock()
	defer r.ingestMu.Unlock()
	for id, cancel := range r.ingest {
		cancel()
		delete(r.ingest, id)
	}
}

func (r *Router) startIngester(branchID string) {
	r.ingestMu.Lock()
	defer r.ingestMu.Unlock()
	if _, running := r.ingest[branchID]; running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.ingest[branchID] = cancel
	go func() {
		if err := r.services.Ingest.Run(ctx, branchID); err != nil && ctx.Err() == nil {
			log.Printf("api: ingester for branch %s stopped: %v", branchID, err)
		}
	}()
}

func (r *Router) stopIngester(branchID string) {
	r.ingestMu.Lock()
	defer r.ingestMu.Unlock()
	if cancel, ok := r.ingest[branchID]; ok {
		cancel()
		delete(r.ingest, branchID)
	}
}

// --- helpers ---

func abortErr(c *gin.Context, status int, err error) {
	c.JSON(status, gin.H{"error": err.Error()})
}

func (r *Router) resolve(c *gin.Context) (projectID, branchID string, ok bool) {
	project, err := r.services.Projects.GetProjectByName(c.Param("project"))
	if err != nil {
		abortErr(c, http.StatusNotFound, fmt.Errorf("project %q not found", c.Param("project")))
		return "", "", false
	}
	branchName := c.Param("branch")
	if branchName == "" {
		return project.ID, "", true
	}
	branch, err := r.services.Branches.GetBranch(project.ID, branchName)
	if err != nil {
		abortErr(c, http.StatusNotFound, fmt.Errorf("branch %q not found", branchName))
		return "", "", false
	}
	return project.ID, branch.ID, true
}

// --- projects ---

func (r *Router) listProjects(c *gin.Context) {
	projects, err := r.services.Projects.ListProjects()
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

func (r *Router) createProject(c *gin.Context) {
	var body struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	project, err := r.services.Projects.CreateProject(body.Name)
	if err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusCreated, project)
}

// --- branches ---

func (r *Router) listBranches(c *gin.Context) {
	projectID, _, ok := r.resolve(c)
	if !ok {
		return
	}
	branches, err := r.services.Branches.ListBranches(projectID)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"branches": branches})
}

func (r *Router) createBranch(c *gin.Context) {
	projectID, _, ok := r.resolve(c)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name" binding:"required"`
		From string `json:"from"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	parentID := ""
	if body.From != "" {
		parent, err := r.services.Branches.GetBranch(projectID, body.From)
		if err != nil {
			abortErr(c, http.StatusNotFound, fmt.Errorf("parent branch %q not found", body.From))
			return
		}
		parentID = parent.ID
	}
	branch, err := r.services.Branches.CreateBranch(projectID, body.Name, parentID)
	if err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusCreated, branch)
}

func (r *Router) getBranch(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	branch, err := r.services.Branches.GetBranchByID(branchID)
	if err != nil {
		abortErr(c, http.StatusNotFound, err)
		return
	}
	resp := gin.H{"branch": branch}
	if branch.IsLive() {
		resp["connection_string"] = r.services.BranchConnectionString(branch.PhysicalDB)
	}
	c.JSON(http.StatusOK, resp)
}

func (r *Router) deleteBranch(c *gin.Context) {
	projectID, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	r.stopIngester(branchID)
	if err := r.services.Sandbox.Discard(c.Request.Context(), branchID); err != nil {
		// Fall back for non-sandbox branches that are not checked out.
		if err2 := r.services.Branches.DeleteBranch(projectID, c.Param("branch")); err2 != nil {
			abortErr(c, http.StatusConflict, err2)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// --- checkout / connection strings ---

func (r *Router) checkoutBranch(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	info, err := r.services.Checkout.Checkout(c.Request.Context(), branchID)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	r.startIngester(branchID)
	c.JSON(http.StatusOK, gin.H{
		"connection_string": r.services.BranchConnectionString(info.PhysicalDB),
		"physical_db":       info.PhysicalDB,
		"lsn":               info.LSN,
		"collections":       info.Collections,
		"documents":         info.Documents,
	})
}

func (r *Router) releaseBranch(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	r.stopIngester(branchID)
	if err := r.services.Checkout.Release(c.Request.Context(), branchID); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"released": true})
}

// --- sandboxes ---

func (r *Router) createSandbox(c *gin.Context) {
	projectID, _, ok := r.resolve(c)
	if !ok {
		return
	}
	var body struct {
		From       string  `json:"from"`
		Name       string  `json:"name"`
		TTLMinutes float64 `json:"ttl_minutes"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && err.Error() != "EOF" {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	from := body.From
	if from == "" {
		from = "main"
	}
	parent, err := r.services.Branches.GetBranch(projectID, from)
	if err != nil {
		abortErr(c, http.StatusNotFound, fmt.Errorf("parent branch %q not found", from))
		return
	}
	ttl := time.Duration(body.TTLMinutes) * time.Minute
	info, err := r.services.Sandbox.Create(c.Request.Context(), projectID, parent.ID, body.Name, ttl)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	r.startIngester(info.BranchID)
	c.JSON(http.StatusCreated, gin.H{
		"branch":            info.BranchName,
		"connection_string": r.services.BranchConnectionString(info.PhysicalDB),
		"expires_at":        info.ExpiresAt,
		"forked_from":       info.ForkedFrom,
		"fork_lsn":          info.ForkLSN,
	})
}

// --- diff / merge ---

func (r *Router) diffBranch(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	plan, err := r.services.Merge.Compute(branchID)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	c.JSON(http.StatusOK, plan)
}

func (r *Router) mergePreview(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	plan, err := r.services.Merge.Preview(c.Request.Context(), branchID)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	c.JSON(http.StatusCreated, plan)
}

func (r *Router) mergeApply(c *gin.Context) {
	planID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		abortErr(c, http.StatusBadRequest, fmt.Errorf("invalid plan id"))
		return
	}
	var body struct {
		Strategy string `json:"strategy"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && err.Error() != "EOF" {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	result, err := r.services.Merge.Apply(c.Request.Context(), planID, body.Strategy)
	if err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"applied": result.Applied, "conflicts_resolved": result.ConflictsResolved})
}

// --- undo / time travel / snapshots ---

func (r *Router) undoRange(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	var body struct {
		FromLSN int64  `json:"from_lsn" binding:"required"`
		ToLSN   int64  `json:"to_lsn"`
		Actor   string `json:"actor"`
		DryRun  bool   `json:"dry_run"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	plan, err := r.services.BuildUndoPlan(branchID, body.FromLSN, body.ToLSN, body.Actor)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	resp := gin.H{
		"from_lsn":      plan.FromLSN,
		"to_lsn":        plan.ToLSN,
		"compensations": len(plan.Compensations),
		"conflicts":     len(plan.Conflicts),
		"unrecoverable": len(plan.Unrecoverable),
		"dry_run":       body.DryRun,
	}
	if !body.DryRun {
		restored, deleted, err := r.services.ApplyUndoPlan(c.Request.Context(), branchID, plan)
		if err != nil {
			abortErr(c, http.StatusInternalServerError, err)
			return
		}
		resp["restored"] = restored
		resp["deleted"] = deleted
	}
	c.JSON(http.StatusOK, resp)
}

func (r *Router) timeTravelInfo(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	branch, err := r.services.Branches.GetBranchByID(branchID)
	if err != nil {
		abortErr(c, http.StatusNotFound, err)
		return
	}
	info, err := r.services.TimeTravel.GetTimeTravelInfo(branch)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, info)
}

func (r *Router) createSnapshot(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	branch, err := r.services.Branches.GetBranchByID(branchID)
	if err != nil {
		abortErr(c, http.StatusNotFound, err)
		return
	}
	snaps, err := r.services.Snapshots.CreateSnapshot(c.Request.Context(), branchID, branch.HeadLSN)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"snapshots": len(snaps), "lsn": branch.HeadLSN})
}
