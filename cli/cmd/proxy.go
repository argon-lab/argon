package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Serve branch-aliased MongoDB connection strings",
	Long: `A MongoDB wire-protocol proxy that resolves branch aliases: clients
connect to

  mongodb://<proxy-host>:<port>/<project>~<branch>?directConnection=true

and the proxy rewrites commands to the branch's physical database.
Traffic to non-alias databases passes through untouched. Run "argon
watch" (or the API/MCP server) for the branch so writes become
versioned history.

The proxy listens on loopback by default. It adds no authentication of
its own: whoever can reach it can reach the upstream deployment with
whatever access that deployment grants an anonymous client. Listening
on another interface therefore needs --allow-remote, and should only be
done when the upstream MongoDB itself requires authentication.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		allowRemote, _ := cmd.Flags().GetBool("allow-remote")
		if err := validateProxyListen(listen, allowRemote); err != nil {
			return err
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}

		upstream := upstreamAddr(services.MongoURI)
		proxy := services.NewWireProxy(upstream)

		listener, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %w", listen, err)
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		hint := listen
		if strings.HasPrefix(hint, ":") {
			hint = "<host>" + hint
		}
		if !isLoopbackListen(listen) {
			fmt.Fprintf(os.Stderr, "Warning: the proxy is reachable from other hosts and forwards to %s without authenticating clients itself; make sure that deployment requires authentication.\n", upstream)
		}
		fmt.Printf("Argon proxy listening on %s (upstream %s)\n", listen, upstream)
		fmt.Printf("Connect with: mongodb://%s/<project>~<branch>?directConnection=true\n", hint)
		return proxy.Serve(ctx, listener)
	},
}

// isLoopbackListen reports whether a listen address accepts connections
// from this machine only. An empty host (":27018") binds every interface.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateProxyListen keeps the proxy from exposing the upstream
// deployment by accident: the proxy authenticates nobody, so a
// non-loopback listener must be asked for explicitly.
func validateProxyListen(addr string, allowRemote bool) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("invalid --listen address %q: %w", addr, err)
	}
	if isLoopbackListen(addr) || allowRemote {
		return nil
	}
	return fmt.Errorf("--listen %s accepts connections from other hosts, and the proxy forwards them to MongoDB without authenticating clients itself; "+
		"use 127.0.0.1 for local access, or pass --allow-remote once the upstream deployment requires authentication", addr)
}

// upstreamAddr extracts host:port from a mongodb URI.
func upstreamAddr(uri string) string {
	rest := uri
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if rest == "" {
		return "localhost:27017"
	}
	if !strings.Contains(rest, ":") {
		rest += ":27017"
	}
	return rest
}

func init() {
	proxyCmd.Flags().String("listen", "127.0.0.1:27018", "Address to listen on (loopback by default)")
	proxyCmd.Flags().Bool("allow-remote", false, "Permit a non-loopback --listen address; the upstream MongoDB must require authentication")
	rootCmd.AddCommand(proxyCmd)
}
