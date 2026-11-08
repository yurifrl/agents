package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yurifrl/agents/internal/protocol"
)

// ServerVersion is reported in health/attach responses (mirror SERVER_VERSION).
const ServerVersion = "0.2.0"

// api wires the HTTP handlers over store + supervisor.
type api struct {
	config      Config
	store       *TaskStore
	supervisor  *Supervisor
	heartbeatMs time.Duration
	bootedAt    time.Time
}

// NewHTTPHandler builds the http.Handler serving the extension API.
func NewHTTPHandler(config Config, store *TaskStore, supervisor *Supervisor, heartbeat time.Duration) http.Handler {
	a := &api{
		config:      config,
		store:       store,
		supervisor:  supervisor,
		heartbeatMs: heartbeat,
		bootedAt:    time.Now(),
	}
	if a.heartbeatMs <= 0 {
		a.heartbeatMs = 15 * time.Second
	}
	mux := http.NewServeMux()
	mux.HandleFunc(protocol.PathTasksPrefix, a.route)
	mux.HandleFunc(protocol.PathHealth, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			a.health(w)
			return
		}
		a.writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no route: %s %s", r.Method, r.URL.Path)})
	})
	mux.HandleFunc(protocol.PathAttach, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.attach(w, r)
			return
		}
		a.writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no route: %s %s", r.Method, r.URL.Path)})
	})
	mux.HandleFunc(protocol.PathTasks, func(w http.ResponseWriter, r *http.Request) {
		a.route(w, r)
	})
	return mux
}

func (a *api) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}

func (a *api) fail(w http.ResponseWriter, err error) {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		a.writeJSON(w, httpErr.Status, map[string]string{"error": httpErr.Message})
		return
	}
	a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func (a *api) route(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("%v", rec)})
		}
	}()

	path := r.URL.Path
	method := r.Method
	ctx := r.Context()

	if method == http.MethodGet && path == protocol.PathHealth {
		a.health(w)
		return
	}
	if method == http.MethodPost && path == protocol.PathAttach {
		a.attach(w, r)
		return
	}
	if method == http.MethodPost && path == protocol.PathTasks {
		a.createTask(w, r)
		return
	}
	if method == http.MethodGet && path == protocol.PathTasks {
		a.listTasks(w, r)
		return
	}
	if rest, ok := strings.CutPrefix(path, protocol.PathTasksPrefix); ok {
		id, action := rest, ""
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			id, action = rest[:i], rest[i+1:]
		}
		if action == "" && method == http.MethodGet {
			a.getTask(w, id)
			return
		}
		if action == "stream" && method == http.MethodGet {
			a.stream(w, r, id)
			return
		}
		if method == http.MethodPost {
			switch action {
			case "steer":
				a.steer(w, r, ctx, id)
				return
			case "cancel":
				a.cancelTask(w, ctx, id)
				return
			case "ack":
				a.ackTask(w, id)
				return
			}
		}
	}
	a.writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no route: %s %s", method, path)})
}

func (a *api) health(w http.ResponseWriter) {
	tasks := a.store.All()
	var body protocol.HealthResponse
	body.Ok = true
	body.Version = ServerVersion
	body.Surface = a.supervisor.Kind()
	body.UptimeMs = time.Since(a.bootedAt).Milliseconds()
	for i := range tasks {
		body.Tasks.Total++
		if protocol.IsActiveStatus(tasks[i].Status) {
			body.Tasks.Active++
		}
	}
	a.writeJSON(w, http.StatusOK, body)
}

func (a *api) readObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be valid JSON"})
		return nil, false
	}
	return body, true
}

func requiredString(body map[string]any, key string) (string, *HTTPError) {
	value, _ := body[key].(string)
	if strings.TrimSpace(value) == "" {
		return "", badRequest(fmt.Sprintf("%s must be a non-empty string", key))
	}
	return value, nil
}

func optionalString(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return value
}

func (a *api) createTask(w http.ResponseWriter, r *http.Request) {
	body, ok := a.readObject(w, r)
	if !ok {
		return
	}
	owner, httpErr := requiredString(body, "owner")
	if httpErr != nil {
		a.fail(w, httpErr)
		return
	}
	name, httpErr := requiredString(body, "name")
	if httpErr != nil {
		a.fail(w, httpErr)
		return
	}
	taskText, httpErr := requiredString(body, "task")
	if httpErr != nil {
		a.fail(w, httpErr)
		return
	}
	cwd, httpErr := requiredString(body, "cwd")
	if httpErr != nil {
		a.fail(w, httpErr)
		return
	}
	if !strings.HasPrefix(cwd, "/") {
		a.fail(w, badRequest("cwd must be an absolute path"))
		return
	}
	created := a.supervisor.Create(protocol.CreateTaskInput{
		Owner:         owner,
		Name:          name,
		Task:          taskText,
		Cwd:           cwd,
		Model:         optionalString(body, "model"),
		ParentSession: optionalString(body, "parentSession"),
	})
	a.writeJSON(w, http.StatusCreated, created)
}

func (a *api) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := TaskFilter{Owner: q.Get("owner")}
	switch q.Get("unacknowledged") {
	case "1", "true":
		filter.Unacknowledged = true
	}
	a.writeJSON(w, http.StatusOK, a.store.List(filter))
}

func (a *api) getTask(w http.ResponseWriter, id string) {
	task := a.store.Get(id)
	if task == nil {
		a.fail(w, notFound(fmt.Sprintf("task not found: %s", id)))
		return
	}
	// Contract addition: detail responses embed the last 10 events so the
	// extension can print `/agents logs <id>` without a separate endpoint.
	task.RecentEvents = a.store.RecentEvents(id, 10)
	a.writeJSON(w, http.StatusOK, task)
}

func (a *api) steer(w http.ResponseWriter, r *http.Request, ctx context.Context, id string) {
	body, ok := a.readObject(w, r)
	if !ok {
		return
	}
	message, httpErr := requiredString(body, "message")
	if httpErr != nil {
		a.fail(w, httpErr)
		return
	}
	task, err := a.supervisor.Steer(ctx, id, message)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusAccepted, task)
}

func (a *api) cancelTask(w http.ResponseWriter, ctx context.Context, id string) {
	task, err := a.supervisor.Cancel(ctx, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, task)
}

func (a *api) ackTask(w http.ResponseWriter, id string) {
	task, err := a.supervisor.Ack(id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, task)
}

func (a *api) attach(w http.ResponseWriter, r *http.Request) {
	body, ok := a.readObject(w, r)
	if !ok {
		return
	}
	owner, httpErr := requiredString(body, "owner")
	if httpErr != nil {
		a.fail(w, httpErr)
		return
	}
	a.writeJSON(w, http.StatusOK, protocol.AttachResponse{
		Tasks: a.store.List(TaskFilter{Owner: owner, Unacknowledged: true}),
		Server: protocol.ServerFlags{
			UptimeMs: time.Since(a.bootedAt).Milliseconds(),
			Surface:  a.supervisor.Kind(),
			Version:  ServerVersion,
		},
	})
}

// stream serves GET /tasks/:id/stream?since=N: subscribe-then-replay SSE with
// a heartbeat, closing on the terminal state_changed event.
func (a *api) stream(w http.ResponseWriter, r *http.Request, id string) {
	if a.store.Get(id) == nil {
		a.fail(w, notFound(fmt.Sprintf("task not found: %s", id)))
		return
	}
	since := int64(0)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			a.fail(w, badRequest("since must be a non-negative integer"))
			return
		}
		since = parsed
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		a.fail(w, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	events := make(chan protocol.TaskEvent, 1024)
	unsubscribe := a.store.Subscribe(id, func(event protocol.TaskEvent) {
		// Best-effort live delivery: the event log is the durable record; a
		// slow SSE consumer must never block the store.
		select {
		case events <- event:
		default:
		}
	})
	defer unsubscribe()

	seen := map[int64]bool{}
	forward := func(event protocol.TaskEvent) bool {
		if seen[event.Seq] {
			return true
		}
		seen[event.Seq] = true
		frame, err := json.Marshal(protocol.SseFrame{
			TaskID: event.TaskID,
			Seq:    event.Seq,
			Type:   event.Type,
			Data:   event.Data,
			Ts:     event.Ts,
		})
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
			return false
		}
		flusher.Flush()
		if event.Type == protocol.EventStateChanged && protocol.IsTerminalStatus(event.Data.To) {
			return false
		}
		return true
	}

	// Subscribe already active; replay missed events, then stream live.
	for _, event := range a.store.EventsSince(id, since) {
		if !forward(event) {
			return
		}
	}
	if current := a.store.Get(id); current != nil && protocol.IsTerminalStatus(current.Status) {
		return
	}

	heartbeat := time.NewTicker(a.heartbeatMs)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return // client disconnect: release SSE resources only, never the worker
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event := <-events:
			if !forward(event) {
				return
			}
		}
	}
}
