package main

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/yurifrl/agents/internal/config"
	"github.com/yurifrl/agents/internal/logging"
	"github.com/yurifrl/agents/internal/workstrator"
)

func newWorkstratorCmd() *cobra.Command {
	var (
		cfgPath  string
		gateway  string
		owner    string
		repo     string
		agent    string
		interval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "workstrator",
		Short: "Poll the beads server for ready work and dispatch it to the gateway",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			logger := logging.New(cfg.Log)

			ws := workstrator.New(workstrator.Config{
				GatewayURL:   gateway,
				Owner:        owner,
				AgentType:    agent,
				DefaultRepo:  repo,
				PollInterval: interval,
			}, logger)
			return ws.Run(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", envOr("AGENTS_CONFIG", ""), "path to config file (optional; env AGENTS_* overrides)")
	cmd.Flags().StringVar(&gateway, "gateway", envOr("AGENTS_GATEWAY_URL", "http://agents-gateway.agents.svc:9090"), "gateway base URL")
	cmd.Flags().StringVar(&owner, "owner", envOr("AGENTS_OWNER", "workstrator"), "owner key for dispatched tasks")
	cmd.Flags().StringVar(&repo, "repo", envOr("WORKSTRATOR_REPO", ""), "default repo (owner/repo) for dispatched work")
	cmd.Flags().StringVar(&agent, "type", envOr("WORKSTRATOR_AGENT", "worker"), "agent profile to dispatch to")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "poll interval")
	return cmd
}
