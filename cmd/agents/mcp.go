package main

import (
	"github.com/spf13/cobra"

	"github.com/yurifrl/agents/internal/mcp"
)

func newMCPCmd() *cobra.Command {
	var (
		gateway string
		owner   string
	)

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the MCP facade (agents_* + beads_*) over stdio",
		RunE: func(_ *cobra.Command, _ []string) error {
			return mcp.New(gateway, owner).ServeStdio()
		},
	}

	cmd.Flags().StringVar(&gateway, "gateway", envOr("AGENTS_GATEWAY_URL", "http://agents-gateway.agents.svc:9090"), "gateway base URL")
	cmd.Flags().StringVar(&owner, "owner", envOr("AGENTS_OWNER", "mcp"), "owner key for created tasks")
	return cmd
}
