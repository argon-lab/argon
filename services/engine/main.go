package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"argon/engine/internal/api"
	"argon/engine/internal/branch"
	"argon/engine/internal/config"
	"argon/engine/internal/monitoring"
	"argon/engine/internal/storage"
	"argon/engine/internal/streams"
	"argon/engine/internal/workers"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Initialize metrics
	if err := monitoring.InitMetrics(); err != nil {
		log.Fatal("Failed to initialize metrics:", err)
	}

	// Start metrics server in goroutine
	go monitoring.StartMetricsServer("9090")

	// Setup MongoDB connection
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		log.Fatal("Failed to connect to MongoDB:", err)
	}
	defer client.Disconnect(context.Background())

	// Initialize services
	storageService, err := storage.NewService(cfg)
	if err != nil {
		log.Fatal("Failed to initialize storage service:", err)
	}
	
	// Initialize job queue
	jobQueue := workers.NewMongoQueue(client, "argon")
	if err := jobQueue.Initialize(context.Background()); err != nil {
		log.Fatal("Failed to initialize job queue:", err)
	}
	
	// Initialize services
	branchService := branch.NewService(client, storageService)
	
	// Initialize worker pool
	workerPool := workers.NewWorkerPool(jobQueue, branchService, storageService)
	
	// Configure workers (5 sync workers, 2 compression workers, etc.)
	workerPool.SetWorkerConfiguration(map[workers.JobType]int{
		workers.JobTypeSync:        5,
		workers.JobTypeCompression: 2,
		workers.JobTypeNotification: 1,
		workers.JobTypeCleanup:     1,
	})
	
	// Start worker pool
	if err := workerPool.Start(context.Background()); err != nil {
		log.Fatal("Failed to start worker pool:", err)
	}

	// Initialize health checker
	healthChecker := monitoring.NewHealthChecker(client, workerPool, "1.0.0")
	
	// Start health checks in background
	go healthChecker.StartHealthChecks(context.Background())
	
	// Initialize streams service with worker pool
	streamsService := streams.NewService(client, storageService, workerPool)
	
	// Start change streams processor
	go func() {
		if err := streamsService.Start(context.Background()); err != nil {
			log.Printf("Change streams processor error: %v", err)
		}
	}()

	// Setup HTTP server
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()
	
	// Add CORS middleware
	router.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// Add health check routes
	router.GET("/health", gin.WrapF(healthChecker.HealthHandler()))
	router.GET("/health/ready", gin.WrapF(healthChecker.ReadinessHandler()))
	router.GET("/health/live", gin.WrapF(healthChecker.LivenessHandler()))

	// Setup API routes
	api.SetupRoutes(router, branchService, streamsService, workerPool)

	// Setup HTTP server with graceful shutdown
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router,
	}

	// Start server in goroutine
	go func() {
		log.Printf("Argon Engine starting on port %s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	
	log.Println("Shutting down server...")

	// Shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop worker pool first
	if err := workerPool.Stop(ctx); err != nil {
		log.Printf("Error stopping worker pool: %v", err)
	}

	// Then stop HTTP server
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exited")
}