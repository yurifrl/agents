// Package runner orchestrates a single agent invocation: it runs the model and
// applies the output contract (response | commit | pr), emitting A2A events as
// it goes. Both the one-shot entrypoint and the in-pod A2A server use it.
package runner

import (
	"context"
	"fmt"

	"github.com/yurifrl/agents/internal/a2a"
	"github.com/yurifrl/agents/internal/agent"
	"github.com/yurifrl/agents/internal/agentmd"
	"github.com/yurifrl/agents/internal/output"
)

// Runner executes invocations for one agent profile.
type Runner struct {
	client *agent.Client
	def    *agentmd.Definition
	token  string
	author output.Author
}

// New builds a Runner from a parsed Agent.md definition.
func New(client *agent.Client, def *agentmd.Definition, token string, author output.Author) *Runner {
	return &Runner{client: client, def: def, token: token, author: author}
}

// Run executes the invocation, emitting streaming events, and returns the
// terminal result. It matches the a2a.Runner signature. The InvokeRequest may
// override the definition's model and output.
func (r *Runner) Run(ctx context.Context, req a2a.InvokeRequest, emit func(a2a.Event)) (*a2a.Result, error) {
	model := req.Model
	if model == "" {
		model = r.def.Model
	}
	out := req.Output
	if out == "" {
		out = r.def.Output
	}

	switch out {
	case "pr", "commit":
		return r.runPR(ctx, model, out, req, emit)
	default:
		return r.runResponse(ctx, model, req, emit)
	}
}

func (r *Runner) runResponse(ctx context.Context, model string, req a2a.InvokeRequest, emit func(a2a.Event)) (*a2a.Result, error) {
	text, err := r.client.Run(ctx, agent.Request{Model: model, Prompt: req.Prompt, System: r.def.Instruction})
	if err != nil {
		return nil, err
	}
	emit(a2a.Event{Type: a2a.EventArtifact, Artifact: &a2a.Artifact{Name: "response", Text: text}})
	return &a2a.Result{Output: text}, nil
}

func (r *Runner) runPR(ctx context.Context, model, out string, req a2a.InvokeRequest, emit func(a2a.Event)) (*a2a.Result, error) {
	if req.Repo == "" {
		return nil, fmt.Errorf("repo is required for output=%s", out)
	}
	if r.token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is required for output=%s", out)
	}

	emit(a2a.Event{Type: a2a.EventArtifact, Artifact: &a2a.Artifact{Name: "progress", Text: "planning change"}})
	taskPrompt := r.def.Instruction + "\n\nTask:\n" + req.Prompt
	change, err := r.client.Plan(ctx, model, taskPrompt, "")
	if err != nil {
		return nil, err
	}
	emit(a2a.Event{Type: a2a.EventArtifact, Artifact: &a2a.Artifact{Name: "plan", Text: change.Summary}})

	emit(a2a.Event{Type: a2a.EventArtifact, Artifact: &a2a.Artifact{Name: "progress", Text: "pushing and opening PR"}})
	url, err := output.PR(ctx, req.Repo, r.token, "main", change, r.author, out == "pr")
	if err != nil {
		return nil, err
	}
	emit(a2a.Event{Type: a2a.EventArtifact, Artifact: &a2a.Artifact{Name: "pr", Text: url}})
	return &a2a.Result{Output: url, Artifacts: []string{url}}, nil
}
