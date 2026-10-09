package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/yurifrl/agents/internal/server"
)

func newServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Run the remote-agents server (spawns omp workers in process/herdr surfaces)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := server.LoadConfig()
			if err != nil {
				return err
			}
			app, err := server.CreateApp(config, server.AppOptions{})
			if err != nil {
				return err
			}
			kind := app.Supervisor.Kind()
			adopted := app.Supervisor.AdoptExisting()

			fmt.Printf("remote-agents server listening on %d (surface=%s, adopted=%d)\n", app.Port(), kind, adopted)

			errc := make(chan error, 1)
			go func() { errc <- app.Serve() }()

			select {
			case err := <-errc:
				if err != nil && err != http.ErrServerClosed {
					return err
				}
				return nil
			case <-cmd.Context().Done():
				// Signal: graceful shutdown, supervisor first (mirrors the TS main).
				app.Supervisor.Shutdown()
				_ = app.Store.SaveNow()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = app.Shutdown(shutdownCtx)
				fmt.Println("remote-agents: signal received — shut down")
				return nil
			}
		},
	}
}
