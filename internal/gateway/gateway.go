// Package gateway is the stateful front door: it turns task-create requests into
// AgentTask CRDs, dispatches the prompt to the in-pod A2A server, streams the
// result back, and keeps an owner-scoped record so callers can disconnect and
// reconnect (the reconciliation requirement, design D5).
package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/yurifrl/agents/api/v1alpha1"
	"github.com/yurifrl/agents/internal/a2a"
)

// Gateway coordinates task creation, dispatch, and reconnection.
type Gateway struct {
	store     TaskStore
	k8s       client.Client
	namespace string
	http      *http.Client

	mu       sync.Mutex
	brokers  map[string]*broker
	readyTO  time.Duration
	dialHost func(taskName string) string
}

// New builds a Gateway. namespace is where AgentTasks/pods live.
func New(store TaskStore, k8s client.Client, namespace string) *Gateway {
	g := &Gateway{
		store:     store,
		k8s:       k8s,
		namespace: namespace,
		http:      &http.Client{},
		brokers:   make(map[string]*broker),
		readyTO:   2 * time.Minute,
	}
	g.dialHost = func(taskName string) string {
		// Cluster DNS for the per-task Service created by the controller.
		return fmt.Sprintf("at-%s.%s.svc:8080", taskName, g.namespace)
	}
	return g
}

// CreateRequest is the input to CreateTask.
type CreateRequest struct {
	Owner  string `json:"owner"`
	Type   string `json:"type"`
	Prompt string `json:"prompt"`
	Repo   string `json:"repo,omitempty"`
	Output string `json:"output,omitempty"`
}

// CreateTask records the task, creates its AgentTask CRD, and kicks off async
// dispatch. It returns immediately with the stored task.
func (g *Gateway) CreateTask(ctx context.Context, req CreateRequest) (*Task, error) {
	if req.Owner == "" || req.Type == "" || req.Prompt == "" {
		return nil, fmt.Errorf("owner, type and prompt are required")
	}

	id := strings.ToLower(uuid.NewString()[:8])
	now := time.Now()
	task := &Task{
		ID:        id,
		Owner:     req.Owner,
		Type:      req.Type,
		Prompt:    req.Prompt,
		Repo:      req.Repo,
		Output:    req.Output,
		State:     StatePending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := g.store.Create(task); err != nil {
		return nil, err
	}

	at := &agentsv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id,
			Namespace: g.namespace,
			Labels: map[string]string{
				"agents.yurifrl.dev/owner": req.Owner,
				"agents.yurifrl.dev/type":  req.Type,
			},
		},
		Spec: agentsv1alpha1.AgentTaskSpec{
			Type:   req.Type,
			Prompt: req.Prompt,
			Repo:   req.Repo,
			Owner:  req.Owner,
			Output: agentsv1alpha1.Output(req.Output),
		},
	}
	if err := g.k8s.Create(ctx, at); err != nil {
		return nil, fmt.Errorf("creating AgentTask: %w", err)
	}

	go g.dispatch(task)
	return task, nil
}

func (g *Gateway) brokerFor(id string) *broker {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.brokers[id]
	if !ok {
		b = newBroker()
		g.brokers[id] = b
	}
	return b
}

// dispatch waits for the pod to become reachable, invokes it, and streams the
// result into the store and the task's broker.
func (g *Gateway) dispatch(task *Task) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	b := g.brokerFor(task.ID)
	defer b.close()

	host := g.dialHost(task.ID)
	if err := g.waitReady(ctx, host); err != nil {
		g.finish(task, StateFailed, "", nil, fmt.Sprintf("agent not ready: %v", err))
		b.publish(a2a.Event{Type: a2a.EventError, Error: "agent not ready"})
		return
	}

	task.State = StateRunning
	_ = g.store.Update(task)
	b.publish(a2a.Event{Type: a2a.EventStatus, State: "running"})

	final, err := g.invoke(ctx, host, a2a.InvokeRequest{
		TaskID: task.ID,
		Prompt: task.Prompt,
		Repo:   task.Repo,
		Output: task.Output,
	}, b)
	if err != nil {
		g.finish(task, StateFailed, "", nil, err.Error())
		return
	}

	switch final.Type {
	case a2a.EventResult:
		var artifacts []string
		var out string
		if final.Result != nil {
			artifacts = final.Result.Artifacts
			out = final.Result.Output
		}
		g.finish(task, StateSucceeded, out, artifacts, "")
	case a2a.EventError:
		g.finish(task, StateFailed, "", nil, final.Error)
	default:
		g.finish(task, StateFailed, "", nil, "stream ended without a terminal event")
	}
}

func (g *Gateway) waitReady(ctx context.Context, host string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(g.readyTO)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/health", nil)
		resp, err := g.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", host)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (g *Gateway) invoke(ctx context.Context, host string, ir a2a.InvokeRequest, b *broker) (a2a.Event, error) {
	body, _ := json.Marshal(ir)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+"/invoke", strings.NewReader(string(body)))
	if err != nil {
		return a2a.Event{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := g.http.Do(req)
	if err != nil {
		return a2a.Event{}, fmt.Errorf("invoke: %w", err)
	}
	defer resp.Body.Close()

	final, err := a2a.ReadSSE(ctx, bufio.NewReader(resp.Body), b.publish)
	if err != nil {
		return final, fmt.Errorf("reading stream: %w", err)
	}
	return final, nil
}

func (g *Gateway) finish(task *Task, state State, result string, artifacts []string, errMsg string) {
	task.State = state
	task.Result = result
	task.Artifacts = artifacts
	task.Error = errMsg
	_ = g.store.Update(task)
}

// Cancel deletes the AgentTask (its pod is GC'd via owner refs) and marks the
// task canceled.
func (g *Gateway) Cancel(ctx context.Context, id string) error {
	task, ok := g.store.Get(id)
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	at := &agentsv1alpha1.AgentTask{}
	err := g.k8s.Get(ctx, types.NamespacedName{Name: id, Namespace: g.namespace}, at)
	if err == nil {
		if delErr := g.k8s.Delete(ctx, at); delErr != nil && !apierrors.IsNotFound(delErr) {
			return delErr
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	g.finish(task, StateCanceled, "", nil, "canceled")
	g.brokerFor(id).publish(a2a.Event{Type: a2a.EventError, Error: "canceled"})
	return nil
}

// Ack marks a task acknowledged so it stops appearing in unacked listings.
func (g *Gateway) Ack(id string) error {
	task, ok := g.store.Get(id)
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	task.Acknowledged = true
	return g.store.Update(task)
}

// List returns an owner's tasks, optionally only the unacknowledged ones.
func (g *Gateway) List(owner string, onlyUnacked bool) []*Task {
	return g.store.List(owner, onlyUnacked)
}

// Subscribe returns the task's live event channel (with replayed history) and an
// unsubscribe func — the basis for reconnect-to-running.
func (g *Gateway) Subscribe(id string) (<-chan a2a.Event, func()) {
	return g.brokerFor(id).subscribe()
}
