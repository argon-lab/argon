package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/argon-lab/argon/api/v2/server"
	"github.com/spf13/cobra"
)

var (
	consolePort      int
	consoleHost      string
	consoleNoBrowser bool
	consoleBasePath  string
)

var consoleCmd = &cobra.Command{
	Use:   "console",
	Short: "Serve the web console (REST API + UI) and open it in a browser",
	Long: `Serve the Argon web console against your local engine.

One process serves both the REST control plane and the console UI,
bound to localhost by default. It supervises native write capture and
sweeps expired sandboxes every minute while running. Everything the console shows is your
own data: projects, branches, history, merge plans, pins, sandboxes.

Set ARGON_API_TOKEN to require a bearer token on the API, or
ARGON_READ_ONLY=1 to serve a look-but-don't-touch instance. Without a
token the console answers only to localhost and loopback addresses; set
ARGON_ALLOWED_HOSTS (comma-separated hostnames) when a tunnel or reverse
proxy presents it under another name.

Behind a reverse proxy that mounts the console under a path, e.g.
https://cloud.example.com/p/acme/, pass --base-path /p/acme (or set
ARGON_CONSOLE_BASE_PATH). The page then loads its assets, routes and API
calls under that path. The server answers with or without the prefix, so
the proxy may strip it or forward it unchanged.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		addr := fmt.Sprintf("%s:%d", consoleHost, consolePort)
		opts := server.OptionsFromEnv()
		if cmd.Flags().Changed("base-path") {
			opts.BasePath = consoleBasePath
		}
		if err := server.ValidateListenAddress(addr, opts); err != nil {
			return err
		}
		base, _ := server.NormalizeBasePath(opts.BasePath)
		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect to MongoDB: %w", err)
		}
		router := server.NewRouterWith(services, opts)

		url := "http://" + addr + base + "/"
		srv := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 10 * time.Second}
		errc := make(chan error, 1)
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errc <- err
			}
		}()

		fmt.Printf("Argon console at %s (Ctrl-C to stop)\n", url)
		if !consoleNoBrowser {
			go func() {
				time.Sleep(300 * time.Millisecond)
				_ = openBrowser(url)
			}()
		}

		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(quit)
		select {
		case err := <-errc:
			router.Shutdown()
			return err
		case <-quit:
		}

		fmt.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Stop accepting requests and wait for handlers before capture/snapshot
		// shutdown, so a late request cannot start more background work.
		shutdownErr := srv.Shutdown(ctx)
		if shutdownErr != nil {
			_ = srv.Close()
		}
		router.Shutdown()
		return shutdownErr
	},
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func init() {
	// 18: argon's atomic number, twice.
	consoleCmd.Flags().IntVar(&consolePort, "port", 1818, "port to listen on")
	consoleCmd.Flags().StringVar(&consoleHost, "host", "127.0.0.1", "address to bind")
	consoleCmd.Flags().BoolVar(&consoleNoBrowser, "no-browser", false, "do not open the browser")
	consoleCmd.Flags().StringVar(&consoleBasePath, "base-path", "", "path prefix a reverse proxy serves the console under, e.g. /p/acme (default: ARGON_CONSOLE_BASE_PATH)")
	rootCmd.AddCommand(consoleCmd)
}
