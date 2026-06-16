// Package a2a is a minimal, A2A-conformant HTTP+SSE surface shared by the in-pod
// agent server, the gateway, and clients. It deliberately implements only the
// subset the system needs (agent card, invoke, SSE streaming, health) with the
// standard library — see docs/architecture.md decision D9.
package a2a

// AgentCard is served at /.well-known/agent-card.json so callers can discover
// what an agent is and whether it streams.
type AgentCard struct {
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	URL          string       `json:"url"`
	Version      string       `json:"version"`
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities advertises optional protocol features.
type Capabilities struct {
	Streaming bool `json:"streaming"`
}

// InvokeRequest is the body of POST /invoke.
type InvokeRequest struct {
	TaskID string `json:"taskId,omitempty"`
	Prompt string `json:"prompt"`
	// Repo/Output/Model override the agent defaults when set.
	Repo   string `json:"repo,omitempty"`
	Output string `json:"output,omitempty"`
	Model  string `json:"model,omitempty"`
}

// EventType enumerates the SSE event kinds streamed from /invoke.
type EventType string

const (
	// EventStatus reports a lifecycle transition (running, completing).
	EventStatus EventType = "status"
	// EventArtifact reports incremental output (a progress note or text chunk).
	EventArtifact EventType = "artifact"
	// EventResult is the terminal success event carrying the final output.
	EventResult EventType = "result"
	// EventError is the terminal failure event.
	EventError EventType = "error"
)

// Event is one SSE frame. Exactly one of the payload fields is set per Type.
type Event struct {
	Type     EventType `json:"type"`
	State    string    `json:"state,omitempty"`
	Artifact *Artifact `json:"artifact,omitempty"`
	Result   *Result   `json:"result,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Artifact is a unit of streamed output.
type Artifact struct {
	Name string `json:"name,omitempty"`
	Text string `json:"text"`
}

// Result is the final externalized outcome of a run.
type Result struct {
	Output    string   `json:"output,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}
