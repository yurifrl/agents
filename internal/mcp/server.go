// Package mcp exposes the system over the Model Context Protocol so MCP clients
// (e.g. hermes) can drive agents and beads with no knowledge of the internals.
// It is just another gateway client (design D10): agents_* wrap the gateway,
// beads_* wrap the bd CLI.
package mcp

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/yurifrl/agents/internal/gwclient"
)

// Server builds the MCP server exposing agents_* and beads_* tools.
type Server struct {
	gw    *gwclient.Client
	owner string
	mcp   *server.MCPServer
}

// New builds the MCP facade over the given gateway URL.
func New(gatewayURL, owner string) *Server {
	s := &Server{
		gw:    gwclient.New(gatewayURL),
		owner: owner,
		mcp:   server.NewMCPServer("agents", "v1alpha1"),
	}
	s.registerAgents()
	s.registerBeads()
	return s
}

// ServeStdio runs the MCP server over stdio (the transport `hermes mcp add` uses).
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcp)
}

func (s *Server) registerAgents() {
	s.mcp.AddTool(mcp.NewTool("agents_create",
		mcp.WithDescription("Dispatch a unit of work to a remote agent; returns the task id."),
		mcp.WithString("type", mcp.Required(), mcp.Description("agent profile, e.g. researcher or worker")),
		mcp.WithString("prompt", mcp.Required(), mcp.Description("the task instruction")),
		mcp.WithString("repo", mcp.Description("owner/repo for pr/commit output")),
		mcp.WithString("output", mcp.Description("response | commit | pr")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		task, err := s.gw.Create(ctx, gwclient.CreateRequest{
			Owner:  s.owner,
			Type:   req.GetString("type", ""),
			Prompt: req.GetString("prompt", ""),
			Repo:   req.GetString("repo", ""),
			Output: req.GetString("output", ""),
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("task %s created (state=%s)", task.ID, task.State)), nil
	})

	s.mcp.AddTool(mcp.NewTool("agents_list",
		mcp.WithDescription("List this owner's agent tasks."),
		mcp.WithString("unacknowledged", mcp.Description("true to list only unseen results")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tasks, err := s.gw.List(ctx, s.owner, req.GetString("unacknowledged", "") == "true")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var b strings.Builder
		for _, t := range tasks {
			fmt.Fprintf(&b, "%s [%s] %s %s\n", t.ID, t.State, t.Type, t.Result)
		}
		if b.Len() == 0 {
			b.WriteString("no tasks")
		}
		return mcp.NewToolResultText(b.String()), nil
	})

	s.mcp.AddTool(mcp.NewTool("agents_get",
		mcp.WithDescription("Get one agent task by id."),
		mcp.WithString("id", mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		task, err := s.gw.Get(ctx, req.GetString("id", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultJSON(task)
	})

	s.mcp.AddTool(mcp.NewTool("agents_cancel",
		mcp.WithDescription("Cancel a running agent task."),
		mcp.WithString("id", mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := s.gw.Cancel(ctx, req.GetString("id", "")); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText("canceled"), nil
	})
}

func (s *Server) registerBeads() {
	bd := func(ctx context.Context, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "bd", args...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}

	s.mcp.AddTool(mcp.NewTool("beads_ready",
		mcp.WithDescription("List ready beads (issues with no open blockers)."),
	), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := bd(ctx, "ready", "--json")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(out), nil
	})

	s.mcp.AddTool(mcp.NewTool("beads_show",
		mcp.WithDescription("Show one bead by id."),
		mcp.WithString("id", mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := bd(ctx, "show", req.GetString("id", ""), "--json")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(out), nil
	})

	s.mcp.AddTool(mcp.NewTool("beads_create",
		mcp.WithDescription("Create a bead (issue)."),
		mcp.WithString("title", mcp.Required()),
		mcp.WithString("description", mcp.Description("why this exists and what to do")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := []string{"create", req.GetString("title", "")}
		if d := req.GetString("description", ""); d != "" {
			args = append(args, "--description="+d)
		}
		out, err := bd(ctx, args...)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(out), nil
	})

	s.mcp.AddTool(mcp.NewTool("beads_close",
		mcp.WithDescription("Close a bead with a reason."),
		mcp.WithString("id", mcp.Required()),
		mcp.WithString("reason", mcp.Description("why it's closed")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := bd(ctx, "close", req.GetString("id", ""), "--reason="+req.GetString("reason", "done"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(out), nil
	})
}
