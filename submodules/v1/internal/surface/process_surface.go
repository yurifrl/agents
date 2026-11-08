package surface

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/yurifrl/agents/internal/protocol"
)

// ProcessSurface runs the worker as a direct child of this process (piped
// stdio). The worker dies with the server — re-adoption marks such tasks
// failed with a resume hint (deliberate v1 limit, CONTRACT "known limits").
type ProcessSurface struct{}

// NewProcessSurface builds the process surface.
func NewProcessSurface() *ProcessSurface { return &ProcessSurface{} }

// Kind reports the surface kind.
func (s *ProcessSurface) Kind() protocol.SurfaceKind { return protocol.SurfaceProcess }

// Start launches the worker child and returns its piped channel.
func (s *ProcessSurface) Start(spec WorkerSpec) (RunningWorker, error) {
	argv := WorkerArgv(spec)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.Cwd
	cmd.Env = append(os.Environ(), WorkerEnvVar+"=1", TaskIDEnvVar+"="+spec.TaskID)
	cmd.Stderr = io.Discard // omp errors surface through the RPC handshake path

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return RunningWorker{}, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunningWorker{}, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return RunningWorker{}, fmt.Errorf("start worker: %w", err)
	}

	w := &processWorker{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	pid := cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
		w.mu.Lock()
		w.exitCode = cmd.ProcessState.ExitCode()
		w.exited = true
		w.mu.Unlock()
		close(w.done)
	}()
	return RunningWorker{
		Channel: &processChannel{stdin: stdin, stdout: stdout, worker: w},
		Meta:    protocol.TaskWorkerMeta{PID: new(pid)},
		Stop:    w.Stop,
	}, nil
}

// processWorker tracks the child lifecycle and owns the stop sequence.
type processWorker struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}

	mu       sync.Mutex
	exited   bool
	exitCode int

	stopMu  sync.Mutex
	stopped bool
}

// ExitCode reports the child's exit code once it has exited.
func (w *processWorker) ExitCode() (int, bool) {
	select {
	case <-w.done:
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.exitCode, true
	default:
		return 0, false
	}
}

// Stop terminates the worker: SIGTERM, up to 3s for exit, then SIGKILL.
// Idempotent.
func (w *processWorker) Stop() {
	w.stopMu.Lock()
	defer w.stopMu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-w.done:
	case <-time.After(termGrace * time.Second):
		_ = w.cmd.Process.Kill()
		<-w.done
	}
}

// processChannel adapts the child's piped stdio to the Channel seam and
// exposes the exit code once the child is gone.
type processChannel struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	worker *processWorker
}

func (c *processChannel) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *processChannel) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// ExitCode satisfies surface.ExitCoder (supervisor uses it in fail messages).
func (c *processChannel) ExitCode() (int, bool) { return c.worker.ExitCode() }

func (c *processChannel) Close() error {
	// Closing stdin signals the worker; the supervisor's Stop is the hammer.
	return c.stdin.Close()
}
