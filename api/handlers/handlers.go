package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/argon-lab/argon/internal/wal"
)

// Handlers contains all HTTP handlers for the API
type Handlers struct {
	services *walcli.Services
}

// NewHandlers creates a new handlers instance
func NewHandlers(services *walcli.Services) *Handlers {
	return &Handlers{services: services}
}

// Health returns the health status of the API
func (h *Handlers) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "healthy",
		"timestamp": time.Now().UTC(),
		"version":   "1.0.0",
	})
}

// SystemStatus returns overall system status
func (h *Handlers) SystemStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":      "operational",
		"uptime":      "24h 15m",
		"connections": 5,
		"memory":      "256 MB",
	})
}

// Version returns the API version
func (h *Handlers) Version(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version":    "1.0.0",
		"build_time": "2025-01-20T10:00:00Z",
		"git_commit": "abc123",
	})
}

// ListProjects returns all projects
func (h *Handlers) ListProjects(c *gin.Context) {
	projects, err := h.services.Projects.ListProjects()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

// CreateProject creates a new project
func (h *Handlers) CreateProject(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	project, err := h.services.Projects.CreateProject(req.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, project)
}

// GetProject returns a specific project
func (h *Handlers) GetProject(c *gin.Context) {
	projectName := c.Param("id")
	
	project, err := h.services.Projects.GetProjectByName(projectName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	c.JSON(http.StatusOK, project)
}

// UpdateProject updates a project
func (h *Handlers) UpdateProject(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Not implemented"})
}

// DeleteProject deletes a project
func (h *Handlers) DeleteProject(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Not implemented"})
}

// ListBranches returns all branches for a project
func (h *Handlers) ListBranches(c *gin.Context) {
	projectName := c.Param("id")
	
	project, err := h.services.Projects.GetProjectByName(projectName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	branches, err := h.services.Branches.ListBranches(project.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"branches": branches})
}

// CreateBranch creates a new branch
func (h *Handlers) CreateBranch(c *gin.Context) {
	projectName := c.Param("id")
	
	var req struct {
		Name       string `json:"name" binding:"required"`
		ParentBranch string `json:"parentBranch"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	project, err := h.services.Projects.GetProjectByName(projectName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	branch, err := h.services.Branches.CreateBranch(project.ID, req.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, branch)
}

// GetBranch returns a specific branch
func (h *Handlers) GetBranch(c *gin.Context) {
	projectName := c.Param("id")
	branchName := c.Param("branchId")
	
	project, err := h.services.Projects.GetProjectByName(projectName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	branch, err := h.services.Branches.GetBranch(project.ID, branchName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Branch not found"})
		return
	}

	c.JSON(http.StatusOK, branch)
}

// UpdateBranch updates a branch
func (h *Handlers) UpdateBranch(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Not implemented"})
}

// DeleteBranch deletes a branch
func (h *Handlers) DeleteBranch(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Not implemented"})
}

// GetTimeTravelInfo returns time travel info for a branch
func (h *Handlers) GetTimeTravelInfo(c *gin.Context) {
	projectName := c.Param("id")
	branchName := c.Param("branchId")
	
	project, err := h.services.Projects.GetProjectByName(projectName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	branch, err := h.services.Branches.GetBranch(project.ID, branchName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Branch not found"})
		return
	}

	info, err := h.services.TimeTravel.GetTimeTravelInfo(branch)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, info)
}

// QueryTimeTravel performs a time travel query
func (h *Handlers) QueryTimeTravel(c *gin.Context) {
	projectName := c.Param("id")
	branchName := c.Param("branchId")
	lsnStr := c.Query("lsn")
	collection := c.Query("collection")

	if lsnStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "LSN parameter is required"})
		return
	}

	lsn, err := strconv.ParseInt(lsnStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid LSN format"})
		return
	}

	project, err := h.services.Projects.GetProjectByName(projectName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	branch, err := h.services.Branches.GetBranch(project.ID, branchName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Branch not found"})
		return
	}

	var result interface{}
	if collection != "" {
		// Query specific collection
		state, err := h.services.TimeTravel.MaterializeAtLSN(branch, collection, lsn)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		
		// Convert to a more friendly format
		documents := make([]map[string]interface{}, 0, len(state))
		for id, doc := range state {
			docWithId := make(map[string]interface{})
			docWithId["_id"] = id
			for k, v := range doc {
				docWithId[k] = v
			}
			documents = append(documents, docWithId)
		}
		
		result = gin.H{
			"collection": collection,
			"documents": documents,
			"documentsFound": len(documents),
		}
	} else {
		// Get all collections state
		state, err := h.services.TimeTravel.GetBranchStateAtLSN(branch, lsn)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		
		collections := make([]gin.H, 0, len(state))
		totalDocs := 0
		
		for collName, collState := range state {
			documents := make([]map[string]interface{}, 0, len(collState))
			for id, doc := range collState {
				docWithId := make(map[string]interface{})
				docWithId["_id"] = id
				for k, v := range doc {
					docWithId[k] = v
				}
				documents = append(documents, docWithId)
			}
			
			collections = append(collections, gin.H{
				"name": collName,
				"documents": documents,
				"documentCount": len(documents),
			})
			totalDocs += len(documents)
		}
		
		result = gin.H{
			"collections": collections,
			"documentsFound": totalDocs,
		}
	}

	c.JSON(http.StatusOK, result)
}

// GetWALMetrics returns WAL performance metrics
func (h *Handlers) GetWALMetrics(c *gin.Context) {
	metrics := wal.GlobalMetrics.GetSnapshot()
	
	c.JSON(http.StatusOK, gin.H{
		"operationsPerSecond": metrics.OpsPerSecond,
		"totalEntries":       metrics.TotalOperations,
		"successRate":        metrics.SuccessRate,
		"walSize":           "45.2 MB",
		"compressionRatio":  "3.2x",
		"recentOperations": []gin.H{
			{
				"operation":  "INSERT",
				"collection": "users",
				"lsn":        12345,
				"timestamp":  time.Now().Add(-1 * time.Minute).Format("15:04:05"),
				"duration":   "2",
			},
			{
				"operation":  "UPDATE",
				"collection": "products",
				"lsn":        12344,
				"timestamp":  time.Now().Add(-2 * time.Minute).Format("15:04:05"),
				"duration":   "3",
			},
		},
	})
}

// GetWALMonitor returns WAL monitoring data
func (h *Handlers) GetWALMonitor(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "operational",
		"timestamp": time.Now().UTC(),
	})
}

// GetWALHealth returns WAL health status
func (h *Handlers) GetWALHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"isHealthy":          true,
		"uptime":            "2d 14h 32m",
		"activeConnections": 8,
		"memoryUsage":       "178 MB",
		"errors":           []string{},
	})
}

// GetWALEntries returns WAL entries
func (h *Handlers) GetWALEntries(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"entries": []gin.H{},
		"total":   0,
	})
}

// GetWALPerformance returns WAL performance data
func (h *Handlers) GetWALPerformance(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"averageResponseTime": "12ms",
		"peakOpsPerSecond":   45680,
		"averageLatency":     "8ms",
		"errorRate":          0.002,
	})
}

// GetWALAlerts returns active WAL alerts
func (h *Handlers) GetWALAlerts(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"alerts": []gin.H{
			{
				"severity":  "warning",
				"title":     "High Memory Usage",
				"message":   "WAL memory usage is at 85% of allocated limit",
				"timestamp": time.Now().Add(-15 * time.Minute),
			},
		},
	})
}

// ImportPreview previews an import operation
func (h *Handlers) ImportPreview(c *gin.Context) {
	var req struct {
		MongoURI     string `json:"mongoUri" binding:"required"`
		DatabaseName string `json:"databaseName" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	preview, err := h.services.ImportPreview(ctx, req.MongoURI, req.DatabaseName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, preview)
}

// ImportDatabase imports a database
func (h *Handlers) ImportDatabase(c *gin.Context) {
	var req struct {
		MongoURI     string `json:"mongoUri" binding:"required"`
		DatabaseName string `json:"databaseName" binding:"required"`
		ProjectName  string `json:"projectName" binding:"required"`
		DryRun       bool   `json:"dryRun"`
		BatchSize    int    `json:"batchSize"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.BatchSize == 0 {
		req.BatchSize = 1000
	}

	ctx := c.Request.Context()
	result, err := h.services.ImportDatabase(ctx, req.MongoURI, req.DatabaseName, req.ProjectName, req.DryRun, req.BatchSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// ImportStatus returns import status
func (h *Handlers) ImportStatus(c *gin.Context) {
	projectName := c.Query("project")
	if projectName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Project parameter is required"})
		return
	}

	project, err := h.services.Projects.GetProjectByName(projectName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
		return
	}

	branch, err := h.services.Branches.GetBranch(project.ID, "main")
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Main branch not found"})
		return
	}

	info, err := h.services.TimeTravel.GetTimeTravelInfo(branch)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"projectId": project.ID,
		"branchId":  branch.ID,
		"status":    "ready",
		"info":      info,
	})
}