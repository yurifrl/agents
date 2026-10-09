package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yurifrl/agents/internal/surface"
)

// workerOptions configures the in-pod worker bridge.
type workerOptions struct {
	port       int
	ompBin     string
	sessionDir string
	model      string
	maxTimeS   int
	workdir    string
}

// newWorkerCmd runs the in-pod worker: it listens for the supervisor's TCP
// connection and bridges it to a live omp RPC child. The child outlives
// connections (bridge semantics), so the worker survives supervisor restarts;
// every new client receives the child's authentic `ready` frame so a fresh
// OmpRpc can complete its handshake against the same session.
func newWorkerCmd() *cobra.Command {
	opts := &workerOptions{}

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run the in-pod worker bridge (omp RPC over TCP)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorker(cmd.Context(), opts)
		},
	}

	cmd.Flags().IntVar(&opts.port, "port", envInt("REMOTE_AGENTS_WORKER_PORT", 8080), "TCP port to listen on")
	cmd.Flags().StringVar(&opts.ompBin, "omp-bin", envOr("REMOTE_AGENTS_OMP_BIN", "omp"), "worker binary")
	cmd.Flags().StringVar(&opts.sessionDir, "session-dir", envOr("REMOTE_AGENTS_SESSION_DIR", ""), "omp session dir")
	cmd.Flags().StringVar(&opts.model, "model", envOr("REMOTE_AGENTS_MODEL", ""), "worker model")
	cmd.Flags().IntVar(&opts.maxTimeS, "max-time", envInt("REMOTE_AGENTS_MAX_TIME_S", 1800), "worker wall clock (seconds)")
	cmd.Flags().StringVar(&opts.workdir, "workdir", envOr("REMOTE_AGENTS_WORKDIR", ""), "worker working directory")
	return cmd
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func runWorker(ctx context.Context, opts *workerOptions) error {
	if opts.sessionDir == "" {
		return fmt.Errorf("session dir is required (--session-dir or REMOTE_AGENTS_SESSION_DIR)")
	}

	spec := surface.WorkerSpec{
		TaskID:     os.Getenv(surface.TaskIDEnvVar),
		Cwd:        opts.workdir,
		SessionDir: opts.sessionDir,
		OmpBin:     opts.ompBin,
		Model:      opts.model,
		MaxTimeS:   opts.maxTimeS,
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", opts.port))
	if err != nil {
		return fmt.Errorf("listening on %d: %w", opts.port, err)
	}

	bridge := &workerBridge{spec: spec}
	go func() {
		<-ctx.Done()
		bridge.shutdown(0)
	}()

	fmt.Printf("worker bridge listening on %d (worker=%s)\n", opts.port, spec.OmpBin)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return bridge.exitError()
		}
		bridge.serve(conn)
	}
}

// workerBridge owns the omp child and fans its stdout out to connected
// supervisors. Concurrency model: one writer mutex per client set, one stdout
// pump goroutine, connects/disconnects handled inline from the accept loop.
type workerBridge struct {
	spec surface.WorkerSpec

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	clients map[net.Conn]chan struct{}
	ready   []byte // captured authentic ready frame (nil until seen)
	exited  bool
	exit    int
	done    chan struct{} // closed when the child exits

	closing bool
}

// child lazily spawns omp on the first client and returns its stdin.
func (b *workerBridge) child() (io.WriteCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd != nil {
		if b.exited {
			return nil, fmt.Errorf("worker exited with code %d", b.exit)
		}
		return b.stdin, nil
	}

	argv := surface.WorkerArgv(b.spec)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = b.spec.Cwd
	cmd.Env = append(os.Environ(), surface.WorkerEnvVar+"=1")
	if b.spec.TaskID != "" {
		cmd.Env = append(cmd.Env, surface.TaskIDEnvVar+"="+b.spec.TaskID)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b.cmd = cmd
	b.stdin = stdin
	b.clients = map[net.Conn]chan struct{}{}
	b.done = make(chan struct{})
	go b.pump(stdout)
	go b.awaitExit()
	return stdin, nil
}

// pump relays child stdout to every client while scanning for the ready frame.
func (b *workerBridge) pump(stdout io.Reader) {
	reader := bufio.NewReaderSize(stdout, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			b.noteReady(chunk)
			b.broadcast(chunk)
		}
		if err != nil {
			return
		}
	}
}

// noteReady captures the first authentic `ready` frame from the child's stdout.
func (b *workerBridge) noteReady(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ready != nil {
		return
	}
	for _, line := range strings.Split(string(chunk), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var frame struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && frame.Type == "ready" {
			b.ready = []byte(line + "\n")
			return
		}
	}
}

// broadcast fans a child stdout chunk out to all live clients.
func (b *workerBridge) broadcast(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for conn, done := range b.clients {
		select {
		case <-done:
			continue
		default:
		}
		if _, err := conn.Write(chunk); err != nil {
			b.removeLocked(conn)
		}
	}
}

// serve wires one supervisor connection to the child until it drops.
func (b *workerBridge) serve(conn net.Conn) {
	stdin, err := b.child()
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		_ = conn.Close()
		if b.isExited() {
			b.shutdown(1)
		}
		return
	}

	// New client: replay the handshake so OmpRpc.start() completes.
	b.mu.Lock()
	if b.ready != nil {
		if _, err := conn.Write(b.ready); err != nil {
			b.mu.Unlock()
			_ = conn.Close()
			return
		}
	}
	done := make(chan struct{})
	b.clients[conn] = done
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.removeLocked(conn)
		b.mu.Unlock()
		_ = conn.Close()
	}()

	io.Copy(stdin, conn) //nolint:errcheck — client drops are expected; the child outlives them
	close(done)
}

// removeLocked drops a closed client. Caller holds b.mu.
func (b *workerBridge) removeLocked(conn net.Conn) {
	if _, ok := b.clients[conn]; ok {
		delete(b.clients, conn)
		_ = conn.Close()
	}
}

// awaitExit records the child's exit and tears the bridge down.
func (b *workerBridge) awaitExit() {
	err := b.cmd.Wait()
	b.mu.Lock()
	b.exited = true
	b.exit = 1
	if err == nil {
		b.exit = 0
	} else if ee, ok := err.(*exec.ExitError); ok {
		b.exit = ee.ExitCode()
	}
	b.mu.Unlock()
	close(b.done)
	b.shutdown(b.exitCode())
}

func (b *workerBridge) isExited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exited
}

func (b *workerBridge) exitCode() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exit
}

func (b *workerBridge) exitError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exited && b.exit != 0 {
		return fmt.Errorf("worker exited with code %d", b.exit)
	}
	return nil
}

// shutdown terminates the child (SIGTERM, 5s, SIGKILL), closes all clients,
// and exits. Idempotent; only the first caller's exit code wins.
func (b *workerBridge) shutdown(exitCode int) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.closing = true
	clients := b.clients
	b.clients = nil
	cmd := b.cmd
	childDone := b.done
	b.mu.Unlock()

	if cmd != nil && cmd.Process != nil && childDone != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-childDone:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-childDone
		}
	}
	for conn := range clients {
		_ = conn.Close()
	}
	os.Exit(exitCode)
}
