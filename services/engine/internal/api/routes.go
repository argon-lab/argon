package api

import (
	"net/http"
	"strconv"

	"argon/engine/internal/branch"
	"argon/engine/internal/streams"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Handlers struct {
	branchService  *branch.Service
	streamsService *streams.Service
}

func SetupRoutes(router *gin.Engine, branchService *branch.Service, streamsService *streams.Service) {
	h := &Handlers{
		branchService:  branchService,
		streamsService: streamsService,
	}

	// Health check
	router.GET("/health", h.healthCheck)

	// API v1 routes
	v1 := router.Group("/api/v1")
	{
		// Branch operations
		branches := v1.Group("/branches")
		{
			branches.POST("", h.createBranch)
			branches.GET("/:id", h.getBranch)
			branches.PUT("/:id", h.updateBranch)
			branches.DELETE("/:id", h.deleteBranch)
			branches.GET("/:id/stats", h.getBranchStats)
		}

		// Project operations
		projects := v1.Group("/projects")
		{
			projects.GET("/:id/branches", h.listBranches)
			projects.POST("/:id/switch/:branchId", h.switchBranch)
		}

		// Change streams
		streams := v1.Group("/streams")
		{
			streams.GET("/:branchId/changes", h.getChanges)
			streams.GET("/:branchId/ws", h.watchChanges)
		}

		// Internal operations (used by Python API)
		internal := v1.Group("/internal")
		{
			internal.POST("/branches/create", h.createBranch)
			internal.GET("/branches/:id/status", h.getBranchStatus)
		}
	}
}

// Health check endpoint
func (h *Handlers) healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "healthy",
		"service": "argon-engine",
		"version": "2.0.0",
	})
}

// Branch handlers

func (h *Handlers) createBranch(c *gin.Context) {
	var req branch.BranchCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	branch, err := h.branchService.CreateBranch(c.Request.Context(), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, branch)
}

func (h *Handlers) getBranch(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch ID"})
		return
	}

	branch, err := h.branchService.GetBranch(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, branch)
}

func (h *Handlers) updateBranch(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch ID"})
		return
	}

	var req branch.BranchUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	branch, err := h.branchService.UpdateBranch(c.Request.Context(), id, &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, branch)
}

func (h *Handlers) deleteBranch(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch ID"})
		return
	}

	err = h.branchService.DeleteBranch(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "branch deleted successfully"})
}

func (h *Handlers) getBranchStats(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch ID"})
		return
	}

	stats, err := h.branchService.GetBranchStats(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, stats)
}

// Project handlers

func (h *Handlers) listBranches(c *gin.Context) {
	projectID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project ID"})
		return
	}

	branches, err := h.branchService.ListBranches(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, branches)
}

func (h *Handlers) switchBranch(c *gin.Context) {
	projectID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project ID"})
		return
	}

	branchID, err := primitive.ObjectIDFromHex(c.Param("branchId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch ID"})
		return
	}

	err = h.branchService.SwitchBranch(c.Request.Context(), projectID, branchID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "branch switched successfully"})
}

// Stream handlers

func (h *Handlers) getChanges(c *gin.Context) {
	branchID, err := primitive.ObjectIDFromHex(c.Param("branchId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch ID"})
		return
	}

	// Parse query parameters
	limit := 50
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}

	changes, err := h.streamsService.GetChanges(c.Request.Context(), branchID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"changes": changes,
		"count":   len(changes),
	})
}

func (h *Handlers) watchChanges(c *gin.Context) {
	branchID, err := primitive.ObjectIDFromHex(c.Param("branchId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch ID"})
		return
	}

	// This would implement WebSocket connection for real-time change streaming
	// For now, just return a placeholder
	c.JSON(http.StatusOK, gin.H{
		"message":   "WebSocket endpoint for real-time changes",
		"branch_id": branchID.Hex(),
		"status":    "not_implemented",
	})
}

// Internal handlers

func (h *Handlers) getBranchStatus(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch ID"})
		return
	}

	branch, err := h.branchService.GetBranch(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":     branch.ID,
		"status": branch.Status,
		"name":   branch.Name,
	})
}