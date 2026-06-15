// Package agent implements the in-pod agent runtime. The MVP runtime performs a
// single Claude completion against the task prompt and returns the text.
// Tool-use loops, the pi runtime, and pr/commit output wrappers come later.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	defaultMaxTokens = 4096
)

// Request is the input to a single agent run.
type Request struct {
	Model     string
	Prompt    string
	System    string
	MaxTokens int
}

// Client talks to the Anthropic Messages API.
type Client struct {
	apiKey string
	http   *http.Client
}

// NewClient builds an Anthropic client with the given API key.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 5 * time.Minute},
	}
}

type messagesRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []messagePayload `json:"messages"`
}

type messagePayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Run executes a single completion and returns the concatenated text content.
func (c *Client) Run(ctx context.Context, req Request) (string, error) {
	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens
	}

	body, err := json.Marshal(messagesRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    req.System,
		Messages:  []messagePayload{{Role: "user", Content: req.Prompt}},
	})
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling anthropic: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	var parsed messagesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decoding response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic error: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic returned status %d: %s", resp.StatusCode, string(raw))
	}

	var out bytes.Buffer
	for _, block := range parsed.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String(), nil
}
