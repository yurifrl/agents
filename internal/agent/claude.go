// Package agent implements the in-pod agent runtime. It runs a Claude
// completion loop with optional native tool use and returns the text.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yurifrl/agents/internal/tool"
)

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	defaultMaxTokens = 4096
	defaultMaxTurns  = 10
)

// ToolExecutor provides tool schemas and executes tool calls. *tool.Registry
// implements it.
type ToolExecutor interface {
	Schemas() []json.RawMessage
	Execute(ctx context.Context, c tool.Call) tool.Result
}

// Request is the input to a single agent run.
type Request struct {
	Model     string
	Prompt    string
	System    string
	MaxTokens int
}

// LoopRequest is the input to a tool-use loop run.
type LoopRequest struct {
	Model, System, Prompt string
	MaxTokens, MaxTurns   int
	Tools                 ToolExecutor
	OnToolEvent           func(name string, isResult bool, text string)
}

// Client talks to the Anthropic Messages API.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithBaseURL overrides the Anthropic Messages endpoint (for tests and proxies).
func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = url }
}

// NewClient builds an Anthropic client with the given API key.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:  apiKey,
		baseURL: anthropicURL,
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type messagesRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	System    string            `json:"system,omitempty"`
	Messages  []messagePayload  `json:"messages"`
	Tools     []json.RawMessage `json:"tools,omitempty"`
}

type messagePayload struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// contentBlock is one Anthropic Messages content block. The fields cover text,
// tool_use (id/name/input), and tool_result (tool_use_id/content/is_error).
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type messagesResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Run executes a single completion and returns the concatenated text content.
func (c *Client) Run(ctx context.Context, req Request) (string, error) {
	return c.RunLoop(ctx, LoopRequest{
		Model:     req.Model,
		System:    req.System,
		Prompt:    req.Prompt,
		MaxTokens: req.MaxTokens,
	})
}

// RunLoop runs completions until the model stops requesting tools or MaxTurns
// is hit, returning the concatenated text content of all turns.
func (c *Client) RunLoop(ctx context.Context, req LoopRequest) (string, error) {
	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens
	}
	if req.MaxTurns == 0 {
		req.MaxTurns = defaultMaxTurns
	}

	var tools []json.RawMessage
	if req.Tools != nil {
		tools = req.Tools.Schemas()
	}

	msgs := []messagePayload{{
		Role:    "user",
		Content: []contentBlock{{Type: "text", Text: req.Prompt}},
	}}

	var out strings.Builder
	for turn := 0; turn < req.MaxTurns; turn++ {
		resp, err := c.call(ctx, messagesRequest{
			Model:     req.Model,
			MaxTokens: req.MaxTokens,
			System:    req.System,
			Messages:  msgs,
			Tools:     tools,
		})
		if err != nil {
			return "", err
		}

		var toolUses []contentBlock
		for _, b := range resp.Content {
			switch b.Type {
			case "text":
				out.WriteString(b.Text)
			case "tool_use":
				toolUses = append(toolUses, b)
			}
		}

		if resp.StopReason != "tool_use" || len(toolUses) == 0 {
			return out.String(), nil
		}
		if req.Tools == nil {
			return "", fmt.Errorf("model requested tool use but no tools are configured")
		}

		msgs = append(msgs, messagePayload{Role: "assistant", Content: resp.Content})
		results := make([]contentBlock, 0, len(toolUses))
		for _, tu := range toolUses {
			if req.OnToolEvent != nil {
				req.OnToolEvent(tu.Name, false, string(tu.Input))
			}
			res := req.Tools.Execute(ctx, tool.Call{Name: tu.Name, Arguments: tu.Input})
			if req.OnToolEvent != nil {
				req.OnToolEvent(tu.Name, true, res.Text)
			}
			results = append(results, contentBlock{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Content:   res.Text,
				IsError:   res.IsError,
			})
		}
		msgs = append(msgs, messagePayload{Role: "user", Content: results})
	}
	return "", fmt.Errorf("agent loop exceeded max turns (%d)", req.MaxTurns)
}

func (c *Client) call(ctx context.Context, payload messagesRequest) (*messagesResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling anthropic: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var parsed messagesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decoding response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("anthropic error: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic returned status %d: %s", resp.StatusCode, string(raw))
	}
	return &parsed, nil
}
