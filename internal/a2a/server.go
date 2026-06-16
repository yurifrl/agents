package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Runner executes an agent invocation, emitting streaming events and returning
// the terminal result. The in-pod server is otherwise agent-agnostic.
type Runner func(ctx context.Context, req InvokeRequest, emit func(Event)) (*Result, error)

// Server exposes a single agent over the A2A surface.
type Server struct {
	card AgentCard
	run  Runner
}

// NewServer builds an A2A server for one agent.
func NewServer(card AgentCard, run Runner) *Server {
	return &Server{card: card, run: run}
}

// Handler returns the HTTP routes: agent card, health, and invoke.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent-card.json", s.handleCard)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/invoke", s.handleInvoke)
	return mux
}

// ListenAndServe serves until ctx is canceled, then shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleCard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.card)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req InvokeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	SetSSEHeaders(w)

	emit := func(ev Event) { _ = WriteSSE(w, flusher, ev) }
	emit(Event{Type: EventStatus, State: "running"})

	result, err := s.run(r.Context(), req, emit)
	if err != nil {
		emit(Event{Type: EventError, Error: err.Error()})
		return
	}
	emit(Event{Type: EventResult, Result: result})
}
