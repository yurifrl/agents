package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yurifrl/agents/internal/a2a"
)

// Handler returns the gateway's HTTP API:
//
//	POST /tasks/create          create a task -> {id}
//	GET  /tasks?owner=&acknowledged=false   list tasks
//	GET  /tasks/{id}            get one task
//	GET  /tasks/{id}/stream     SSE live+replay stream
//	POST /tasks/{id}/cancel     cancel a task
//	POST /tasks/{id}/ack        acknowledge a result
//	GET  /health
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/tasks/create", g.handleCreate)
	mux.HandleFunc("/tasks", g.handleList)
	mux.HandleFunc("/tasks/", g.handleTaskByID)
	return mux
}

func (g *Gateway) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	task, err := g.CreateTask(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (g *Gateway) handleList(w http.ResponseWriter, r *http.Request) {
	owner := r.URL.Query().Get("owner")
	onlyUnacked := r.URL.Query().Get("acknowledged") == "false"
	writeJSON(w, http.StatusOK, g.List(owner, onlyUnacked))
}

// handleTaskByID routes /tasks/{id}, /tasks/{id}/stream, /tasks/{id}/cancel,
// /tasks/{id}/ack.
func (g *Gateway) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/tasks/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		task, ok := g.store.Get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, task)
	case "stream":
		g.handleStream(w, r, id)
	case "cancel":
		if err := g.Cancel(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "ack":
		if err := g.Ack(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (g *Gateway) handleStream(w http.ResponseWriter, r *http.Request, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	a2a.SetSSEHeaders(w)

	events, unsub := g.Subscribe(id)
	defer unsub()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			if err := a2a.WriteSSE(w, flusher, ev); err != nil {
				return
			}
			if ev.Type == a2a.EventResult || ev.Type == a2a.EventError {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
