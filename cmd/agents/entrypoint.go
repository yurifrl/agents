package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yurifrl/agents/internal/agent"
	"github.com/yurifrl/agents/internal/output"
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

			out := envOr("AGENT_OUTPUT", "response")
			client := agent.NewClient(apiKey)
			ctx := context.Background()

			fmt.Printf("== agent %s (runtime=%s, model=%s, output=%s) ==\n", profile, runtime, model, out)
			fmt.Printf("== task ==\n%s\n\n", prompt)

			switch out {
			case "pr", "commit":
				return runPR(ctx, client, model, profile, out)
			default:
				return runResponse(ctx, client, model, profile, prompt)
			}
		},
	}

	cmd.Flags().StringVar(&profile, "profile", envOr("AGENT_PROFILE", "researcher"), "agent profile/definition name")
	cmd.Flags().StringVar(&runtime, "runtime", envOr("AGENT_RUNTIME", "adk"), "agent runtime (adk|pi)")
	cmd.Flags().StringVar(&model, "model", envOr("AGENT_MODEL", "claude-sonnet-4-5-20250929"), "model id")

	return cmd
}

func runResponse(ctx context.Context, client *agent.Client, model, profile, prompt string) error {
	system := fmt.Sprintf("You are the %q agent. Answer the user's task directly and concisely.", profile)
	fmt.Println("== response ==")
	resp, err := client.Run(ctx, agent.Request{Model: model, Prompt: prompt, System: system})
	if err != nil {
		return err
	}
	fmt.Println(resp)
	return nil
}

func runPR(ctx context.Context, client *agent.Client, model, profile, out string) error {
	repo := os.Getenv("AGENT_REPO")
	if repo == "" {
		return fmt.Errorf("AGENT_REPO is required for output=%s", out)
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return fmt.Errorf("GITHUB_TOKEN is required for output=%s (mount the agents secret)", out)
	}
	base := envOr("AGENT_BASE_BRANCH", "main")
	prompt := os.Getenv("AGENT_PROMPT")

	fmt.Println("== planning change ==")
	change, err := client.Plan(ctx, model, prompt, "")
	if err != nil {
		return err
	}
	fmt.Printf("summary: %s\nfiles: %d\n\n", change.Summary, len(change.Files))

	author := output.Author{
		Name:  envOr("GIT_AUTHOR_NAME", "agents-bot"),
		Email: envOr("GIT_EMAIL", "agents-bot@users.noreply.github.com"),
	}

	fmt.Println("== opening pull request ==")
	url, err := output.PR(ctx, repo, token, base, change, author, out == "pr")
	if err != nil {
		return err
	}
	if out == "pr" {
		fmt.Printf("PR: %s\n", url)
	} else {
		fmt.Printf("pushed branch: %s\n", url)
	}
	return nil
}
