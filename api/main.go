package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/argon-lab/argon/api/handlers"
	"github.com/argon-lab/argon/pkg/walcli"
)

func main() {
	// Initialize services
	services, err := walcli.NewServices()
	if err != nil {
		log.Fatal("Failed to initialize services:", err)
	}

	// Create router
	r := gin.Default()

	// Configure CORS for dashboard
	config := cors.DefaultConfig()
	config.AllowOrigins = []string{"http://localhost:3000", "http://127.0.0.1:3000"}
	config.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	config.AllowHeaders = []string{"Origin", "Content-Type", "Accept", "Authorization"}
	r.Use(cors.New(config))

	// Initialize handlers
	h := handlers.NewHandlers(services)

	// API routes
	api := r.Group("/api")
	{
		// Health and system status
		api.GET("/health", h.Health)
		api.GET("/status", h.SystemStatus)
		api.GET("/version", h.Version)

		// Projects
		projects := api.Group("/projects")
		{
			projects.GET("", h.ListProjects)
			projects.POST("", h.CreateProject)
			projects.GET("/:id", h.GetProject)
			projects.PUT("/:id", h.UpdateProject)
			projects.DELETE("/:id", h.DeleteProject)

			// Branches
			projects.GET("/:id/branches", h.ListBranches)
			projects.POST("/:id/branches", h.CreateBranch)
			projects.GET("/:id/branches/:branchId", h.GetBranch)
			projects.PUT("/:id/branches/:branchId", h.UpdateBranch)
			projects.DELETE("/:id/branches/:branchId", h.DeleteBranch)

			// Time travel
			projects.GET("/:id/branches/:branchId/timetravel/info", h.GetTimeTravelInfo)
			projects.GET("/:id/branches/:branchId/timetravel/query", h.QueryTimeTravel)
		}

		// WAL monitoring
		wal := api.Group("/wal")
		{
			wal.GET("/metrics", h.GetWALMetrics)
			wal.GET("/monitor", h.GetWALMonitor)
			wal.GET("/health", h.GetWALHealth)
			wal.GET("/entries", h.GetWALEntries)
			wal.GET("/performance", h.GetWALPerformance)
			wal.GET("/alerts", h.GetWALAlerts)
		}

		// Import
		imports := api.Group("/import")
		{
			imports.POST("/preview", h.ImportPreview)
			imports.POST("/database", h.ImportDatabase)
			imports.GET("/status", h.ImportStatus)
		}
	}

	// Server configuration
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	// Start server in a goroutine
	go func() {
		log.Printf("Starting Argon API server on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	// Give server 30 seconds to shutdown gracefully
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exited")
}