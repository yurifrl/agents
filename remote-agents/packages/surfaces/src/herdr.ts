import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import type { Channel, RunningWorker, Surface, SurfaceKind, WorkerSpec } from "@remote-agents/protocol";
import { renderBridgeScript } from "./bridge";
import { SurfaceError } from "./error";
import { TASK_ID_ENV_VAR, WORKER_ENV_VAR, workerArgv } from "./process";

export const DEFAULT_HERDR_BIN = "herdr";
export const DEFAULT_HERDR_WORKSPACE = "remote-agents";

const PORT_POLL_INTERVAL_MS = 500;
const PORT_POLL_BUDGET_MS = 30_000;
const PANE_RUN_TIMEOUT_MS = 20_000;
const CLI_TIMEOUT_MS = 60_000;

export interface HerdrSurfaceOptions {
  herdrBin?: string;
  /** Workspace name ensured/created per start (default "remote-agents"). */
  workspace?: string;
  /** Bridge script location; defaults to <sessionDir>/bridge.mjs. */
  bridgePath?: string;
}

interface CliResult {
  code: number;
  stdout: string;
  stderr: string;
  timedOut: boolean;
}

interface SocketHandle {
  write(data: Uint8Array): unknown;
  end(): unknown;
}

interface SocketReadable {
  readable: ReadableStream<Uint8Array>;
  push(chunk: Uint8Array): void;
  end(): void;
  fail(cause: unknown): void;
}

/**
 * Runs workers inside a herdr pane, bridged over TCP so they survive server
 * restarts (CONTRACT.md §3):
 *  1. ensure the workspace exists (`workspace list` / `workspace create`)
 *  2. create a dedicated tab (`tab create --workspace ... --label ra-<taskId>`)
 *     and use its returned root pane — the workspace root pane stays idle
 *  3. write bridge.mjs + an empty port file into the session dir
 *  4. `pane run <paneId> "<quoted bridge command>"` (execFile-style argv)
 *  5. poll the port file, then Bun.connect → Channel
 *  6. stop() closes the socket and the pane (best effort)
 *
 * Every failure surfaces as SurfaceError("herdr_unavailable", ..., cause) so
 * the server can fall back per its config policy.
 */

/**
 * Herdr session identity inherited from the omp session that spawned this
 * server (pi-herdr-subagents convention). When present, worker tabs are
 * created in the user's current workspace — visible, not hidden away.
 */
function inheritedWorkspaceId(): string | undefined {
  const id = process.env.HERDR_WORKSPACE_ID;
  return id && id.trim() !== "" ? id : undefined;
}

export class HerdrSurface implements Surface {
  readonly kind: SurfaceKind = "herdr";

  private readonly herdrBin: string;
  private readonly workspaceName: string;
  private readonly bridgePath: string | undefined;

  constructor(opts: HerdrSurfaceOptions = {}) {
    this.herdrBin = opts.herdrBin ?? DEFAULT_HERDR_BIN;
    this.workspaceName = opts.workspace ?? DEFAULT_HERDR_WORKSPACE;
    this.bridgePath = opts.bridgePath;
  }

  async start(spec: WorkerSpec): Promise<RunningWorker> {
    try {
      return await this.startWorker(spec);
    } catch (err) {
      if (err instanceof SurfaceError) throw err;
      throw new SurfaceError("herdr_unavailable", describe(err), err);
    }
  }

  private async startWorker(spec: WorkerSpec): Promise<RunningWorker> {
    // Visible by default: when the server inherited a herdr session context
    // (extension-spawned), land the worker tab in THAT workspace so the user
    // sees the agent working next to their session. The dedicated
    // "remote-agents" workspace is only the fallback for headless servers.
    const inherited = inheritedWorkspaceId();
    const workspaceId = inherited ?? (await this.ensureWorkspace(this.workspaceName));
    const paneId = await this.createTab(workspaceId, spec);
    void this.reportTask(paneId, spec);

    mkdirSync(spec.sessionDir, { recursive: true });
    const bridgePath = this.bridgePath ?? join(spec.sessionDir, "bridge.mjs");
    mkdirSync(dirname(bridgePath), { recursive: true });
    writeFileSync(bridgePath, renderBridgeScript());
    const portFile = join(spec.sessionDir, "port");
    writeFileSync(portFile, ""); // bridge replaces this atomically once bound

    const command = paneCommand(spec, bridgePath, portFile);
    const launched = await this.run(["pane", "run", paneId, command], PANE_RUN_TIMEOUT_MS);
    if (!launched.timedOut && launched.code !== 0) {
      throw new Error(
        `herdr pane run exited ${launched.code}: ${cliDetail(launched)}`,
      );
    }

    const port = await this.waitForPort(portFile);
    if (port === undefined) {
      await this.closePaneBestEffort(paneId);
      throw new Error(`port file ${portFile} never appeared within ${PORT_POLL_BUDGET_MS}ms`);
    }

    const stream = makeSocketReadable();
    let socket: SocketHandle;
    try {
      socket = await Bun.connect({
        hostname: "127.0.0.1",
        port,
        socket: {
          data(_sock, chunk) {
            stream.push(chunk);
          },
          close() {
            stream.end();
          },
          error(_sock, cause) {
            stream.fail(cause);
          },
        },
      });
    } catch (cause) {
      await this.closePaneBestEffort(paneId);
      throw new Error(`TCP connect to bridge port ${port} failed: ${describe(cause)}`);
    }

    const channel: Channel = {
      readable: stream.readable,
      writable: new WritableStream<Uint8Array>({
        write(chunk) {
          socket.write(chunk);
        },
        close() {
          try {
            socket.end();
          } catch {
            // Socket already gone.
          }
        },
        abort() {
          try {
            socket.end();
          } catch {
            // Socket already gone.
          }
        },
      }),
      close: async () => {
        try {
          socket.end();
        } catch {
          // Socket already gone.
        }
      },
    };

    return {
      channel,
      meta: { paneId, workspaceId, port, portFile },
      stop: async () => {
        try {
          socket.end();
        } catch {
          // Socket already gone.
        }
        await this.closePaneBestEffort(paneId);
      },
    };
  }

  // -- herdr CLI plumbing ---------------------------------------------------

  /** Find-or-create the workspace by label; returns its id. */
  private async ensureWorkspace(name: string): Promise<string> {
    const list = await this.runJson(["workspace", "list"]);
    const existing = workspaceIdFromList(list, name);
    if (existing) return existing;

    const created = await this.runJson(["workspace", "create", "--label", name, "--no-focus"]);
    const result = resultOf(created);
    const id = pickString(recordOf(result.workspace), ["workspace_id", "workspaceId", "id"])
      ?? pickString(result, ["workspace_id", "workspaceId", "id"]);
    if (!id) throw new Error("herdr workspace create output carried no workspace id");
    return id;
  }

  /**
   * Best-effort herdr UI metadata so the pane/tab shows what the agent is
   * doing (pi-herdr-subagents `report-metadata` convention). Never throws:
   * cosmetic only.
   */
  private async reportTask(paneId: string, spec: WorkerSpec): Promise<void> {
    try {
      await this.run([
        "pane",
        "report-metadata",
        paneId,
        "--source",
        "remote-agents",
        "--token",
        `task=${spec.name}`,
      ]);
      await this.run(["pane", "rename", paneId, `▸ ${spec.name}`]);
    } catch {
      // cosmetic only
    }
  }

  /** Create the task tab and return its root pane id. */
  private async createTab(workspaceId: string, spec: WorkerSpec): Promise<string> {
    const created = await this.runJson([
      "tab",
      "create",
      "--workspace",
      workspaceId,
      "--label",
      `▸ ${spec.name}`,
      "--cwd",
      spec.cwd,
      "--no-focus",
    ]);
    const paneId = paneIdFromCreate(created);
    if (!paneId) throw new Error("herdr tab create output carried no root pane id");
    return paneId;
  }

  private async run(args: string[], timeoutMs = CLI_TIMEOUT_MS): Promise<CliResult> {
    let proc: Bun.Subprocess;
    try {
      proc = Bun.spawn([this.herdrBin, ...args], {
        stdin: "ignore",
        stdout: "pipe",
        stderr: "pipe",
        env: process.env,
      });
    } catch (cause) {
      throw new SurfaceError(
        "herdr_unavailable",
        `cannot execute ${this.herdrBin} (missing from PATH?)`,
        cause,
      );
    }

    const out = proc.stdout;
    const err = proc.stderr;
    if (!(out instanceof ReadableStream) || !(err instanceof ReadableStream)) {
      throw new SurfaceError("herdr_unavailable", "unexpected subprocess stdio shape");
    }
    const collect = (async () => {
      const [stdout, stderr] = await Promise.all([
        new Response(out).text(),
        new Response(err).text(),
      ]);
      const code = await proc.exited;
      return { code, stdout, stderr, timedOut: false };
    })().catch(() => ({ code: -1, stdout: "", stderr: "", timedOut: true }));

    const timeout = Promise.withResolvers<CliResult>();
    const timer = setTimeout(() => {
      try {
        proc.kill("SIGKILL");
      } catch {
        // Already exited.
      }
      timeout.resolve({ code: -1, stdout: "", stderr: "", timedOut: true });
    }, timeoutMs);
    try {
      return await Promise.race([collect, timeout.promise]);
    } finally {
      clearTimeout(timer);
    }
  }

  private async runJson(args: string[]): Promise<Record<string, unknown>> {
    const res = await this.run(args);
    if (res.timedOut) {
      throw new Error(`${this.herdrBin} ${args.join(" ")} timed out`);
    }
    if (res.code !== 0) {
      throw new Error(`${this.herdrBin} ${args.join(" ")} exited ${res.code}: ${cliDetail(res)}`);
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(res.stdout);
    } catch (cause) {
      throw new Error(`unparseable JSON from ${this.herdrBin} ${args.join(" ")}`, { cause });
    }
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      throw new Error(`unexpected JSON shape from ${this.herdrBin} ${args.join(" ")}`);
    }
    return parsed as Record<string, unknown>;
  }

  private async waitForPort(portFile: string): Promise<number | undefined> {
    const deadline = Date.now() + PORT_POLL_BUDGET_MS;
    for (;;) {
      await sleep(PORT_POLL_INTERVAL_MS);
      const port = readPortFile(portFile);
      if (port !== undefined) return port;
      if (Date.now() >= deadline) return undefined;
    }
  }

  private async closePaneBestEffort(paneId: string): Promise<void> {
    if (paneId.length === 0) return;
    try {
      await this.run(["pane", "close", paneId], 10_000);
    } catch {
      // Best effort by contract.
    }
  }
}

// -- helpers ----------------------------------------------------------------

function shellQuote(arg: string): string {
  return `'${arg.replace(/'/g, "'\\''")}'`;
}

/** The command typed into the pane shell: env vars + bun bridge -- omp args. */
function paneCommand(spec: WorkerSpec, bridgePath: string, portFile: string): string {
  const parts = [
    "env",
    shellQuote(`${WORKER_ENV_VAR}=1`),
    shellQuote(`${TASK_ID_ENV_VAR}=${spec.taskId}`),
    shellQuote(process.execPath),
    shellQuote(bridgePath),
    "--port-file",
    shellQuote(portFile),
    "--",
    ...workerArgv(spec).map(shellQuote),
  ];
  return parts.join(" ");
}

function readPortFile(portFile: string): number | undefined {
  let raw: string;
  try {
    raw = readFileSync(portFile, "utf8").trim();
  } catch {
    return undefined; // not written yet
  }
  const port = Number.parseInt(raw, 10);
  return Number.isInteger(port) && port > 0 ? port : undefined;
}

export function makeSocketReadable(): SocketReadable {
  let pushChunk: (chunk: Uint8Array) => void = () => {};
  let endStream: () => void = () => {};
  let failStream: (cause: unknown) => void = () => {};
  const readable = new ReadableStream<Uint8Array>({
    start(controller) {
      pushChunk = (chunk) => {
        try {
          controller.enqueue(chunk);
        } catch {
          // Stream already closed.
        }
      };
      endStream = () => {
        try {
          controller.close();
        } catch {
          // Stream already closed.
        }
      };
      failStream = (cause) => {
        try {
          controller.error(cause);
        } catch {
          // Stream already closed.
        }
      };
    },
  });
  return {
    readable,
    push: (chunk) => pushChunk(chunk),
    end: () => endStream(),
    fail: (cause) => failStream(cause),
  };
}

function resultOf(payload: Record<string, unknown>): Record<string, unknown> {
  return recordOf(payload.result) ?? {};
}

function recordOf(value: unknown): Record<string, unknown> | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

function pickString(
  record: Record<string, unknown> | undefined,
  keys: string[],
): string | undefined {
  if (!record) return undefined;
  for (const key of keys) {
    const value = record[key];
    if (typeof value === "string" && value.length > 0) return value;
  }
  return undefined;
}

function workspaceIdFromList(
  payload: Record<string, unknown>,
  label: string,
): string | undefined {
  const workspaces = resultOf(payload).workspaces;
  if (!Array.isArray(workspaces)) return undefined;
  for (const entry of workspaces) {
    const ws = recordOf(entry);
    if (!ws || (ws.label !== label && ws.name !== label)) continue;
    const id = pickString(ws, ["workspace_id", "workspaceId", "id"]);
    if (id) return id;
  }
  return undefined;
}

function paneIdFromCreate(payload: Record<string, unknown>): string | undefined {
  const result = resultOf(payload);
  return (
    pickString(recordOf(result.root_pane), ["pane_id", "paneId", "id"]) ??
    pickString(result, ["root_pane_id", "pane_id", "paneId", "id"])
  );
}

function cliDetail(res: CliResult): string {
  const detail = res.stderr.trim() || res.stdout.trim();
  return detail.length > 0 ? detail.slice(0, 400) : "(no output)";
}

function describe(value: unknown): string {
  return value instanceof Error ? value.message : String(value);
}

function sleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}
