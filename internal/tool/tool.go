package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yurifrl/agents/internal/agentmd"
)

type Call struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Result struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
}

type Tool interface {
	Name() string
	Schema() json.RawMessage
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

type entry struct {
	tool     Tool
	grant    agentmd.ToolGrant
	timeout  time.Duration
	cooldown time.Duration
	calls    int
	lastCall time.Time
}

type Registry struct {
	mu      sync.Mutex
	tools   map[string]*entry
	secrets []string
}

func NewRegistry(grants []agentmd.ToolGrant, secrets []string) (*Registry, error) {
	r := &Registry{tools: map[string]*entry{}, secrets: secrets}
	for _, g := range grants {
		f, ok := builtins[g.Name]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q: no built-in implementation", g.Name)
		}
		e := &entry{grant: g}
		var err error
		if g.Timeout != "" {
			if e.timeout, err = time.ParseDuration(g.Timeout); err != nil {
				return nil, fmt.Errorf("tool %q: invalid timeout: %w", g.Name, err)
			}
		}
		if g.Cooldown != "" {
			if e.cooldown, err = time.ParseDuration(g.Cooldown); err != nil {
				return nil, fmt.Errorf("tool %q: invalid cooldown: %w", g.Name, err)
			}
		}
		tool, err := f(g)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", g.Name, err)
		}
		e.tool = tool
		r.tools[g.Name] = e
	}
	return r, nil
}

func (r *Registry) Schemas() []json.RawMessage {
	out := make([]json.RawMessage, 0, len(r.tools))
	for _, e := range r.tools {
		out = append(out, e.tool.Schema())
	}
	return out
}

func (r *Registry) Execute(ctx context.Context, c Call) Result {
	r.mu.Lock()
	e, ok := r.tools[c.Name]
	if !ok {
		r.mu.Unlock()
		return Result{Text: fmt.Sprintf("tool %q not granted", c.Name), IsError: true}
	}
	if err := checkPolicy(e.grant, c.Arguments); err != nil {
		r.mu.Unlock()
		return Result{Text: err.Error(), IsError: true}
	}
	if e.grant.MaxCalls > 0 && e.calls >= e.grant.MaxCalls {
		r.mu.Unlock()
		return Result{Text: fmt.Sprintf("tool %q max calls (%d) exceeded", c.Name, e.grant.MaxCalls), IsError: true}
	}
	if e.cooldown > 0 && time.Since(e.lastCall) < e.cooldown {
		r.mu.Unlock()
		return Result{Text: fmt.Sprintf("tool %q in cooldown", c.Name), IsError: true}
	}
	e.calls++
	e.lastCall = time.Now()
	r.mu.Unlock()

	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}
	res, err := e.tool.Execute(ctx, c.Arguments)
	if err != nil {
		res = Result{Text: err.Error(), IsError: true}
	}
	res.Text = r.Redact(res.Text)
	return res
}

// Redact replaces configured secrets in s with ***.
func (r *Registry) Redact(s string) string {
	for _, secret := range r.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

func checkPolicy(g agentmd.ToolGrant, args json.RawMessage) error {
	switch g.Name {
	case "exec":
		var a struct {
			Argv []string `json:"argv"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Errorf("exec: invalid arguments: %w", err)
		}
		if len(a.Argv) == 0 {
			return fmt.Errorf("exec: argv must not be empty")
		}
		for _, prefix := range g.Allow {
			if len(prefix) <= len(a.Argv) && equalStrings(prefix, a.Argv[:len(prefix)]) {
				return nil
			}
		}
		return fmt.Errorf("exec: argv %v denied by allow list", a.Argv)
	case "http":
		var a struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Errorf("http: invalid arguments: %w", err)
		}
		host := urlHost(a.URL)
		for _, h := range g.AllowHosts {
			if host == h {
				return nil
			}
		}
		return fmt.Errorf("http: host %q denied by allow list", host)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
