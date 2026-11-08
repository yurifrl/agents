package main

import (
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/yurifrl/agents/internal/config"
	"github.com/yurifrl/agents/internal/controller/agenttask"
	"github.com/yurifrl/agents/internal/kube"
	"github.com/yurifrl/agents/internal/logging"
)

func newControllerCmd() *cobra.Command {
	var cfgPath string

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the AgentTask controller manager",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			logger := logging.New(cfg.Log)

			mgr, err := kube.NewManager(cfg, logger)
			if err != nil {
				return err
			}

			reconciler := agenttask.NewReconciler(mgr.GetClient(), cfg)
			if err := reconciler.SetupWithManager(mgr); err != nil {
				return err
			}

			logger.Info("starting controller manager", "namespace", cfg.Namespace, "image", cfg.AgentImage)
			return mgr.Start(ctrl.SetupSignalHandler())
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", envOr("AGENTS_CONFIG", ""), "path to config file (optional; env AGENTS_* overrides)")

	return cmd
}
