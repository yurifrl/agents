// Package surface hosts workers: every surface reduces a launch to an
// io.ReadWriteCloser channel (the Go seam for the TS protocol's Channel) plus
// metadata and a stop function.
package surface

import (
	"io"

	"github.com/yurifrl/agents/internal/protocol"
)

// Channel is the byte-transport to a worker's stdio (io.ReadWriteCloser).
type Channel = io.ReadWriteCloser

// WorkerSpec is everything a surface needs to launch one worker.
type WorkerSpec struct {
	TaskID string
	Name   string
	Cwd    string
	// SessionDir holds session files, the bridge script, and the port file.
	SessionDir string
	OmpBin     string
	Model      string
	// MaxTimeS is the hard wall clock for the worker process (--max-time).
	MaxTimeS int
}

// ExitCoder is implemented by workers that can report a process exit code.
type ExitCoder interface {
	ExitCode() (int, bool)
}

// RunningWorker is a started worker.
type RunningWorker struct {
	Channel Channel
	Meta    protocol.TaskWorkerMeta
	// Stop tears the worker down; must be idempotent.
	Stop func()
}

// Surface launches workers of one kind.
type Surface interface {
	Kind() protocol.SurfaceKind
	Start(spec WorkerSpec) (RunningWorker, error)
}
