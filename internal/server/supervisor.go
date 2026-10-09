package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yurifrl/agents/internal/protocol"
	"github.com/yurifrl/agents/internal/rpc"
	"github.com/yurifrl/agents/internal/surface"
)

// SupervisorOpts tunes the supervisor (zero values = defaults).
type SupervisorOpts struct {
	// MaxTime is the wall-clock backstop per task; 0 disables.
	MaxTime time.Duration
	// SettleQuiet is the quiet window after agent_end before settling without
	// agent_settled (some omp builds never emit it). 0 disables; default 3s.
	SettleQuiet time.Duration
	OmpBin      string
	Model       string
	// AdoptionConnect / AdoptionHandshake / AdoptionPoll tune re-adoption.
	AdoptionConnect   time.Duration
	AdoptionHandshake time.Duration
	AdoptionPoll      time.Duration
}

func (o SupervisorOpts) maxTime() time.Duration {
	if o.MaxTime == 0 {
		return 30 * time.Minute
	}
	return o.MaxTime
}

func (o SupervisorOpts) settleQuiet() time.Duration {
	if o.SettleQuiet == 0 {
		return 3 * time.Second
	}
	return o.SettleQuiet
}

func (o SupervisorOpts) adoptionConnect() time.Duration {
	if o.AdoptionConnect == 0 {
		return 3 * time.Second
	}
	return o.AdoptionConnect
}

func (o SupervisorOpts) adoptionHandshake() time.Duration {
	if o.AdoptionHandshake == 0 {
		return 3 * time.Second
	}
	return o.AdoptionHandshake
}

func (o SupervisorOpts) adoptionPoll() time.Duration {
	if o.AdoptionPoll == 0 {
		return time.Second
	}
	return o.AdoptionPoll
}

// transitions are the legal lifecycle edges; every transition is guarded on
// the current status.
var transitions = map[protocol.TaskStatus][]protocol.TaskStatus{
	protocol.StatusQueued:     {protocol.StatusStarting, protocol.StatusFinalizing, protocol.StatusCanceled, protocol.StatusFailed},
	protocol.StatusStarting:   {protocol.StatusRunning, protocol.StatusFinalizing, protocol.StatusCanceled, protocol.StatusFailed},
	protocol.StatusRunning:    {protocol.StatusFinalizing, protocol.StatusCanceled, protocol.StatusFailed},
	protocol.StatusFinalizing: {protocol.StatusCompleted, protocol.StatusFailed, protocol.StatusCanceled},
	protocol.StatusCompleted:  {},
	protocol.StatusFailed:     {},
	protocol.StatusCanceled:   {},
}

// Supervisor owns worker lifecycles: queued → starting → running → finalizing
// → completed | failed | canceled, re-adoption after restart, steer/cancel/ack,
// and the wall-clock deadline backstop.
type Supervisor struct {
	store   *TaskStore
	surface surface.Surface
	opts    SupervisorOpts
	kind    protocol.SurfaceKind

	mu             sync.Mutex
	rpcs           map[string]*rpc.RPC
	workers        map[string]surface.RunningWorker
	deadlines      map[string]*time.Timer
	settleTimers   map[string]*time.Timer
	lastAssistant  map[string]string
	pendingSteers  map[string][]string
	settledWaiters map[string][]chan *protocol.Task
}

// NewSupervisor wires a supervisor over a store and one surface.
func NewSupervisor(store *TaskStore, surf surface.Surface, opts SupervisorOpts) *Supervisor {
	return &Supervisor{
		store:          store,
		surface:        surf,
		opts:           opts,
		kind:           surf.Kind(),
		rpcs:           map[string]*rpc.RPC{},
		workers:        map[string]surface.RunningWorker{},
		deadlines:      map[string]*time.Timer{},
		settleTimers:   map[string]*time.Timer{},
		lastAssistant:  map[string]string{},
		pendingSteers:  map[string][]string{},
		settledWaiters: map[string][]chan *protocol.Task{},
	}
}

// Kind reports the supervisor's surface kind.
func (s *Supervisor) Kind() protocol.SurfaceKind { return s.kind }

// Create queues a task and launches it in the background.
func (s *Supervisor) Create(input protocol.CreateTaskInput) protocol.Task {
	task := s.store.Create(input, s.kind)
	go s.launch(task.ID) // routes its own failures into the store
	return task
}

// WhenSettled resolves once the task reaches a terminal status (observation
// seam). The channel receives the final task (or nil if it vanished) exactly
// once.
func (s *Supervisor) WhenSettled(id string) <-chan *protocol.Task {
	if task := s.store.Get(id); task != nil && protocol.IsTerminalStatus(task.Status) {
		ch := make(chan *protocol.Task, 1)
		ch <- task
		return ch
	}
	ch := make(chan *protocol.Task, 1)
	s.mu.Lock()
	s.settledWaiters[id] = append(s.settledWaiters[id], ch)
	s.mu.Unlock()
	return ch
}

// Steer sends a mid-turn follow-up to a running worker. Queued tasks buffer
// the message until the initial prompt has landed.
func (s *Supervisor) Steer(ctx context.Context, id, message string) (protocol.Task, error) {
	task := s.store.Get(id)
	if task == nil {
		return protocol.Task{}, notFound(fmt.Sprintf("task not found: %s", id))
	}
	if strings.TrimSpace(message) == "" {
		return protocol.Task{}, badRequest("message must be a non-empty string")
	}
	if task.Status == protocol.StatusQueued {
		s.mu.Lock()
		s.pendingSteers[id] = append(s.pendingSteers[id], message)
		s.mu.Unlock()
		return *task, nil
	}
	if task.Status != protocol.StatusRunning {
		return protocol.Task{}, conflict(fmt.Sprintf("task is not steerable (status %s)", task.Status))
	}
	s.mu.Lock()
	worker := s.rpcs[id]
	s.mu.Unlock()
	if worker == nil {
		return protocol.Task{}, conflict("worker is not attached")
	}
	if _, err := worker.Steer(ctx, message); err != nil {
		return protocol.Task{}, err
	}
	_, _ = s.store.AppendEvent(id, protocol.EventLog, protocol.TaskEventData{Line: new("steered: " + message)})
	updated := s.store.Get(id)
	if updated == nil {
		return *task, nil
	}
	return *updated, nil
}

// Cancel requests a graceful stop and then forces it.
func (s *Supervisor) Cancel(ctx context.Context, id string) (protocol.Task, error) {
	task := s.store.Get(id)
	if task == nil {
		return protocol.Task{}, notFound(fmt.Sprintf("task not found: %s", id))
	}
	if protocol.IsTerminalStatus(task.Status) {
		return protocol.Task{}, conflict(fmt.Sprintf("task already %s", task.Status))
	}
	if task.Status == protocol.StatusFinalizing {
		return protocol.Task{}, conflict("task is finalizing")
	}
	_, _ = s.store.Update(id, TaskPatch{StopRequested: new(true)})
	if task.Status == protocol.StatusQueued {
		// No worker was spawned yet; a launch in flight notices and backs off.
		s.transition(id, protocol.StatusCanceled, "canceled before start")
		s.teardown(id)
		return *s.store.Get(id), nil
	}
	s.mu.Lock()
	worker := s.rpcs[id]
	running := s.workers[id]
	s.mu.Unlock()
	if worker != nil {
		go func() {
			defer func() { _ = recover() }()
			_, _ = worker.Abort(ctx) // graceful stop signal; Stop below is the hammer
		}()
	}
	if running.Stop != nil {
		running.Stop()
	}
	s.finishCancel(id, "cancel requested") // idempotent if a close/settled handler won the race
	final := s.store.Get(id)
	if final == nil {
		return *task, nil
	}
	return *final, nil
}

// Ack marks the task's result as consumed by the parent.
func (s *Supervisor) Ack(id string) (protocol.Task, error) {
	task, err := s.store.Ack(id)
	if err != nil {
		return protocol.Task{}, notFound(err.Error())
	}
	return task, nil
}

// AdoptExisting re-adopts active tasks at boot: herdr workers (meta.port) are
// re-attached over TCP and polled to completion; process workers are checked
// with kill(pid, 0) and marked failed with a resume hint. Returns the number
// of workers adopted.
func (s *Supervisor) AdoptExisting() int {
	active := s.store.All()
	adopted := 0
	for i := range active {
		if !protocol.IsActiveStatus(active[i].Status) {
			continue
		}
		if s.adoptTask(active[i].ID) {
			adopted++
		}
	}
	return adopted
}

// Shutdown is the graceful path: process-surface workers are children of this
// server (piped stdio) and would die anyway, so they are aborted and marked
// canceled. Herdr workers live in their own panes and are left running so the
// next boot re-adopts them.
func (s *Supervisor) Shutdown() {
	active := s.store.All()
	for i := range active {
		task := active[i]
		if !protocol.IsActiveStatus(task.Status) {
			continue
		}
		if s.kind != protocol.SurfaceProcess {
			continue
		}
		_, _ = s.store.Update(task.ID, TaskPatch{StopRequested: new(true)})
		s.mu.Lock()
		worker := s.rpcs[task.ID]
		running := s.workers[task.ID]
		s.mu.Unlock()
		if worker != nil {
			go func() {
				defer func() { _ = recover() }()
				_, _ = worker.Abort(context.Background())
			}()
		}
		if running.Stop != nil {
			running.Stop()
		}
		s.transition(task.ID, protocol.StatusCanceled, "server shutdown")
		s.teardown(task.ID)
	}
	_ = s.store.SaveNow()
}

// ------------------------------------------------------------------ launch

func (s *Supervisor) launch(id string) {
	task := s.store.Get(id)
	if task == nil || protocol.IsTerminalStatus(task.Status) {
		return
	}
	s.armDeadline(id)

	spec := surface.WorkerSpec{
		TaskID:     id,
		Name:       task.Name,
		Cwd:        task.Cwd,
		SessionDir: filepath.Join(s.store.StateDir(), protocol.WorkersDirname, id),
		OmpBin:     s.opts.OmpBin,
		Model:      task.Model,
		MaxTimeS:   int(s.opts.maxTime() / time.Second),
	}
	if spec.Model == "" {
		spec.Model = s.opts.Model
	}
	if spec.MaxTimeS < 1 {
		spec.MaxTimeS = 1
	}
	if err := os.MkdirAll(spec.SessionDir, 0o755); err != nil {
		s.fail(id, fmt.Sprintf("failed to create session dir: %s", err))
		return
	}
	worker, err := s.surface.Start(spec)
	if err != nil {
		s.fail(id, fmt.Sprintf("failed to start worker: %s", err))
		return
	}
	if !s.isActive(id) {
		// Canceled (or otherwise settled) while the surface was starting.
		worker.Stop()
		return
	}
	if !s.transition(id, protocol.StatusStarting, "") {
		worker.Stop()
		return
	}
	_, _ = s.store.Update(id, TaskPatch{Worker: &worker.Meta})
	workerRPC := rpc.New(worker.Channel)
	s.mu.Lock()
	s.rpcs[id] = workerRPC
	s.workers[id] = worker
	s.mu.Unlock()
	workerRPC.SetEventHandler(func(ev rpc.Event) { s.onWorkerEvent(id, ev) })
	workerRPC.OnClose(func() { s.onChannelClosed(id, worker) })

	ready, err := workerRPC.Start(context.Background())
	if err != nil {
		s.fail(id, fmt.Sprintf("worker handshake failed: %s", err))
		s.teardown(id)
		return
	}
	if ready.SessionFile != "" {
		if cur := s.store.Get(id); cur != nil && cur.SessionFile == "" {
			_, _ = s.store.Update(id, TaskPatch{SessionFile: new(ready.SessionFile)})
		}
	}
	if !s.transition(id, protocol.StatusRunning, "") {
		s.teardown(id)
		return
	}
	// A slow/partial prompt response must not kill a healthy streaming worker
	// (agent_settled + the deadline remain the authority).
	if _, err := workerRPC.Prompt(context.Background(), task.Task); err != nil && s.isActive(id) {
		_, _ = s.store.AppendEvent(id, protocol.EventLog, protocol.TaskEventData{Line: new(fmt.Sprintf("prompt response error: %s", err))})
	}
	s.flushPendingSteers(id)
}

// recordSessionFile falls back to globbing the worker's session dir when the
// ready frame carried no sessionFile (observed on the installed omp build).
// Picks the newest *.jsonl — the worker creates its session file with the
// first message, so this must run after the turn has started.
func (s *Supervisor) recordSessionFile(id string) {
	if cur := s.store.Get(id); cur != nil && cur.SessionFile != "" {
		return
	}
	sessionDir := filepath.Join(s.store.StateDir(), protocol.WorkersDirname, id)
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return
	}
	type candidate struct {
		path  string
		mtime time.Time
	}
	var files []candidate
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, candidate{filepath.Join(sessionDir, e.Name()), info.ModTime()})
	}
	if len(files) == 0 {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	_, _ = s.store.Update(id, TaskPatch{SessionFile: new(files[0].path)})
}

func (s *Supervisor) flushPendingSteers(id string) {
	s.mu.Lock()
	pending := s.pendingSteers[id]
	delete(s.pendingSteers, id)
	worker := s.rpcs[id]
	s.mu.Unlock()
	if len(pending) == 0 || worker == nil {
		return
	}
	for _, message := range pending {
		if _, err := worker.Steer(context.Background(), message); err != nil {
			// Worker already gone; the task state will reflect that shortly.
			continue
		}
		_, _ = s.store.AppendEvent(id, protocol.EventLog, protocol.TaskEventData{Line: new("steered: " + message)})
	}
}

// ------------------------------------------------------------ rpc plumbing

func (s *Supervisor) onWorkerEvent(id string, ev rpc.Event) {
	switch ev["type"] {
	case "tool_execution_start":
		toolName := firstString(ev, "toolName", "name", "tool")
		if toolName == "" {
			toolName = "tool"
		}
		args, hasArgs := ev["args"]
		if !hasArgs {
			args, hasArgs = ev["input"]
		}
		if !hasArgs {
			args, hasArgs = ev["arguments"]
		}
		data := protocol.TaskEventData{ToolName: toolName}
		if hasArgs {
			data.Detail = shortDetail(args)
		}
		_, _ = s.store.AppendEvent(id, protocol.EventActivity, data)
	case "message_end":
		if text := assistantTextOf(ev); text != "" {
			s.mu.Lock()
			s.lastAssistant[id] = text
			s.mu.Unlock()
		}
	case "agent_settled":
		s.clearSettleQuiet(id)
		go s.settleFromWorker(id)
	case "agent_end":
		s.armSettleQuiet(id)
	case "agent_start", "turn_start", "message_start":
		// A follow-up turn (retry, compaction, steer) began; not idle after all.
		s.clearSettleQuiet(id)
	}
	// default: forwarded verbatim rpc noise we do not act on
}

func firstString(ev rpc.Event, keys ...string) string {
	for _, key := range keys {
		if s, ok := ev[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// assistantTextOf extracts assistant text from a message_end rpc event; omp
// field shapes vary. Empty string means nothing usable.
func assistantTextOf(ev rpc.Event) string {
	if message, ok := ev["message"].(map[string]any); ok {
		if role, _ := message["role"].(string); role == "assistant" {
			switch content := message["content"].(type) {
			case string:
				return content
			case []any:
				var b strings.Builder
				for _, part := range content {
					switch p := part.(type) {
					case string:
						b.WriteString(p)
					case map[string]any:
						if text, ok := p["text"].(string); ok {
							b.WriteString(text)
						}
					}
				}
				return b.String()
			}
		}
	}
	text, _ := ev["text"].(string)
	return text
}

func shortDetail(args any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = []byte("unserializable args")
	}
	out := string(raw)
	if out == "" {
		out = "undefined"
	}
	if len(out) > 120 {
		return out[:117] + "..."
	}
	return out
}

// armSettleQuiet settles once the agent has been idle for settleQuiet. Some
// omp builds never emit agent_settled; agent_end plus this quiet window is
// then the only completion signal. The window re-arms itself while get_state
// still reports streaming (retry/compaction turns keep the task alive).
func (s *Supervisor) armSettleQuiet(id string) {
	quiet := s.opts.settleQuiet()
	if quiet <= 0 {
		return
	}
	s.clearSettleQuiet(id)
	timer := time.AfterFunc(quiet, func() {
		s.mu.Lock()
		delete(s.settleTimers, id)
		s.mu.Unlock()
		s.settleQuietly(id)
	})
	s.mu.Lock()
	s.settleTimers[id] = timer
	s.mu.Unlock()
}

func (s *Supervisor) clearSettleQuiet(id string) {
	s.mu.Lock()
	timer := s.settleTimers[id]
	delete(s.settleTimers, id)
	s.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

func (s *Supervisor) settleQuietly(id string) {
	s.mu.Lock()
	worker := s.rpcs[id]
	s.mu.Unlock()
	if worker != nil {
		resp, err := worker.GetState(context.Background())
		if err == nil {
			if streaming, ok := resp.Bool("isStreaming"); ok && streaming {
				s.armSettleQuiet(id)
				return
			}
		}
		// Channel already dead or state unclear: settle from what we have.
	}
	s.settleFromWorker(id)
}

func (s *Supervisor) settleFromWorker(id string) {
	task := s.store.Get(id)
	if task == nil || protocol.IsTerminalStatus(task.Status) || task.Status == protocol.StatusFinalizing {
		return
	}
	if task.StopRequested {
		s.finishCancel(id, "aborted by request")
		return
	}
	if !s.transition(id, protocol.StatusFinalizing, "") {
		return
	}
	var text string
	s.mu.Lock()
	worker := s.rpcs[id]
	s.mu.Unlock()
	if worker != nil {
		if resp, err := worker.GetLastAssistantText(context.Background()); err == nil {
			text = resp.String("text")
		}
	}
	s.recordSessionFile(id)
	if text == "" {
		s.mu.Lock()
		text = s.lastAssistant[id]
		s.mu.Unlock()
	}
	_, _ = s.store.Update(id, TaskPatch{Result: new(text)})
	_, _ = s.store.AppendEvent(id, protocol.EventResult, protocol.TaskEventData{Text: new(text)})
	s.transition(id, protocol.StatusCompleted, "")
	s.teardown(id)
}

func (s *Supervisor) onChannelClosed(id string, worker surface.RunningWorker) {
	task := s.store.Get(id)
	if task == nil || protocol.IsTerminalStatus(task.Status) || task.Status == protocol.StatusFinalizing {
		return
	}
	if task.StopRequested {
		s.finishCancel(id, "worker stopped")
		return
	}
	code := ""
	if ec, ok := worker.Channel.(surface.ExitCoder); ok {
		if c, ok := ec.ExitCode(); ok {
			code = fmt.Sprintf(" (code %d)", c)
		}
	}
	s.fail(id, "worker exited"+code)
}

func (s *Supervisor) onAdoptedChannelClosed(id string) {
	task := s.store.Get(id)
	if task == nil || protocol.IsTerminalStatus(task.Status) || task.Status == protocol.StatusFinalizing {
		return
	}
	if task.StopRequested {
		s.finishCancel(id, "worker stopped")
		return
	}
	s.fail(id, "worker connection closed before completion")
}

// ------------------------------------------------------------ transitions

func (s *Supervisor) transition(id string, to protocol.TaskStatus, reason string) bool {
	task := s.store.Get(id)
	if task == nil {
		return false
	}
	from := task.Status
	legal := false
	for _, next := range transitions[from] {
		if next == to {
			legal = true
			break
		}
	}
	if from == to || !legal {
		return false
	}
	now := time.Now().UnixMilli()
	patch := TaskPatch{Status: new(to)}
	if task.StartedAt == nil {
		patch.StartedAt = new(now)
	}
	if protocol.IsTerminalStatus(to) {
		patch.EndedAt = new(now)
	}
	_, _ = s.store.Update(id, patch)
	data := protocol.TaskEventData{From: from, To: to}
	if reason != "" {
		data.Reason = reason
	}
	_, _ = s.store.AppendEvent(id, protocol.EventStateChanged, data)
	if protocol.IsTerminalStatus(to) {
		s.resolveSettled(id)
	}
	return true
}

func (s *Supervisor) resolveSettled(id string) {
	s.mu.Lock()
	waiters := s.settledWaiters[id]
	delete(s.settledWaiters, id)
	s.mu.Unlock()
	task := s.store.Get(id)
	for _, ch := range waiters {
		ch <- task
	}
}

func (s *Supervisor) finishCancel(id, reason string) {
	if s.transition(id, protocol.StatusCanceled, reason) {
		go s.teardown(id)
	}
}

func (s *Supervisor) fail(id, message string) {
	task := s.store.Get(id)
	if task == nil || protocol.IsTerminalStatus(task.Status) {
		return
	}
	if task.StopRequested {
		// A cancel landed while we were failing; cancellation wins the race.
		s.finishCancel(id, message)
		return
	}
	_, _ = s.store.Update(id, TaskPatch{Error: new(message)})
	_, _ = s.store.AppendEvent(id, protocol.EventError, protocol.TaskEventData{Message: new(message)})
	if s.transition(id, protocol.StatusFailed, message) {
		go s.teardown(id)
	}
}

func (s *Supervisor) isActive(id string) bool {
	task := s.store.Get(id)
	return task != nil && protocol.IsActiveStatus(task.Status)
}

// -------------------------------------------------------------- resources

func (s *Supervisor) armDeadline(id string) {
	maxTime := s.opts.maxTime()
	if maxTime <= 0 {
		return
	}
	timer := time.AfterFunc(maxTime, func() {
		s.mu.Lock()
		delete(s.deadlines, id)
		s.mu.Unlock()
		s.fail(id, fmt.Sprintf("deadline exceeded (%s wall clock)", maxTime))
	})
	s.mu.Lock()
	s.deadlines[id] = timer
	s.mu.Unlock()
}

func (s *Supervisor) clearDeadline(id string) {
	s.mu.Lock()
	timer := s.deadlines[id]
	delete(s.deadlines, id)
	s.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

func (s *Supervisor) teardown(id string) {
	s.clearDeadline(id)
	s.clearSettleQuiet(id)
	s.mu.Lock()
	delete(s.lastAssistant, id)
	delete(s.pendingSteers, id)
	worker := s.rpcs[id]
	delete(s.rpcs, id)
	running := s.workers[id]
	delete(s.workers, id)
	s.mu.Unlock()
	if worker != nil {
		_ = worker.Close()
	}
	if running.Stop != nil {
		running.Stop() // must be idempotent
	}
}

// --------------------------------------------------------------- adoption

func (s *Supervisor) adoptTask(id string) bool {
	task := s.store.Get(id)
	if task == nil || !protocol.IsActiveStatus(task.Status) {
		return false
	}
	meta := task.Worker
	if meta == nil {
		s.fail(id, fmt.Sprintf("worker record missing (task was %s when the server restarted)", task.Status))
		return false
	}
	if meta.Port != nil {
		return s.adoptTCP(id, meta.Host, *meta.Port)
	}
	if meta.PID != nil {
		return s.adoptProcess(id, *meta.PID)
	}
	s.fail(id, fmt.Sprintf("worker record missing (task was %s when the server restarted)", task.Status))
	return false
}

func (s *Supervisor) adoptTCP(id string, host string, port int) bool {
	conn, err := dialTCP(host, port, s.opts.adoptionConnect())
	if err != nil {
		s.fail(id, fmt.Sprintf("worker bridge unreachable on %s:%d", hostOrDefault(host), port))
		return false
	}
	workerRPC := rpc.New(conn, rpc.WithHandshakeTimeout(s.opts.adoptionHandshake()))
	s.mu.Lock()
	s.rpcs[id] = workerRPC
	s.mu.Unlock()
	workerRPC.SetEventHandler(func(ev rpc.Event) { s.onWorkerEvent(id, ev) })
	workerRPC.OnClose(func() { s.onAdoptedChannelClosed(id) })
	// Tolerate bridges that do not replay ready; requests still work.
	_, _ = workerRPC.Start(context.Background())
	_, _ = s.store.AppendEvent(id, protocol.EventLog, protocol.TaskEventData{Line: new("readopted")})
	s.armDeadline(id)
	go s.pollAdopted(id)
	return true
}

func (s *Supervisor) adoptProcess(id string, pid int) bool {
	if pidAlive(pid) {
		s.fail(id, fmt.Sprintf("orphaned worker (pid %d) — resume via session file", pid))
		return false
	}
	s.fail(id, fmt.Sprintf("worker process (pid %d) exited before completion", pid))
	return false
}

func (s *Supervisor) pollAdopted(id string) {
	poll := s.opts.adoptionPoll()
	failures := 0
	for {
		time.Sleep(poll)
		task := s.store.Get(id)
		if task == nil || protocol.IsTerminalStatus(task.Status) {
			return
		}
		s.mu.Lock()
		worker := s.rpcs[id]
		s.mu.Unlock()
		if worker == nil {
			return
		}
		resp, err := worker.GetState(context.Background())
		if err != nil {
			failures++
			if failures >= 3 {
				s.fail(id, "worker unreachable after re-attach")
				return
			}
			continue
		}
		failures = 0
		if streaming, ok := resp.Bool("isStreaming"); ok && !streaming {
			s.settleFromWorker(id)
			return
		}
	}
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func dialTCP(host string, port int, budget time.Duration) (net.Conn, error) {
	addr := net.JoinHostPort(hostOrDefault(host), strconv.Itoa(port))
	deadline := time.Now().Add(budget)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("tcp connect to %s timed out", addr)
}

// hostOrDefault resolves the dial host for a bridge address; an empty host is
// the local loopback (herdr surface).
func hostOrDefault(host string) string {
	if strings.TrimSpace(host) == "" {
		return "127.0.0.1"
	}
	return host
}
