// Package gwclient is a small Go client for the agents gateway HTTP API, shared
// by the workstrator and the MCP facade.
package gwclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Task mirrors the gateway's task record.
type Task struct {
	ID        string   `json:"id"`
	Owner     string   `json:"owner"`
	Type      string   `json:"type"`
	State     string   `json:"state"`
	Result    string   `json:"result,omitempty"`
	Error     string   `json:"error,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// CreateRequest is the body for creating a task.
type CreateRequest struct {
	Owner  string `json:"owner"`
	Type   string `json:"type"`
	Prompt string `json:"prompt"`
	Repo   string `json:"repo,omitempty"`
	Output string `json:"output,omitempty"`
}

// Client talks to a gateway base URL.
type Client struct {
	base string
	http *http.Client
}

// New builds a gateway client.
func New(base string) *Client {
	return &Client{base: base, http: &http.Client{}}
}

func (c *Client) Create(ctx context.Context, req CreateRequest) (*Task, error) {
	body, _ := json.Marshal(req)
	var task Task
	if err := c.do(ctx, http.MethodPost, "/tasks/create", bytes.NewReader(body), &task, http.StatusCreated); err != nil {
		return nil, err
	}
	return &task, nil
}

func (c *Client) Get(ctx context.Context, id string) (*Task, error) {
	var task Task
	if err := c.do(ctx, http.MethodGet, "/tasks/"+id, nil, &task, http.StatusOK); err != nil {
		return nil, err
	}
	return &task, nil
}

func (c *Client) List(ctx context.Context, owner string, onlyUnacked bool) ([]Task, error) {
	path := "/tasks?owner=" + owner
	if onlyUnacked {
		path += "&acknowledged=false"
	}
	var tasks []Task
	if err := c.do(ctx, http.MethodGet, path, nil, &tasks, http.StatusOK); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/tasks/"+id+"/cancel", nil, nil, http.StatusNoContent)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any, wantStatus int) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("gateway %s %s -> %d: %s", method, path, resp.StatusCode, string(raw))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}
