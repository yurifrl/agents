package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yurifrl/agents/internal/agentmd"
)

const maxHTTPBody = 1 << 20

var builtins = map[string]func(agentmd.ToolGrant) (Tool, error){
	"exec":   func(agentmd.ToolGrant) (Tool, error) { return NewExec(), nil },
	"http":   func(g agentmd.ToolGrant) (Tool, error) { return NewHTTP(g.AllowHosts), nil },
	"notify": func(g agentmd.ToolGrant) (Tool, error) { return NewNotify(g.URLFromEnv) },
	"sleep":  func(agentmd.ToolGrant) (Tool, error) { return &sleepTool{}, nil },
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

type execTool struct{}

func NewExec() Tool { return &execTool{} }

func (e *execTool) Name() string { return "exec" }

func (e *execTool) Schema() json.RawMessage {
	return json.RawMessage(`{"name":"exec","description":"Run a command as argv with no shell interpretation","input_schema":{"type":"object","properties":{"argv":{"type":"array","items":{"type":"string"}}},"required":["argv"]}}`)
}

func (e *execTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("exec: invalid arguments: %w", err)
	}
	if len(a.Argv) == 0 {
		return Result{Text: "exec: argv must not be empty", IsError: true}, nil
	}
	cmd := exec.CommandContext(ctx, a.Argv[0], a.Argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Text: fmt.Sprintf("%s\n%v", out, err), IsError: true}, nil
	}
	return Result{Text: string(out)}, nil
}

type httpTool struct{ client *http.Client }

func NewHTTP(allowHosts []string) Tool {
	allowed := map[string]bool{}
	for _, h := range allowHosts {
		allowed[h] = true
	}
	return &httpTool{client: &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !allowed[req.URL.Host] {
				return fmt.Errorf("http: redirect to host %q denied by allow list", req.URL.Host)
			}
			return nil
		},
	}}
}

func (h *httpTool) Name() string { return "http" }

func (h *httpTool) Schema() json.RawMessage {
	return json.RawMessage(`{"name":"http","description":"Make an HTTP request to an allow-listed host","input_schema":{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string"},"body":{"type":"string"}},"required":["url"]}}`)
}

func (h *httpTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		URL    string `json:"url"`
		Method string `json:"method"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("http: invalid arguments: %w", err)
	}
	method := a.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL, strings.NewReader(a.Body))
	if err != nil {
		return Result{}, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return Result{}, err
	}
	text := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, body)
	return Result{Text: text, IsError: resp.StatusCode >= 400}, nil
}

type notifyTool struct {
	url    string
	client *http.Client
}

func NewNotify(urlFromEnv string) (Tool, error) {
	if urlFromEnv == "" {
		return nil, fmt.Errorf("notify requires urlFromEnv")
	}
	u := os.Getenv(urlFromEnv)
	if u == "" {
		return nil, fmt.Errorf("env var %q is not set", urlFromEnv)
	}
	return &notifyTool{url: u, client: &http.Client{}}, nil
}

func (n *notifyTool) Name() string { return "notify" }

func (n *notifyTool) Schema() json.RawMessage {
	return json.RawMessage(`{"name":"notify","description":"Send a text notification to the configured webhook","input_schema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}`)
}

func (n *notifyTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("notify: invalid arguments: %w", err)
	}
	payload, _ := json.Marshal(map[string]string{"text": a.Text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("notify: webhook request failed: %s", strings.ReplaceAll(err.Error(), n.url, "***"))
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return Result{Text: fmt.Sprintf("webhook returned HTTP %d", resp.StatusCode), IsError: true}, nil
	}
	return Result{Text: "notification sent"}, nil
}

type sleepTool struct{}

func (s *sleepTool) Name() string { return "sleep" }

func (s *sleepTool) Schema() json.RawMessage {
	return json.RawMessage(`{"name":"sleep","description":"Sleep for a duration","input_schema":{"type":"object","properties":{"duration":{"type":"string"}},"required":["duration"]}}`)
}

func (s *sleepTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Duration string `json:"duration"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("sleep: invalid arguments: %w", err)
	}
	d, err := time.ParseDuration(a.Duration)
	if err != nil {
		return Result{Text: fmt.Sprintf("sleep: invalid duration: %v", err), IsError: true}, nil
	}
	select {
	case <-time.After(d):
		return Result{Text: fmt.Sprintf("slept %s", d)}, nil
	case <-ctx.Done():
		return Result{Text: "sleep interrupted: " + ctx.Err().Error(), IsError: true}, nil
	}
}
