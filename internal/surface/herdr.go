package surface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yurifrl/agents/internal/protocol"
	"github.com/yurifrl/agents/internal/surface/bridge"
)

// herdr timing knobs (mirror the TS surface).
const (
	DefaultHerdrBin       = "herdr"
	DefaultHerdrWorkspace = "remote-agents"

	portPollInterval = 500 * time.Millisecond
	portPollBudget   = 30 * time.Second
	paneRunTimeout   = 20 * time.Second
	cliTimeout       = 60 * time.Second
	paneCloseTimeout = 10 * time.Second
	dialTimeout      = 5 * time.Second
)

// HerdrSurface runs workers inside a herdr pane, bridged over TCP so they
// survive server restarts (CONTRACT.md §3):
//  1. ensure the workspace exists (workspace list / workspace create)
//  2. create a dedicated tab (tab create --workspace ... --label ...) and use
//     its returned root pane — the workspace root pane stays idle
//  3. write bridge.mjs + an empty port file into the session dir
//  4. pane run <paneId> "<quoted bridge command>" (execFile-style argv)
//  5. poll the port file, then dial 127.0.0.1:<port>
//  6. stop() closes the socket and the pane (best effort)
type HerdrSurface struct {
	// Bin is the herdr CLI executable.
	Bin string
	// Workspace is the fallback workspace label for headless servers.
	Workspace string
	// BridgePath overrides where bridge.mjs is written (default: sessionDir).
	BridgePath string
}

// NewHerdrSurface builds the herdr surface with defaults.
func NewHerdrSurface() *HerdrSurface {
	return &HerdrSurface{Bin: DefaultHerdrBin, Workspace: DefaultHerdrWorkspace}
}

// Kind reports the surface kind.
func (s *HerdrSurface) Kind() protocol.SurfaceKind { return protocol.SurfaceHerdr }

// Start launches the worker in a fresh herdr tab behind a TCP bridge.
func (s *HerdrSurface) Start(spec WorkerSpec) (RunningWorker, error) {
	// Visible by default: when the server inherited a herdr session context
	// (extension-spawned via HERDR_WORKSPACE_ID), land the worker tab in THAT
	// workspace so the user sees the agent working next to their session. The
	// dedicated "remote-agents" workspace is only the headless fallback.
	workspaceID := inheritedWorkspaceID()
	if workspaceID == "" {
		id, err := s.ensureWorkspace(s.Workspace)
		if err != nil {
			return RunningWorker{}, fmt.Errorf("herdr workspace: %w", err)
		}
		workspaceID = id
	}

	paneID, err := s.createTab(workspaceID, spec)
	if err != nil {
		return RunningWorker{}, err
	}
	go s.reportTask(paneID, spec) // cosmetic; never fails the launch

	if err := os.MkdirAll(spec.SessionDir, 0o755); err != nil {
		s.closePaneBestEffort(paneID)
		return RunningWorker{}, fmt.Errorf("session dir: %w", err)
	}
	bridgePath := s.BridgePath
	if bridgePath == "" {
		bridgePath = filepath.Join(spec.SessionDir, "bridge.mjs")
	}
	if err := os.MkdirAll(filepath.Dir(bridgePath), 0o755); err != nil {
		s.closePaneBestEffort(paneID)
		return RunningWorker{}, fmt.Errorf("bridge dir: %w", err)
	}
	if err := os.WriteFile(bridgePath, []byte(bridge.Script), 0o644); err != nil {
		s.closePaneBestEffort(paneID)
		return RunningWorker{}, fmt.Errorf("write bridge: %w", err)
	}
	portFile := filepath.Join(spec.SessionDir, "port")
	if err := os.WriteFile(portFile, nil, 0o644); err != nil {
		s.closePaneBestEffort(paneID)
		return RunningWorker{}, fmt.Errorf("write port file: %w", err)
	}

	command := paneCommand(spec, bridgePath, portFile)
	res := s.run([]string{"pane", "run", paneID, command}, paneRunTimeout)
	if !res.timedOut && res.code != 0 {
		s.closePaneBestEffort(paneID)
		return RunningWorker{}, fmt.Errorf("herdr pane run exited %d: %s", res.code, cliDetail(res))
	}

	port := waitForPort(portFile, portPollInterval, portPollBudget)
	if port == 0 {
		s.closePaneBestEffort(paneID)
		return RunningWorker{}, fmt.Errorf("port file %s never appeared within %s", portFile, portPollBudget)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), dialTimeout)
	if err != nil {
		s.closePaneBestEffort(paneID)
		return RunningWorker{}, fmt.Errorf("TCP connect to bridge port %d failed: %w", port, err)
	}

	meta := protocol.TaskWorkerMeta{
		PaneID:      paneID,
		WorkspaceID: workspaceID,
		Port:        new(port),
		PortFile:    portFile,
	}
	return RunningWorker{
		Channel: conn,
		Meta:    meta,
		Stop: func() {
			_ = conn.Close()
			s.closePaneBestEffort(paneID)
		},
	}, nil
}

// inheritedWorkspaceID returns HERDR_WORKSPACE_ID when set (pi-herdr-subagents
// convention): worker tabs land in the user's current workspace.
func inheritedWorkspaceID() string {
	return strings.TrimSpace(os.Getenv("HERDR_WORKSPACE_ID"))
}

// ensureWorkspace find-or-creates the workspace by label; returns its id.
func (s *HerdrSurface) ensureWorkspace(name string) (string, error) {
	payload, err := s.runJSON([]string{"workspace", "list"})
	if err != nil {
		return "", err
	}
	if id := workspaceIDFromList(payload, name); id != "" {
		return id, nil
	}
	created, err := s.runJSON([]string{"workspace", "create", "--label", name, "--no-focus"})
	if err != nil {
		return "", err
	}
	result := resultOf(created)
	if id := pickString(recordOf(result["workspace"]), "workspace_id", "workspaceId", "id"); id != "" {
		return id, nil
	}
	if id := pickString(result, "workspace_id", "workspaceId", "id"); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("herdr workspace create output carried no workspace id")
}

// reportTask sets best-effort herdr UI metadata so the pane shows what the
// agent is doing (pi-herdr-subagents report-metadata convention).
func (s *HerdrSurface) reportTask(paneID string, spec WorkerSpec) {
	defer func() { _ = recover() }()
	_ = s.run([]string{"pane", "report-metadata", paneID, "--source", "remote-agents", "--token", "task=" + spec.Name}, cliTimeout)
	_ = s.run([]string{"pane", "rename", paneID, "▸ " + spec.Name}, cliTimeout)
}

// createTab creates the task tab and returns its root pane id.
func (s *HerdrSurface) createTab(workspaceID string, spec WorkerSpec) (string, error) {
	created, err := s.runJSON([]string{
		"tab", "create",
		"--workspace", workspaceID,
		"--label", "▸ " + spec.Name,
		"--cwd", spec.Cwd,
		"--no-focus",
	})
	if err != nil {
		return "", err
	}
	result := resultOf(created)
	if id := pickString(recordOf(result["root_pane"]), "pane_id", "paneId", "id"); id != "" {
		return id, nil
	}
	if id := pickString(result, "root_pane_id", "pane_id", "paneId", "id"); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("herdr tab create output carried no root pane id")
}

type cliResult struct {
	code     int
	stdout   string
	stderr   string
	timedOut bool
}

// run executes one herdr CLI call with a timeout (SIGKILL on expiry).
func (s *HerdrSurface) run(args []string, timeout time.Duration) cliResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Bin, args...)
	cmd.Env = os.Environ()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return cliResult{timedOut: true}
	}
	code := 0
	if err != nil {
		code = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
	}
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// runJSON executes a CLI call and parses its stdout as a JSON object.
func (s *HerdrSurface) runJSON(args []string) (map[string]any, error) {
	res := s.run(args, cliTimeout)
	if res.timedOut {
		return nil, fmt.Errorf("%s %s timed out", s.Bin, strings.Join(args, " "))
	}
	if res.code != 0 {
		return nil, fmt.Errorf("%s %s exited %d: %s", s.Bin, strings.Join(args, " "), res.code, cliDetail(res))
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &parsed); err != nil {
		return nil, fmt.Errorf("unparseable JSON from %s %s: %w", s.Bin, strings.Join(args, " "), err)
	}
	return parsed, nil
}

func (s *HerdrSurface) closePaneBestEffort(paneID string) {
	if paneID == "" {
		return
	}
	defer func() { _ = recover() }()
	_ = s.run([]string{"pane", "close", paneID}, paneCloseTimeout)
}

// shellQuote quotes one argv item for a POSIX shell single-quoted context.
func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

// paneCommand builds the command typed into the pane shell: env vars +
// bun bridge -- omp argv.
func paneCommand(spec WorkerSpec, bridgePath, portFile string) string {
	parts := []string{
		"env",
		shellQuote(WorkerEnvVar + "=1"),
		shellQuote(TaskIDEnvVar + "=" + spec.TaskID),
		shellQuote(BunBin),
		shellQuote(bridgePath),
		"--port-file",
		shellQuote(portFile),
		"--",
	}
	for _, arg := range WorkerArgv(spec) {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// readPortFile returns the port from the port file, or 0 when not yet written.
func readPortFile(portFile string) int {
	data, err := os.ReadFile(portFile)
	if err != nil {
		return 0
	}
	var port int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &port); err != nil {
		return 0
	}
	if port <= 0 {
		return 0
	}
	return port
}

// waitForPort polls the port file until it carries a port or the budget ends.
func waitForPort(portFile string, interval, budget time.Duration) int {
	deadline := time.Now().Add(budget)
	for {
		time.Sleep(interval)
		if port := readPortFile(portFile); port != 0 {
			return port
		}
		if time.Now().After(deadline) {
			return 0
		}
	}
}

func resultOf(payload map[string]any) map[string]any {
	return recordOf(payload["result"])
}

func recordOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// pickString returns the first non-empty string among keys.
func pickString(record map[string]any, keys ...string) string {
	if record == nil {
		return ""
	}
	for _, key := range keys {
		if s, ok := record[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func workspaceIDFromList(payload map[string]any, label string) string {
	workspaces, ok := resultOf(payload)["workspaces"].([]any)
	if !ok {
		return ""
	}
	for _, entry := range workspaces {
		ws := recordOf(entry)
		if ws == nil {
			continue
		}
		if l, _ := ws["label"].(string); l != label {
			if n, _ := ws["name"].(string); n != label {
				continue
			}
		}
		if id := pickString(ws, "workspace_id", "workspaceId", "id"); id != "" {
			return id
		}
	}
	return ""
}

func cliDetail(res cliResult) string {
	detail := strings.TrimSpace(res.stderr)
	if detail == "" {
		detail = strings.TrimSpace(res.stdout)
	}
	if detail == "" {
		return "(no output)"
	}
	if len(detail) > 400 {
		detail = detail[:400]
	}
	return detail
}
