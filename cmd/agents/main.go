// Command agents is the control plane for the remote-agents system.
//
// Subcommands:
//   - server:     standalone task server spawning omp workers (process/herdr surface)
//   - controller: Kubernetes AgentTask controller
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
	root.AddCommand(newServerCmd())
	root.AddCommand(newWorkerCmd())
	return root
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
