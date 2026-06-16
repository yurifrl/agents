package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yurifrl/agents/internal/a2a"
	"github.com/yurifrl/agents/internal/agent"
	"github.com/yurifrl/agents/internal/output"
	"github.com/yurifrl/agents/internal/runner"
)

func newEntrypointCmd() *cobra.Command {
	var (
		profile string
		model   string
		serve   bool
		addr    string
	)

	cmd := &cobra.Command{
		Use:   "agent-entrypoint",
		Short: "Run an agent task (one-shot) or serve it over A2A (--serve)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiKey := os.Getenv("ANTHROPIC_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("ANTHROPIC_API_KEY is not set (mount the agents secret)")
			}

			run := runner.New(
				agent.NewClient(apiKey),
				model,
				profile,
				os.Getenv("GITHUB_TOKEN"),
				output.Author{
					Name:  envOr("GIT_AUTHOR_NAME", "agents-bot"),
					Email: envOr("GIT_EMAIL", "agents-bot@users.noreply.github.com"),
				},
			)

			if serve {
				return serveAgent(cmd.Context(), run, profile, addr)
			}
			return oneShot(cmd.Context(), run)
		},
	}

	cmd.Flags().StringVar(&profile, "profile", envOr("AGENT_PROFILE", "researcher"), "agent profile/definition name")
	cmd.Flags().StringVar(&model, "model", envOr("AGENT_MODEL", "claude-sonnet-4-5-20250929"), "model id")
	cmd.Flags().BoolVar(&serve, "serve", false, "run as an A2A server instead of one-shot")
	cmd.Flags().StringVar(&addr, "addr", envOr("AGENT_ADDR", ":8080"), "A2A listen address (with --serve)")

	return cmd
}

// oneShot runs a single invocation from env and prints the stream to stdout.
func oneShot(ctx context.Context, run *runner.Runner) error {
	prompt := os.Getenv("AGENT_PROMPT")
	if prompt == "" {
		return fmt.Errorf("AGENT_PROMPT is empty; nothing to do")
	}
	req := a2a.InvokeRequest{
		Prompt: prompt,
		Repo:   os.Getenv("AGENT_REPO"),
		Output: envOr("AGENT_OUTPUT", "response"),
	}

	emit := func(ev a2a.Event) {
		switch ev.Type {
		case a2a.EventArtifact:
			fmt.Printf("[%s] %s\n", ev.Artifact.Name, ev.Artifact.Text)
		case a2a.EventStatus:
			fmt.Printf("[status] %s\n", ev.State)
		}
	}

	result, err := run.Run(ctx, req, emit)
	if err != nil {
		return err
	}
	if result.Output != "" {
		fmt.Printf("\n== result ==\n%s\n", result.Output)
	}
	return nil
}

// serveAgent exposes the agent over A2A until the context is canceled.
func serveAgent(ctx context.Context, run *runner.Runner, profile, addr string) error {
	card := a2a.AgentCard{
		Name:         profile,
		Description:  fmt.Sprintf("Remote agent %q", profile),
		URL:          "http://localhost" + addr,
		Version:      "v1alpha1",
		Capabilities: a2a.Capabilities{Streaming: true},
	}
	srv := a2a.NewServer(card, run.Run)
	fmt.Printf("serving agent %q on %s\n", profile, addr)
	return srv.ListenAndServe(ctx, addr)
}
