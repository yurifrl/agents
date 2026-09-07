// Package agentmd parses Agent.md definitions: YAML frontmatter plus a markdown
// body that is the agent's system prompt / instruction. The same file is the
// declarative unit consumed locally (pi subagents) and remotely (this system) —
// see the remote-agents design D2.
//
// +kubebuilder:object:generate=true
package agentmd

import (
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// ToolGrant declares one tool the agent may use, with optional constraints.
type ToolGrant struct {
	Name       string     `json:"name"`
	Allow      [][]string `json:"allow,omitempty"`
	AllowHosts []string   `json:"allowHosts,omitempty"`
	URLFromEnv string     `json:"urlFromEnv,omitempty"`
	MaxCalls   int        `json:"maxCalls,omitempty"`
	Timeout    string     `json:"timeout,omitempty"`
	Cooldown   string     `json:"cooldown,omitempty"`
}

// LoopSpec configures a recurring agent loop.
type LoopSpec struct {
	Interval string `json:"interval"`
}

// Definition is a parsed Agent.md.
type Definition struct {
	Name      string      `json:"name"`
	Runtime   string      `json:"runtime"`
	Model     string      `json:"model"`
	Tools     []ToolGrant `json:"tools"`
	Output    string      `json:"output"`
	Lifecycle string      `json:"lifecycle"`
	Loop      *LoopSpec   `json:"loop,omitempty"`
	// Instruction is the markdown body (the system prompt).
	Instruction string `json:"-"`
}

// LoopInterval returns the configured loop interval, or 0 when no loop is set.
func (d *Definition) LoopInterval() (time.Duration, error) {
	if d.Loop == nil {
		return 0, nil
	}
	if d.Loop.Interval == "" {
		return 0, fmt.Errorf("agent definition %q has a loop with an empty interval", d.Name)
	}
	dur, err := time.ParseDuration(d.Loop.Interval)
	if err != nil {
		return 0, fmt.Errorf("agent definition %q has an invalid loop interval %q: %w", d.Name, d.Loop.Interval, err)
	}
	return dur, nil
}

// Defaults applied when a field is omitted (design: runtime adk, output response,
// lifecycle ephemeral).
const (
	DefaultRuntime   = "adk"
	DefaultOutput    = "response"
	DefaultLifecycle = "ephemeral"
)

// Parse reads an Agent.md document (frontmatter + body) and applies defaults.
func Parse(content string) (*Definition, error) {
	front, body, err := split(content)
	if err != nil {
		return nil, err
	}

	var def Definition
	if err := yaml.Unmarshal([]byte(front), &def); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}
	def.Instruction = strings.TrimSpace(body)

	def.applyDefaults()
	if err := def.validate(); err != nil {
		return nil, err
	}
	return &def, nil
}

func (d *Definition) applyDefaults() {
	if d.Runtime == "" {
		d.Runtime = DefaultRuntime
	}
	if d.Output == "" {
		d.Output = DefaultOutput
	}
	if d.Lifecycle == "" {
		d.Lifecycle = DefaultLifecycle
	}
}

func (d *Definition) validate() error {
	if d.Name == "" {
		return fmt.Errorf("agent definition missing name")
	}
	switch d.Runtime {
	case "adk", "pi":
	default:
		return fmt.Errorf("invalid runtime %q (want adk|pi)", d.Runtime)
	}
	switch d.Output {
	case "response", "commit", "pr":
	default:
		return fmt.Errorf("invalid output %q (want response|commit|pr)", d.Output)
	}
	switch d.Lifecycle {
	case "ephemeral", "persistent":
	default:
		return fmt.Errorf("invalid lifecycle %q (want ephemeral|persistent)", d.Lifecycle)
	}
	for i, tool := range d.Tools {
		if tool.Name == "" {
			return fmt.Errorf("agent definition %q: tools[%d] missing name", d.Name, i)
		}
	}
	if _, err := d.LoopInterval(); err != nil {
		return err
	}
	if d.Instruction == "" {
		return fmt.Errorf("agent definition %q has an empty body (instruction)", d.Name)
	}
	return nil
}

// split separates leading YAML frontmatter (delimited by --- lines) from the body.
func split(content string) (front, body string, err error) {
	s := strings.TrimLeft(content, "\ufeff \t\r\n")
	if !strings.HasPrefix(s, "---") {
		return "", "", fmt.Errorf("missing frontmatter: file must start with ---")
	}
	rest := strings.TrimPrefix(s, "---")
	rest = strings.TrimLeft(rest, "\r\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", "", fmt.Errorf("unterminated frontmatter: missing closing ---")
	}
	front = rest[:idx]
	body = rest[idx+len("\n---"):]
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		body = body[nl+1:]
	} else {
		body = ""
	}
	return front, body, nil
}
