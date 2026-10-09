// Package protocol is the Go mirror of remote-agents/packages/protocol — the
// wire-shape source of truth shared with the TS extension. JSON tags MUST
// match the TypeScript types exactly; the extension must never need changes
// for a server-side field rename.
package protocol

import "fmt"

// PROTOCOL_VERSION mirrors PROTOCOL_VERSION in the TS protocol.
const PROTOCOL_VERSION = 1

// DEFAULT_PORT mirrors DEFAULT_PORT in the TS protocol.
const DEFAULT_PORT = 8679

// File layout constants inside the state dir.
const (
	StateDirFilename = "state.json"
	EventsDirname    = "events"
	WorkersDirname   = "workers"
	LogsDirname      = "logs"
)

// Well-known environment variables (mirrors ENV in the TS protocol).
const (
	EnvURL       = "REMOTE_AGENTS_URL"
	EnvPort      = "REMOTE_AGENTS_PORT"
	EnvStateDir  = "REMOTE_AGENTS_STATE_DIR"
	EnvSurface   = "REMOTE_AGENTS_SURFACE"
	EnvOmpBin    = "REMOTE_AGENTS_OMP_BIN"
	EnvModel     = "REMOTE_AGENTS_MODEL"
	EnvMaxTimeS  = "REMOTE_AGENTS_MAX_TIME_S"
	EnvOwner     = "REMOTE_AGENTS_OWNER"
	EnvServerBin = "REMOTE_AGENTS_SERVER_BIN"

	// K8s surface pod settings (agents server with REMOTE_AGENTS_SURFACE=k8s).
	EnvK8sNamespace = "REMOTE_AGENTS_K8S_NAMESPACE"
	EnvK8sImage     = "REMOTE_AGENTS_K8S_IMAGE"
	EnvK8sSecret    = "REMOTE_AGENTS_K8S_SECRET"
)

// TaskStatus is the A2A-aligned task lifecycle. Terminal: completed | failed | canceled.
type TaskStatus string

const (
	StatusQueued     TaskStatus = "queued"
	StatusStarting   TaskStatus = "starting"
	StatusRunning    TaskStatus = "running"
	StatusFinalizing TaskStatus = "finalizing"
	StatusCompleted  TaskStatus = "completed"
	StatusFailed     TaskStatus = "failed"
	StatusCanceled   TaskStatus = "canceled"
)

// ActiveStatuses are the non-terminal statuses.
var ActiveStatuses = []TaskStatus{StatusQueued, StatusStarting, StatusRunning, StatusFinalizing}

// IsActiveStatus reports whether s is a non-terminal status.
func IsActiveStatus(s TaskStatus) bool {
	switch s {
	case StatusQueued, StatusStarting, StatusRunning, StatusFinalizing:
		return true
	}
	return false
}

// IsTerminalStatus reports whether s is completed, failed or canceled.
func IsTerminalStatus(s TaskStatus) bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCanceled
}

// SurfaceKind is how the worker process is hosted.
type SurfaceKind string

const (
	SurfaceProcess SurfaceKind = "process"
	SurfaceHerdr   SurfaceKind = "herdr"
	SurfaceK8s     SurfaceKind = "k8s"
)

// TaskWorkerMeta describes the running worker (surface-specific fields).
type TaskWorkerMeta struct {
	PID         *int   `json:"pid,omitempty"`
	PaneID      string `json:"paneId,omitempty"`      // herdr surface: pane id ("w1:p3")
	WorkspaceID string `json:"workspaceId,omitempty"` // herdr surface
	Port        *int   `json:"port,omitempty"`        // herdr/k8s surface: TCP bridge port
	PortFile    string `json:"portFile,omitempty"`    // herdr surface: bound-port file path
	Host        string `json:"host,omitempty"`        // k8s surface: pod IP (default 127.0.0.1)
}

// Task is one unit of remote agent work.
type Task struct {
	ID            string          `json:"id"`
	Owner         string          `json:"owner"`
	Name          string          `json:"name"`
	Task          string          `json:"task"`
	Cwd           string          `json:"cwd"`
	Model         string          `json:"model,omitempty"`
	Status        TaskStatus      `json:"status"`
	Surface       SurfaceKind     `json:"surface"`
	Acknowledged  bool            `json:"acknowledged"`
	CreatedAt     int64           `json:"createdAt"`
	StartedAt     *int64          `json:"startedAt,omitempty"`
	EndedAt       *int64          `json:"endedAt,omitempty"`
	Result        *string         `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
	SessionFile   string          `json:"sessionFile,omitempty"`
	ParentSession string          `json:"parentSession,omitempty"`
	Worker        *TaskWorkerMeta `json:"worker,omitempty"`
	StopRequested bool            `json:"stopRequested,omitempty"`
	// RecentEvents is embedded in detail responses only (last 10 events).
	RecentEvents []TaskEvent `json:"recentEvents,omitempty"`
}

// CreateTaskInput is the POST /tasks body.
type CreateTaskInput struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	Task          string `json:"task"`
	Cwd           string `json:"cwd"`
	Model         string `json:"model,omitempty"`
	ParentSession string `json:"parentSession,omitempty"`
}

// TaskEventType is the event kind (event log + SSE wire).
type TaskEventType string

const (
	EventStateChanged TaskEventType = "state_changed"
	EventActivity     TaskEventType = "activity"
	EventResult       TaskEventType = "result"
	EventError        TaskEventType = "error"
	EventLog          TaskEventType = "log"
)

// TaskEventData is the event payload union. Pointers mark keys that are always
// present inside their variant (so an empty string still serializes), while
// omitempty marks optional keys — this reproduces the exact TS object shapes.
// Construct with new(s) / new(status) per Go 1.26 new(expr).
type TaskEventData struct {
	From     TaskStatus `json:"from,omitempty"`
	To       TaskStatus `json:"to,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	ToolName string     `json:"toolName,omitempty"`
	Detail   string     `json:"detail,omitempty"`
	Text     *string    `json:"text,omitempty"`
	Message  *string    `json:"message,omitempty"`
	Line     *string    `json:"line,omitempty"`
}

// Clone returns a deep copy of the task.
func (t *Task) Clone() *Task {
	out := *t
	if t.Worker != nil {
		w := *t.Worker
		out.Worker = &w
	}
	if t.RecentEvents != nil {
		out.RecentEvents = append([]TaskEvent(nil), t.RecentEvents...)
	}
	return &out
}

// Clone returns a deep copy of the worker meta.
func (m *TaskWorkerMeta) Clone() *TaskWorkerMeta {
	out := *m
	return &out
}

// TaskEvent is one entry in a task's append-only event log.
type TaskEvent struct {
	// Seq is monotonic per task, starting at 1.
	Seq    int64         `json:"seq"`
	TaskID string        `json:"taskId"`
	Type   TaskEventType `json:"type"`
	Data   TaskEventData `json:"data"`
	Ts     int64         `json:"ts"`
}

// SseFrame is the SSE frame payload (data: {...}).
type SseFrame struct {
	TaskID string        `json:"taskId"`
	Seq    int64         `json:"seq"`
	Type   TaskEventType `json:"type"`
	Data   TaskEventData `json:"data"`
	Ts     int64         `json:"ts"`
}

// AttachResponse is the POST /attach response.
type AttachResponse struct {
	Tasks  []Task      `json:"tasks"`
	Server ServerFlags `json:"server"`
}

// ServerFlags describes the server in attach/health responses.
type ServerFlags struct {
	UptimeMs int64       `json:"uptimeMs"`
	Surface  SurfaceKind `json:"surface"`
	Version  string      `json:"version"`
}

// HealthResponse is the GET /health response.
type HealthResponse struct {
	Ok       bool        `json:"ok"`
	Version  string      `json:"version"`
	Surface  SurfaceKind `json:"surface"`
	UptimeMs int64       `json:"uptimeMs"`
	Tasks    struct {
		Active int `json:"active"`
		Total  int `json:"total"`
	} `json:"tasks"`
}

// REST paths (single source of truth, mirrors API in the TS protocol).
const (
	PathHealth      = "/health"
	PathTasks       = "/tasks"
	PathAttach      = "/attach"
	PathTasksPrefix = "/tasks/"
)

func PathTask(id string) string   { return PathTasksPrefix + id }
func PathSteer(id string) string  { return PathTasksPrefix + id + "/steer" }
func PathCancel(id string) string { return PathTasksPrefix + id + "/cancel" }
func PathAck(id string) string    { return PathTasksPrefix + id + "/ack" }
func PathStream(id string) string { return PathTasksPrefix + id + "/stream" }
func PathStreamSince(id string, since int64) string {
	return fmt.Sprintf("%s%s/stream?since=%d", PathTasksPrefix, id, since)
}
