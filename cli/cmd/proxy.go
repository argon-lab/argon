package cmd

import (
	"context"
	"fmt"
	"net"
	"os/signal"
	"strings"
	"syscall"

	"github.com/argon-lab/argon/pkg/walcli"
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
versioned history.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")

		services, err := walcli.NewServices()
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
		fmt.Printf("Argon proxy listening on %s (upstream %s)\n", listen, upstream)
		fmt.Printf("Connect with: mongodb://%s/<project>~<branch>?directConnection=true\n", hint)
		return proxy.Serve(ctx, listener)
	},
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
	proxyCmd.Flags().String("listen", ":27018", "Address to listen on")
	rootCmd.AddCommand(proxyCmd)
}
