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
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/argon-lab/argon/api/server"
	"github.com/argon-lab/argon/pkg/walcli"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	host := os.Getenv("ARGON_API_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, port)
	opts := server.OptionsFromEnv()
	if err := server.ValidateListenAddress(addr, opts); err != nil {
		log.Fatal(err)
	}
	services, err := walcli.NewServices()
	if err != nil {
		log.Fatalf("failed to initialize services: %v", err)
	}

	router := server.NewRouterWith(services, opts)
	srv := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("Argon API listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("api: HTTP shutdown: %v", err)
		_ = srv.Close()
	}
	router.Shutdown() // producers have stopped; drain capture and snapshots last
}
