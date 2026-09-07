import type {
  Channel,
  RunningWorker,
  Surface,
  SurfaceKind,
  WorkerSpec,
} from "@remote-agents/protocol";
import { mkdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { tcpChannel } from "../src/channel";
import { waitUntil } from "./helpers";

interface SpawnedProc {
  pid: number;
  kill(exitCode?: number | NodeJS.Signals): void;
  exited: Promise<number>;
  readonly exitCode: number | null;
}

export interface FakeSurfaceOptions {
  fakePath: string;
  extraArgs?: string[];
  kind?: SurfaceKind;
  /**
   * stop() kills the worker process when true (default). The adoption test
   * passes false so the TCP worker survives the abandoned first supervisor.
   */
  stopKillsProc?: boolean;
  /** Test hook: delay before start() spawns, holding tasks in "queued". */
  startDelayMs?: number;
}

/**
 * In-package test surface: spawns the fake-omp worker via Bun.spawn (stdio
 * mode, kind "process") or over TCP (kind "herdr", emulating the bridge).
 * Lives in test code only — production uses @remote-agents/surfaces.
 */
export class FakeSurface implements Surface {
  readonly kind: SurfaceKind;
  readonly procs: SpawnedProc[] = [];
  private readonly opts: Required<Pick<FakeSurfaceOptions, "fakePath" | "stopKillsProc">> & FakeSurfaceOptions;

  constructor(opts: FakeSurfaceOptions) {
    this.opts = { stopKillsProc: true, ...opts };
    this.kind = opts.kind ?? "process";
  }

  async start(spec: WorkerSpec): Promise<RunningWorker> {
    const delay = this.opts.startDelayMs ?? 0;
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    if (this.kind === "herdr") return this.startTcp(spec);
    return this.startStdio(spec);
  }

  private workerEnv(spec: WorkerSpec): Record<string, string | undefined> {
    return {
      ...process.env,
      REMOTE_AGENTS_WORKER: "1",
      REMOTE_AGENTS_TASK_ID: spec.taskId,
    };
  }

  private async startStdio(spec: WorkerSpec): Promise<RunningWorker> {
    const proc = Bun.spawn(
      [
        "bun",
        this.opts.fakePath,
        "--session-dir",
        spec.sessionDir,
        ...(spec.model ? ["--model", spec.model] : []),
        ...(this.opts.extraArgs ?? []),
      ],
      {
        stdin: "pipe",
        stdout: "pipe",
        stderr: "pipe",
        cwd: spec.cwd,
        env: this.workerEnv(spec),
      },
    );
    this.procs.push(proc);
    const stdin = proc.stdin;
    const channel: Channel = {
      readable: proc.stdout as ReadableStream<Uint8Array>,
      writable: new WritableStream<Uint8Array>({
        write(chunk) {
          stdin.write(chunk);
          stdin.flush();
        },
        close() {
          stdin.end();
        },
        abort() {
          stdin.end();
        },
      }),
      close: async () => {
        stdin.end();
      },
    };
    return {
      channel,
      meta: { pid: proc.pid },
      stop: async () => {
        await this.stopProc(proc);
      },
    };
  }

  private async startTcp(spec: WorkerSpec): Promise<RunningWorker> {
    mkdirSync(spec.sessionDir, { recursive: true });
    const portFile = path.join(spec.sessionDir, "port");
    const proc = Bun.spawn(
      [
        "bun",
        this.opts.fakePath,
        "--tcp",
        "--port-file",
        portFile,
        "--session-dir",
        spec.sessionDir,
        ...(this.opts.extraArgs ?? []),
      ],
      {
        stdin: "pipe",
        stdout: "pipe",
        stderr: "pipe",
        cwd: spec.cwd,
        env: this.workerEnv(spec),
      },
    );
    this.procs.push(proc);
    const port = await waitUntil(() => readPortFile(portFile), 10_000);
    const channel = await tcpChannel("127.0.0.1", port);
    return {
      channel,
      meta: { port },
      stop: async () => {
        await channel.close();
        await this.stopProc(proc);
      },
    };
  }

  private async stopProc(proc: SpawnedProc): Promise<void> {
    if (this.opts.stopKillsProc === false) return;
    if (proc.exitCode !== null) return;
    proc.kill("SIGTERM");
    const killer = setTimeout(() => {
      try {
        proc.kill("SIGKILL");
      } catch {
        // already dead
      }
    }, 2000);
    killer.unref?.();
    await proc.exited;
    clearTimeout(killer);
  }

  /** Hard-kill every worker process this surface spawned (test teardown). */
  async killAll(): Promise<void> {
    const procs = this.procs.splice(0, this.procs.length);
    await Promise.all(
      procs.map(async (proc) => {
        try {
          proc.kill("SIGKILL");
        } catch {
          // already dead
        }
        await proc.exited.catch(() => {});
      }),
    );
  }
}

function readPortFile(file: string): number | undefined {
  try {
    const port = Number(readFileSync(file, "utf8").trim());
    return Number.isInteger(port) && port > 0 ? port : undefined;
  } catch {
    return undefined;
  }
}
