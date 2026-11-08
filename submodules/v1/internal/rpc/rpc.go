// Package rpc is the Go port of remote-agents/packages/rpc-driver: a client
// for omp's line-delimited JSON RPC mode (CONTRACT.md §2). The first frame
// must be {"type":"ready"}; every later frame is either a response correlated
// by numeric id or an event forwarded verbatim to the event handler.
package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Error codes (mirror OmpRpcErrorCode in the TS driver).
const (
	CodeHandshakeTimeout = "handshake_timeout"
	CodeRequestTimeout   = "request_timeout"
	CodeRequestFailed    = "request_failed"
	CodeChannelClosed    = "channel_closed"
)

// Error is the typed rpc failure.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// ReadyInfo is the frame omp emits as its very first stdout line. An empty
// SessionFile means the frame carried none (observed on some omp builds).
type ReadyInfo struct {
	ProtocolVersion int
	SessionFile     string
}

// Event is any non-response JSON frame received from the worker.
type Event map[string]any

// Response is a successful response frame correlated with a pending request id.
type Response struct {
	ID  int
	Raw map[string]any
}

// String returns Raw[key] as a string, or "" when absent/non-string.
func (r Response) String(key string) string {
	s, _ := r.Raw[key].(string)
	return s
}

// Bool returns Raw[key] strictly as a bool, and whether it was present.
func (r Response) Bool(key string) (bool, bool) {
	b, ok := r.Raw[key].(bool)
	return b, ok
}

const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultRequestTimeout   = 30 * time.Second
	// maxLine caps one JSON frame; the TS driver tolerates oversized garbage
	// by resetting its scan buffer, the Scanner equivalent is dropping the token.
	maxLine = 8 << 20
)

type pendingReq struct {
	resp chan Response
	err  chan *Error
}

// RPC talks to one worker over a line-delimited JSON channel. Safe for
// concurrent use; the read pump runs until Close or channel EOF.
type RPC struct {
	rwc              io.ReadWriteCloser
	handshakeTimeout time.Duration
	requestTimeout   time.Duration

	writeMu sync.Mutex

	mu        sync.Mutex
	pending   map[int]*pendingReq
	nextID    int
	started   bool
	handshook bool
	ready     ReadyInfo
	finished  bool
	closeErr  *Error
	readyDone chan struct{}
	closed    chan struct{}
	closeFn   []func()
	eventFn   func(Event)
}

// Option configures an RPC.
type Option func(*RPC)

// WithHandshakeTimeout overrides the ready-frame wait (default 10s).
func WithHandshakeTimeout(d time.Duration) Option { return func(r *RPC) { r.handshakeTimeout = d } }

// WithRequestTimeout overrides the default per-request timeout (default 30s).
func WithRequestTimeout(d time.Duration) Option { return func(r *RPC) { r.requestTimeout = d } }

// New starts consuming rwc immediately.
func New(rwc io.ReadWriteCloser, opts ...Option) *RPC {
	r := &RPC{
		rwc:              rwc,
		handshakeTimeout: defaultHandshakeTimeout,
		requestTimeout:   defaultRequestTimeout,
		pending:          map[int]*pendingReq{},
		nextID:           1,
		readyDone:        make(chan struct{}),
		closed:           make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	go r.pump()
	return r
}

// SetEventHandler registers the single event listener (replaces any prior).
func (r *RPC) SetEventHandler(fn func(Event)) {
	r.mu.Lock()
	r.eventFn = fn
	r.mu.Unlock()
}

// OnClose registers a channel-closed listener; listeners fire exactly once.
func (r *RPC) OnClose(fn func()) {
	r.mu.Lock()
	r.closeFn = append(r.closeFn, fn)
	r.mu.Unlock()
}

// Closed reports whether the channel has finished (EOF, error, or Close).
func (r *RPC) Closed() <-chan struct{} { return r.closed }

// Start waits for the worker's ready frame. Call exactly once. On timeout the
// channel is closed and a *Error with CodeHandshakeTimeout returned.
func (r *RPC) Start(ctx context.Context) (ReadyInfo, error) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return ReadyInfo{}, fmt.Errorf("rpc: Start must be called exactly once")
	}
	r.started = true
	done, info := r.handshook, r.ready
	r.mu.Unlock()
	if done {
		return info, nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.handshakeTimeout)
	defer cancel()
	select {
	case <-r.readyDone:
		if !r.handshakeCompleted() {
			return ReadyInfo{}, r.closureError("handshake")
		}
		return r.readySnapshot(), nil
	case <-r.closed:
		return ReadyInfo{}, r.closureError("handshake")
	case <-ctx.Done():
		err := &Error{Code: CodeHandshakeTimeout, Message: fmt.Sprintf("no ready frame within %s", r.handshakeTimeout)}
		r.finish(err)
		return ReadyInfo{}, err
	}
}

// handshakeCompleted reports whether a ready frame completed the handshake.
func (r *RPC) handshakeCompleted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handshook
}

func (r *RPC) readySnapshot() ReadyInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready
}

// Request sends a method frame and awaits the response with the same id.
// Returns CodeRequestFailed on success:false, CodeRequestTimeout on timeout,
// CodeChannelClosed when the channel dies first. timeout <= 0 disables it.
func (r *RPC) Request(ctx context.Context, frame map[string]any, timeout time.Duration) (Response, error) {
	typ, _ := frame["type"].(string)
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return Response{}, r.closureError(fmt.Sprintf("request %s", typ))
	}
	id := r.nextID
	r.nextID++
	p := &pendingReq{resp: make(chan Response, 1), err: make(chan *Error, 1)}
	r.pending[id] = p
	r.mu.Unlock()

	// Correlate: the wire frame carries the id (TS driver sends {id, ...method}).
	out := make(map[string]any, len(frame)+1)
	for key, value := range frame {
		out[key] = value
	}
	out["id"] = id
	data, err := json.Marshal(out)
	if err == nil {
		_, err = r.writeLine(append(data, '\n'))
	}
	if err != nil {
		r.removePending(id)
		return Response{}, &Error{Code: CodeChannelClosed, Message: fmt.Sprintf("write failed for %s: %v", typ, err)}
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	select {
	case resp := <-p.resp:
		return resp, nil
	case rpcErr := <-p.err:
		return Response{}, rpcErr
	case <-ctx.Done():
		if r.removePending(id) {
			return Response{}, &Error{Code: CodeRequestTimeout, Message: fmt.Sprintf("request %s (id %d) timed out after %s", typ, id, timeout)}
		}
		// The response raced the timeout and already arrived; drain it.
		return r.drain(p, typ)
	case <-r.closed:
		// finish() delivered the closure error into p.err; but a real
		// response may have raced the close and won.
		select {
		case resp := <-p.resp:
			return resp, nil
		default:
		}
		return Response{}, r.closureError(fmt.Sprintf("request %s", typ))
	}
}

func (r *RPC) drain(p *pendingReq, typ string) (Response, error) {
	select {
	case resp := <-p.resp:
		return resp, nil
	case rpcErr := <-p.err:
		return Response{}, rpcErr
	case <-time.After(time.Second):
		return Response{}, r.closureError(fmt.Sprintf("request %s", typ))
	}
}

func (r *RPC) removePending(id int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pending[id]; !ok {
		return false
	}
	delete(r.pending, id)
	return true
}

// Prompt starts a worker turn.
func (r *RPC) Prompt(ctx context.Context, message string) (Response, error) {
	return r.requestDefault(ctx, map[string]any{"type": "prompt", "message": message})
}

// Steer sends a mid-turn follow-up.
func (r *RPC) Steer(ctx context.Context, message string) (Response, error) {
	return r.requestDefault(ctx, map[string]any{"type": "steer", "message": message})
}

// Abort requests a graceful stop.
func (r *RPC) Abort(ctx context.Context) (Response, error) {
	return r.requestDefault(ctx, map[string]any{"type": "abort"})
}

// GetState asks the worker for its streaming state.
func (r *RPC) GetState(ctx context.Context) (Response, error) {
	return r.requestDefault(ctx, map[string]any{"type": "get_state"})
}

// GetLastAssistantText fetches the worker's last assistant text.
func (r *RPC) GetLastAssistantText(ctx context.Context) (Response, error) {
	return r.requestDefault(ctx, map[string]any{"type": "get_last_assistant_text"})
}

// SetSessionName labels the worker session.
func (r *RPC) SetSessionName(ctx context.Context, name string) (Response, error) {
	return r.requestDefault(ctx, map[string]any{"type": "set_session_name", "name": name})
}

func (r *RPC) requestDefault(ctx context.Context, frame map[string]any) (Response, error) {
	return r.Request(ctx, frame, r.requestTimeout)
}

// Close finishes the RPC and closes the underlying channel. Idempotent.
func (r *RPC) Close() error {
	r.finish(&Error{Code: CodeChannelClosed, Message: "rpc.Close() called"})
	return r.rwc.Close()
}

func (r *RPC) writeLine(line []byte) (int, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return r.rwc.Write(line)
}

func (r *RPC) pump() {
	scanner := bufio.NewScanner(r.rwc)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	for scanner.Scan() {
		r.handleLine(scanner.Bytes())
	}
	r.finish(&Error{Code: CodeChannelClosed, Message: "channel closed"})
}

func (r *RPC) handleLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	var frame map[string]any
	if err := json.Unmarshal(trimmed, &frame); err != nil {
		return // Not JSON — tolerate and ignore.
	}

	typ, _ := frame["type"].(string)

	r.mu.Lock()
	if !r.handshook {
		if typ == "ready" {
			info := ReadyInfo{}
			if v, ok := frame["protocolVersion"].(float64); ok {
				info.ProtocolVersion = int(v)
			}
			if s, ok := frame["sessionFile"].(string); ok {
				info.SessionFile = s
			}
			r.ready = info
			r.handshook = true
			close(r.readyDone)
		}
		// Pre-ready frames (extension UI chatter, late events) are ignored.
		r.mu.Unlock()
		return
	}
	if typ == "response" {
		if idf, ok := frame["id"].(float64); ok {
			id := int(idf)
			if p, ok := r.pending[id]; ok {
				delete(r.pending, id)
				r.mu.Unlock()
				if frame["success"] == false {
					msg, _ := frame["error"].(string)
					if msg == "" {
						msg = fmt.Sprintf("request %d failed", id)
					}
					p.err <- &Error{Code: CodeRequestFailed, Message: msg}
				} else {
					p.resp <- Response{ID: id, Raw: frame}
				}
				return
			}
		}
		r.mu.Unlock()
		return
	}
	fn := r.eventFn
	r.mu.Unlock()

	if typ != "" && fn != nil {
		// Listener bugs must never kill the read pump.
		defer func() { _ = recover() }()
		fn(Event(frame))
	}
}

func (r *RPC) finish(err *Error) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.closeErr = err
	if !r.handshook {
		select {
		case <-r.readyDone:
		default:
			close(r.readyDone)
		}
	}
	fns := r.closeFn
	r.closeFn = nil
	r.mu.Unlock()
	close(r.closed)
	for _, fn := range fns {
		func() {
			defer func() { _ = recover() }()
			fn()
		}()
	}
}

// closureError returns the recorded close error, or a synthesized one.
func (r *RPC) closureError(context string) *Error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closeErr != nil {
		return r.closeErr
	}
	return &Error{Code: CodeChannelClosed, Message: fmt.Sprintf("channel closed before %s", context)}
}
