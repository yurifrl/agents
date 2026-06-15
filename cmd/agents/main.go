// Command agents is the multi-tool binary for the remote-agents system.
// MVP surface: the `controller` subcommand runs the AgentTask controller.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
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
	return root
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
