// Package main serves Argon's REST API — the control plane for language
// SDKs (the Python agent adapters foremost): projects, branches, sandboxes,
// checkout/connection strings, diff/merge, undo and time travel. The data
// plane stays native MongoDB — clients write to branch databases through
// their own drivers; this server supervises a change-stream ingester for
// every branch it checks out, so those writes become versioned history.
//
// The router itself lives in the server package so the CLI can embed it
// (`argon console`); this binary is the standalone deployment.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/argon-lab/argon/api/server"
	"github.com/argon-lab/argon/pkg/walcli"
)

func main() {
	services, err := walcli.NewServices()
	if err != nil {
		log.Fatalf("failed to initialize services: %v", err)
	}

	router := server.NewRouter(services)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{Addr: ":" + port, Handler: router}
	go func() {
		log.Printf("Argon API listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	router.Shutdown() // stop supervised ingesters first
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
