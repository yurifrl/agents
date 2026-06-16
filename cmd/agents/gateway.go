package main

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/yurifrl/agents/internal/config"
	"github.com/yurifrl/agents/internal/gateway"
	"github.com/yurifrl/agents/internal/kube"
	"github.com/yurifrl/agents/internal/logging"
)

func newGatewayCmd() *cobra.Command {
	var (
		cfgPath string
		addr    string
	)

	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Run the stateful A2A gateway (task store + dispatch + reconnection API)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			logger := logging.New(cfg.Log)

			scheme, err := kube.NewScheme()
			if err != nil {
				return err
			}
			restCfg, err := kube.RestConfig()
			if err != nil {
				return err
			}
			k8s, err := kube.NewClient(restCfg, scheme)
			if err != nil {
				return err
			}

			gw := gateway.New(gateway.NewMemoryStore(), k8s, cfg.Namespace)

			srv := &http.Server{Addr: addr, Handler: gw.Handler()}
			logger.Info("starting gateway", "addr", addr, "namespace", cfg.Namespace)

			go func() {
				<-cmd.Context().Done()
				_ = srv.Close()
			}()
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("gateway server: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", envOr("AGENTS_CONFIG", ""), "path to config file (optional; env AGENTS_* overrides)")
	cmd.Flags().StringVar(&addr, "addr", envOr("GATEWAY_ADDR", ":9090"), "gateway listen address")
	return cmd
}
