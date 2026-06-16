// Command agents is the multi-tool binary for the remote-agents system.
// MVP surface: the `controller` subcommand runs the AgentTask controller.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agents",
		Short:         "Remote agents control plane",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newControllerCmd())
	root.AddCommand(newEntrypointCmd())
	root.AddCommand(newGatewayCmd())
	root.AddCommand(newWorkstratorCmd())
	return root
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
