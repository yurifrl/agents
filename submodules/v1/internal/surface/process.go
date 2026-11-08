package surface

import (
	"regexp"
	"strconv"
	"strings"
)

// WorkerEnvVar marks the worker process (pi-herdr-subagents convention).
const WorkerEnvVar = "REMOTE_AGENTS_WORKER"

// TaskIDEnvVar carries the owning task id into the worker environment.
const TaskIDEnvVar = "REMOTE_AGENTS_TASK_ID"

const termGrace = 3 // seconds between SIGTERM and SIGKILL

// scriptExtension matches ompBin values that must run through bun.
var scriptExtension = regexp.MustCompile(`\.(?:[cm]?[jt]s)$`)

// BunBin is the bun executable used to run script ompBin values (TS fixtures
// like fake-omp.ts). Overridable for tests.
var BunBin = "bun"

// WorkerArgv is the argv every surface launches the worker with (CONTRACT §3):
// `omp --mode rpc --session-dir <dir> --no-pty --auto-approve --max-time <s>
// [--model <m>]`. An ompBin ending in .ts/.js/.mjs/.cjs/.mts is prefixed with
// the bun executable so test fixtures work without a wrapper script.
func WorkerArgv(spec WorkerSpec) []string {
	var argv []string
	if scriptExtension.MatchString(spec.OmpBin) {
		argv = append(argv, BunBin, spec.OmpBin)
	} else {
		argv = append(argv, spec.OmpBin)
	}
	argv = append(argv,
		"--mode", "rpc",
		"--session-dir", spec.SessionDir,
		"--no-pty",
		"--auto-approve",
		"--max-time", strconv.Itoa(spec.MaxTimeS),
	)
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	return argv
}

// IsScriptOmpBin reports whether the ompBin needs a bun prefix.
func IsScriptOmpBin(ompBin string) bool {
	return scriptExtension.MatchString(ompBin) || strings.HasSuffix(ompBin, ".ts")
}
