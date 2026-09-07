import type { Channel, RunningWorker, Surface, SurfaceKind, WorkerSpec } from "@remote-agents/protocol";

export const WORKER_ENV_VAR = "REMOTE_AGENTS_WORKER";
export const TASK_ID_ENV_VAR = "REMOTE_AGENTS_TASK_ID";
const TERM_GRACE_MS = 3_000;

/** ompBin pointing at a script is run through the current bun binary. */
const SCRIPT_EXTENSION = /\.(?:[cm]?[jt]s)$/;

/**
 * The argv every surface launches the worker with (CONTRACT.md §3):
 * `omp --mode rpc --session-dir <dir> --no-pty --auto-approve --max-time <s> [--model <m>]`.
 * An ompBin ending in .ts/.js/.mjs/.cjs/.mts is prefixed with the current bun
 * executable so test fixtures (fake-omp.ts) work without a wrapper script.
 */
export function workerArgv(spec: WorkerSpec): string[] {
  const bin = SCRIPT_EXTENSION.test(spec.ompBin)
    ? [process.execPath, spec.ompBin]
    : [spec.ompBin];
  const args = [
    ...bin,
    "--mode",
    "rpc",
    "--session-dir",
    spec.sessionDir,
    "--no-pty",
    "--auto-approve",
    "--max-time",
    String(spec.maxTimeS),
  ];
  if (spec.model) args.push("--model", spec.model);
  return args;
}

/** Structural view of the Bun subprocess bits the surface touches. */
interface WorkerProcess {
  readonly pid: number;
  readonly exited: Promise<number>;
  readonly exitCode: number | null;
  readonly stdout: ReadableStream<Uint8Array>;
  readonly stderr: ReadableStream<Uint8Array>;
  readonly stdin: {
    write(data: Uint8Array): unknown;
    flush(): unknown;
    end(): unknown;
  };
  kill(signal?: number | "SIGTERM" | "SIGKILL"): unknown;
}

/**
 * Channel over a piped subprocess: reads come from stdout, writes go through
 * the FileSink stdin (flushed immediately so frames are not buffered).
 */
export function processChannel(proc: WorkerProcess): Channel {
  return {
    readable: proc.stdout,
    writable: new WritableStream<Uint8Array>({
      write(chunk) {
        proc.stdin.write(chunk);
        proc.stdin.flush();
      },
      close() {
        proc.stdin.end();
      },
      abort() {
        proc.stdin.end();
      },
    }),
    close: async () => {
      try {
        proc.stdin.end();
      } catch {
        // Process already gone.
      }
    },
  };
}

/**
 * Run the worker as a direct child of this process (piped stdio). The worker
 * dies with the server — re-adoption marks such tasks failed with a resume
 * hint (deliberate v1 limit, CONTRACT.md "Known deliberate v1 limits").
 */
export class ProcessSurface implements Surface {
  readonly kind: SurfaceKind = "process";

  async start(spec: WorkerSpec): Promise<RunningWorker> {
    const proc: WorkerProcess = Bun.spawn(workerArgv(spec), {
      cwd: spec.cwd,
      env: {
        ...process.env,
        [WORKER_ENV_VAR]: "1",
        [TASK_ID_ENV_VAR]: spec.taskId,
      },
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
    });
    // Drain stderr so a chatty worker can never fill the pipe and stall.
    void new Response(proc.stderr).text();
    return {
      channel: processChannel(proc),
      meta: { pid: proc.pid },
      stop: () => stopProcess(proc),
    };
  }
}

/** SIGTERM, wait up to 3s for exit, then SIGKILL. Idempotent. */
export async function stopProcess(proc: WorkerProcess): Promise<void> {
  if (proc.exitCode !== null) return;
  try {
    proc.kill("SIGTERM");
  } catch {
    return; // Already exited between the check and the signal.
  }
  const exitedInTime = await Promise.race([
    proc.exited.then(() => true),
    new Promise<boolean>((resolve) => setTimeout(() => resolve(false), TERM_GRACE_MS)),
  ]);
  if (!exitedInTime && proc.exitCode === null) {
    try {
      proc.kill("SIGKILL");
    } catch {
      // Already gone.
    }
  }
  await proc.exited.catch(() => {});
}
