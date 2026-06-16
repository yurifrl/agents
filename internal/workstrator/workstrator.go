// Package workstrator is the beads front: it polls the Dolt beads server for
// ready beads, claims them, dispatches each to the gateway as an ordinary
// client, and closes the bead when the resulting task completes. The system
// knows nothing about it — it is just another gateway client (design D10).
package workstrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Config controls the workstrator loop.
type Config struct {
	GatewayURL   string
	Owner        string
	AgentType    string
	DefaultRepo  string
	RepoByPrefix map[string]string
	PollInterval time.Duration
}

// Workstrator polls beads and dispatches them.
type Workstrator struct {
	cfg   Config
	bd    *bd
	log   logr.Logger
	http  *http.Client
	mu    sync.Mutex
	inFlt map[string]bool
}

// New builds a Workstrator.
func New(cfg Config, log logr.Logger) *Workstrator {
	if cfg.AgentType == "" {
		cfg.AgentType = "worker"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 15 * time.Second
	}
	return &Workstrator{
		cfg:   cfg,
		bd:    newBD(),
		log:   log.WithName("workstrator"),
		http:  &http.Client{},
		inFlt: make(map[string]bool),
	}
}

// Run polls until the context is canceled.
func (w *Workstrator) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	w.log.Info("workstrator started", "gateway", w.cfg.GatewayURL, "type", w.cfg.AgentType, "interval", w.cfg.PollInterval)
	for {
		if err := w.poll(ctx); err != nil {
			w.log.Error(err, "poll failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Workstrator) poll(ctx context.Context) error {
	beads, err := w.bd.ClaimReady(ctx)
	if err != nil {
		return err
	}
	for _, b := range beads {
		if w.markInFlight(b.ID) {
			continue
		}
		go w.handle(ctx, b)
	}
	return nil
}

// markInFlight returns true if the bead is already being handled.
func (w *Workstrator) markInFlight(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlt[id] {
		return true
	}
	w.inFlt[id] = true
	return false
}

func (w *Workstrator) clearInFlight(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.inFlt, id)
}

func (w *Workstrator) handle(ctx context.Context, b Bead) {
	defer w.clearInFlight(b.ID)
	log := w.log.WithValues("bead", b.ID)

	repo := w.repoFor(b.ID)
	prompt := strings.TrimSpace(b.Title + "\n\n" + b.Description)

	taskID, err := w.dispatch(ctx, prompt, repo)
	if err != nil {
		log.Error(err, "dispatch failed")
		_ = w.bd.Comment(ctx, b.ID, "agent dispatch failed: "+err.Error())
		_ = w.bd.Unclaim(ctx, b.ID)
		return
	}
	log.Info("dispatched", "task", taskID, "repo", repo)

	result, err := w.waitResult(ctx, taskID)
	if err != nil {
		log.Error(err, "task failed")
		_ = w.bd.Comment(ctx, b.ID, fmt.Sprintf("agent task %s failed: %v", taskID, err))
		_ = w.bd.Unclaim(ctx, b.ID)
		return
	}

	_ = w.bd.Comment(ctx, b.ID, "agent completed: "+result)
	if err := w.bd.Close(ctx, b.ID, "Completed by remote agent: "+result); err != nil {
		log.Error(err, "close failed")
	}
}

func (w *Workstrator) repoFor(id string) string {
	if w.cfg.RepoByPrefix != nil {
		if i := strings.IndexByte(id, '-'); i > 0 {
			if repo, ok := w.cfg.RepoByPrefix[id[:i]]; ok {
				return repo
			}
		}
	}
	return w.cfg.DefaultRepo
}

// dispatch creates a task on the gateway and returns its id.
func (w *Workstrator) dispatch(ctx context.Context, prompt, repo string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"owner":  w.cfg.Owner,
		"type":   w.cfg.AgentType,
		"prompt": prompt,
		"repo":   repo,
		"output": "pr",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.GatewayURL+"/tasks/create", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("gateway create -> %d: %s", resp.StatusCode, string(raw))
	}
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &task); err != nil {
		return "", err
	}
	return task.ID, nil
}

// waitResult polls the task until it reaches a terminal state.
func (w *Workstrator) waitResult(ctx context.Context, taskID string) (string, error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.GatewayURL+"/tasks/"+taskID, nil)
		resp, err := w.http.Do(req)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var task struct {
			State  string `json:"state"`
			Result string `json:"result"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(raw, &task); err != nil {
			continue
		}
		switch task.State {
		case "succeeded":
			return task.Result, nil
		case "failed", "canceled":
			return "", fmt.Errorf("%s: %s", task.State, task.Error)
		}
	}
}
