package gateway

import (
	"sync"
	"time"
)

// State is the lifecycle state of a gateway task.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

// Task is the gateway's owner-scoped record of a unit of work. It is the durable
// reconciliation key: listing and re-attaching are by owner, not by session.
type Task struct {
	ID           string    `json:"id"`
	Owner        string    `json:"owner"`
	Type         string    `json:"type"`
	Prompt       string    `json:"prompt"`
	Repo         string    `json:"repo,omitempty"`
	Output       string    `json:"output,omitempty"`
	State        State     `json:"state"`
	Result       string    `json:"result,omitempty"`
	Artifacts    []string  `json:"artifacts,omitempty"`
	Error        string    `json:"error,omitempty"`
	Acknowledged bool      `json:"acknowledged"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// TaskStore persists tasks. The MVP uses an in-memory implementation; a durable
// SQL (Dolt) implementation is the documented follow-up for D5 — until then,
// reconciliation state does not survive a gateway restart.
type TaskStore interface {
	Create(t *Task) error
	Get(id string) (*Task, bool)
	List(owner string, onlyUnacked bool) []*Task
	Update(t *Task) error
	Delete(id string) error
}

// MemoryStore is an in-process TaskStore.
type MemoryStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

// NewMemoryStore builds an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{tasks: make(map[string]*Task)}
}

func (s *MemoryStore) Create(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	s.tasks[t.ID] = &cp
	return nil
}

func (s *MemoryStore) Get(id string) (*Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

func (s *MemoryStore) List(owner string, onlyUnacked bool) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Task
	for _, t := range s.tasks {
		if owner != "" && t.Owner != owner {
			continue
		}
		if onlyUnacked && t.Acknowledged {
			continue
		}
		cp := *t
		out = append(out, &cp)
	}
	return out
}

func (s *MemoryStore) Update(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.UpdatedAt = time.Now()
	cp := *t
	s.tasks[t.ID] = &cp
	return nil
}

func (s *MemoryStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
	return nil
}
