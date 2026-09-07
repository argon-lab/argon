package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

// Cobra executes one command at a time. Retain every connection opened by the
// command so errors, as well as successful returns, run the same cleanup.
var commandServices []*walcli.Services

func newCommandServices(cmd *cobra.Command) (*walcli.Services, error) {
	services, err := walcli.NewServices()
	if err != nil {
		return nil, err
	}
	switch cmd.Name() {
	case "console", "mcp", "watch", "proxy":
	default:
		services.SynchronousSnapshots()
	}
	commandServices = append(commandServices, services)
	return services, nil
}

func closeCommandServices() error {
	var failures []error
	for _, services := range commandServices {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		// Versioned operations already drained their required boundary. Cancel
		// further capture and wait for its current transaction before disconnecting;
		// an abrupt process exit can otherwise leave server-side transaction locks.
		for _, status := range services.Ingest.Statuses() {
			if err := services.Ingest.Cancel(ctx, status.BranchID); err != nil {
				failures = append(failures, err)
			}
		}
		cancel()
		// Use a separate deadline: draining capture must not consume the time
		// available to finish and release automatic snapshot storage locks.
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		if err := services.WaitAuto(ctx); err != nil {
			failures = append(failures, err)
			// The first timeout cancels storage work. Give cooperative deferred
			// lock release a bounded grace period before closing its connection.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if cleanupErr := services.WaitAuto(cleanupCtx); cleanupErr != nil {
				failures = append(failures, cleanupErr)
			}
			cleanupCancel()
		}
		cancel()
		services.Monitor.Stop()
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		if err := services.Client.Disconnect(ctx); err != nil {
			failures = append(failures, err)
		}
		cancel()
	}
	commandServices = nil
	return errors.Join(failures...)
}
