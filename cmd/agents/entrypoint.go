package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yurifrl/agents/internal/agent"
)

func newEntrypointCmd() *cobra.Command {
	var (
		profile string
		runtime string
		model   string
	)

	cmd := &cobra.Command{
		Use:   "agent-entrypoint",
		Short: "Run a single agent task inside a pod",
		RunE: func(cmd *cobra.Command, _ []string) error {
			prompt := os.Getenv("AGENT_PROMPT")
			if prompt == "" {
				return fmt.Errorf("AGENT_PROMPT is empty; nothing to do")
			}
			apiKey := os.Getenv("ANTHROPIC_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("ANTHROPIC_API_KEY is not set (mount the agents secret)")
			}

			system := fmt.Sprintf("You are the %q agent. Answer the user's task directly and concisely.", profile)

			fmt.Printf("== agent %s (runtime=%s, model=%s) ==\n", profile, runtime, model)
			fmt.Printf("== task ==\n%s\n\n== response ==\n", prompt)

			client := agent.NewClient(apiKey)
			out, err := client.Run(context.Background(), agent.Request{
				Model:  model,
				Prompt: prompt,
				System: system,
			})
			if err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}

	cmd.Flags().StringVar(&profile, "profile", envOr("AGENT_PROFILE", "researcher"), "agent profile/definition name")
	cmd.Flags().StringVar(&runtime, "runtime", envOr("AGENT_RUNTIME", "adk"), "agent runtime (adk|pi)")
	cmd.Flags().StringVar(&model, "model", envOr("AGENT_MODEL", "claude-sonnet-4-5-20250929"), "model id")

	return cmd
}
