package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yurifrl/agents/internal/protocol"
)

const (
	mirrorCap      = 5000
	saveDebounce   = 25 * time.Millisecond
	appendFilePerm = 0o644
)

// TaskFilter narrows store listings.
type TaskFilter struct {
	Owner          string
	Unacknowledged bool
}

// TaskListener receives live events for one task. Listeners MUST NOT block:
// appendEvent notifies them synchronously under the store lock.
type TaskListener func(protocol.TaskEvent)

// TaskStore is the durable task store: state.json (all tasks, atomic
// tmp+rename, debounced) plus one append-only events/<taskId>.jsonl per task
// with a per-task monotonic seq starting at 1. A bounded in-memory mirror
// backs recentEvents and cheap eventsSince reads; the jsonl file is the
// source of truth once the mirror has been capped.
type TaskStore struct {
	stateDir  string
	eventsDir string
	stateFile string

	mu        sync.Mutex
	tasks     map[string]protocol.Task
	mirror    map[string][]protocol.TaskEvent
	seqs      map[string]int64
	listeners map[string]map[int]TaskListener
	nextLn    int
	saveTimer *time.Timer
}

// NewTaskStore loads (or initializes) the store rooted at stateDir.
func NewTaskStore(stateDir string) (*TaskStore, error) {
	s := &TaskStore{
		stateDir:  stateDir,
		eventsDir: filepath.Join(stateDir, protocol.EventsDirname),
		stateFile: filepath.Join(stateDir, protocol.StateDirFilename),
		tasks:     map[string]protocol.Task{},
		mirror:    map[string][]protocol.TaskEvent{},
		seqs:      map[string]int64{},
		listeners: map[string]map[int]TaskListener{},
	}
	if err := os.MkdirAll(s.eventsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	s.load()
	return s, nil
}

// StateDir returns the store root (used for worker session dirs).
func (s *TaskStore) StateDir() string { return s.stateDir }

func (s *TaskStore) load() {
	if data, err := os.ReadFile(s.stateFile); err == nil {
		var parsed struct {
			Tasks []protocol.Task `json:"tasks"`
		}
		if err := json.Unmarshal(data, &parsed); err == nil {
			for _, t := range parsed.Tasks {
				s.tasks[t.ID] = t
			}
		}
		// Corrupted state file: start clean rather than crash; events stay intact.
	}
	names, err := os.ReadDir(s.eventsDir)
	if err != nil {
		return
	}
	for _, name := range names {
		if filepath.Ext(name.Name()) != ".jsonl" {
			continue
		}
		taskID := name.Name()[:len(name.Name())-len(".jsonl")]
		events, err := s.readEventsFile(taskID)
		if err != nil || len(events) == 0 {
			continue
		}
		s.seqs[taskID] = events[len(events)-1].Seq
		if len(events) > mirrorCap {
			events = events[len(events)-mirrorCap:]
		}
		s.mirror[taskID] = events
	}
}

func (s *TaskStore) cloneTask(t protocol.Task) protocol.Task { return *t.Clone() }

// Create stores a new queued task and returns a copy.
func (s *TaskStore) Create(input protocol.CreateTaskInput, kind protocol.SurfaceKind) protocol.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := protocol.Task{
		ID:            uuid.NewString(),
		Owner:         input.Owner,
		Name:          input.Name,
		Task:          input.Task,
		Cwd:           input.Cwd,
		Model:         input.Model,
		ParentSession: input.ParentSession,
		Status:        protocol.StatusQueued,
		Surface:       kind,
		Acknowledged:  false,
		CreatedAt:     time.Now().UnixMilli(),
	}
	s.tasks[task.ID] = task
	s.scheduleSaveLocked()
	return s.cloneTask(task)
}

// Get returns a copy of the task, or nil.
func (s *TaskStore) Get(id string) *protocol.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil
	}
	return t.Clone()
}

// List returns copies sorted by createdAt then id.
func (s *TaskStore) List(filter TaskFilter) []protocol.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if filter.Owner != "" && t.Owner != filter.Owner {
			continue
		}
		if filter.Unacknowledged && t.Acknowledged {
			continue
		}
		out = append(out, *t.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// All lists every task.
func (s *TaskStore) All() []protocol.Task { return s.List(TaskFilter{}) }

// Update applies patch fields to the task and returns the updated copy.
func (s *TaskStore) Update(id string, patch TaskPatch) (protocol.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return protocol.Task{}, fmt.Errorf("task not found: %s", id)
	}
	patch.apply(&t)
	s.tasks[id] = t
	s.scheduleSaveLocked()
	return *t.Clone(), nil
}

// Ack marks the task's result as consumed.
func (s *TaskStore) Ack(id string) (protocol.Task, error) {
	return s.Update(id, TaskPatch{Acknowledged: new(true)})
}

// AppendEvent appends to the event log, notifies subscribers, and returns the event.
func (s *TaskStore) AppendEvent(taskID string, typ protocol.TaskEventType, data protocol.TaskEventData) (protocol.TaskEvent, error) {
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return protocol.TaskEvent{}, fmt.Errorf("task not found: %s", taskID)
	}
	seq := s.seqs[taskID] + 1
	s.seqs[taskID] = seq
	event := protocol.TaskEvent{
		Seq:    seq,
		TaskID: taskID,
		Type:   typ,
		Data:   data,
		Ts:     time.Now().UnixMilli(),
	}
	line, err := json.Marshal(event)
	if err != nil {
		s.mu.Unlock()
		return protocol.TaskEvent{}, fmt.Errorf("marshal event: %w", err)
	}
	if err := appendLine(filepath.Join(s.eventsDir, taskID+".jsonl"), line); err != nil {
		s.mu.Unlock()
		return protocol.TaskEvent{}, fmt.Errorf("append event: %w", err)
	}
	buf := append(s.mirror[taskID], event)
	if len(buf) > mirrorCap {
		buf = buf[len(buf)-mirrorCap:]
	}
	s.mirror[taskID] = buf
	subs := make([]TaskListener, 0, len(s.listeners[taskID]))
	for _, cb := range s.listeners[taskID] {
		subs = append(subs, cb)
	}
	s.mu.Unlock()

	// A broken subscriber must not break the store.
	for _, cb := range subs {
		func() {
			defer func() { _ = recover() }()
			cb(event)
		}()
	}
	_ = t
	return event, nil
}

// EventsSince returns events with seq > sinceSeq, preferring the mirror.
func (s *TaskStore) EventsSince(taskID string, sinceSeq int64) []protocol.TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if buf := s.mirror[taskID]; len(buf) > 0 && sinceSeq >= buf[0].Seq-1 {
		out := make([]protocol.TaskEvent, 0, len(buf))
		for _, e := range buf {
			if e.Seq > sinceSeq {
				out = append(out, e)
			}
		}
		return out
	}
	events, err := s.readEventsFile(taskID)
	if err != nil {
		return nil
	}
	out := make([]protocol.TaskEvent, 0, len(events))
	for _, e := range events {
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out
}

// RecentEvents returns the last n events (default 10) — mirrors the file tail.
func (s *TaskStore) RecentEvents(taskID string, n int) []protocol.TaskEvent {
	if n <= 0 {
		n = 10
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if buf := s.mirror[taskID]; len(buf) > 0 {
		if len(buf) > n {
			buf = buf[len(buf)-n:]
		}
		out := make([]protocol.TaskEvent, len(buf))
		copy(out, buf)
		return out
	}
	events, err := s.readEventsFile(taskID)
	if err != nil {
		return nil
	}
	if len(events) > n {
		events = events[len(events)-n:]
	}
	return events
}

// Subscribe registers a live-event listener; returns the unsubscribe handle.
func (s *TaskStore) Subscribe(taskID string, cb TaskListener) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handle := s.nextLn
	s.nextLn++
	set, ok := s.listeners[taskID]
	if !ok {
		set = map[int]TaskListener{}
		s.listeners[taskID] = set
	}
	set[handle] = cb
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if set, ok := s.listeners[taskID]; ok {
			delete(set, handle)
			if len(set) == 0 {
				delete(s.listeners, taskID)
			}
		}
	}
}

// SaveNow flushes state.json immediately (atomic tmp+rename).
func (s *TaskStore) SaveNow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *TaskStore) scheduleSaveLocked() {
	if s.saveTimer != nil {
		s.saveTimer.Stop()
	}
	tasks := make([]protocol.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, *t.Clone())
	}
	s.saveTimer = time.AfterFunc(saveDebounce, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = s.saveLocked()
	})
}

func (s *TaskStore) saveLocked() error {
	if s.saveTimer != nil {
		s.saveTimer.Stop()
		s.saveTimer = nil
	}
	payload := struct {
		Tasks []protocol.Task `json:"tasks"`
	}{Tasks: make([]protocol.Task, 0, len(s.tasks))}
	for _, t := range s.tasks {
		payload.Tasks = append(payload.Tasks, *t.Clone())
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tmp := s.stateFile + ".tmp." + fmt.Sprint(os.Getpid())
	if err := os.WriteFile(tmp, data, appendFilePerm); err != nil {
		return err
	}
	return os.Rename(tmp, s.stateFile)
}

func (s *TaskStore) readEventsFile(taskID string) ([]protocol.TaskEvent, error) {
	data, err := os.ReadFile(filepath.Join(s.eventsDir, taskID+".jsonl"))
	if err != nil {
		return nil, err
	}
	out := []protocol.TaskEvent{}
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var e protocol.TaskEvent
		if err := json.Unmarshal(line, &e); err == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, appendFilePerm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func splitLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			out = append(out, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}
