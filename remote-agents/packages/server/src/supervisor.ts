import { OmpRpc } from "@remote-agents/rpc-driver";
import {
  isActiveStatus,
  isTerminalStatus,
  WORKERS_DIRNAME,
  type Channel,
  type CreateTaskInput,
  type RunningWorker,
  type Surface,
  type SurfaceKind,
  type Task,
  type TaskEventData,
  type TaskStatus,
  type WorkerSpec,
} from "@remote-agents/protocol";
import { mkdirSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { tcpChannel } from "./channel";
import { badRequest, conflict, notFound } from "./errors";
import type { TaskStore } from "./store";

export interface SupervisorOpts {
  /** Wall-clock backstop per task in ms; 0 disables. */
  maxTimeMs?: number;
  ompBin?: string;
  model?: string;
  adoptionConnectMs?: number;
  adoptionHandshakeMs?: number;
  adoptionPollMs?: number;
  /**
   * Quiet window after `agent_end` before settling without `agent_settled`.
   * Some omp builds never emit `agent_settled` (observed on the installed
   * binary); the supervisor then settles once the agent has been idle for
   * this long. 0 disables the fallback. Default 3000.
   */
  settleQuietMs?: number;
}

interface RpcEventLike {
  type: string;
  [key: string]: unknown;
}

/** Legal lifecycle edges; every transition is guarded on the current status. */
const TRANSITIONS: Record<TaskStatus, readonly TaskStatus[]> = {
  queued: ["starting", "finalizing", "canceled", "failed"],
  starting: ["running", "finalizing", "canceled", "failed"],
  running: ["finalizing", "canceled", "failed"],
  finalizing: ["completed", "failed", "canceled"],
  completed: [],
  failed: [],
  canceled: [],
};

const sleep = (ms: number): Promise<void> => {
  const { promise, resolve } = Promise.withResolvers<void>();
  const timer = setTimeout(resolve, ms);
  timer.unref?.();
  return promise;
};

const errMessage = (err: unknown): string => (err instanceof Error ? err.message : String(err));

function firstString(event: RpcEventLike, keys: string[]): string | undefined {
  for (const key of keys) {
    const value = event[key];
    if (typeof value === "string" && value.length > 0) return value;
  }
  return undefined;
}

/** Extract assistant text from a `message_end` rpc event; omp field shapes vary. */
function assistantTextOf(event: RpcEventLike): string | undefined {
  const message = event.message;
  if (message && typeof message === "object" && "role" in message && message.role === "assistant") {
    const content = "content" in message ? message.content : undefined;
    if (typeof content === "string") return content;
    if (Array.isArray(content)) {
      const parts: unknown[] = content;
      return parts
        .map((part) => {
          if (typeof part === "string") return part;
          if (part && typeof part === "object" && "text" in part && typeof part.text === "string") {
            return part.text;
          }
          return "";
        })
        .join("");
    }
  }
  const text = event.text;
  return typeof text === "string" ? text : undefined;
}

function shortDetail(args: unknown): string {
  let raw: string;
  try {
    raw = JSON.stringify(args) ?? "undefined";
  } catch {
    raw = "unserializable args";
  }
  return raw.length > 120 ? `${raw.slice(0, 117)}...` : raw;
}

function pidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return err !== null && typeof err === "object" && "code" in err && err.code === "EPERM";
  }
}

/**
 * Owns worker lifecycles: queued → starting → running → finalizing →
 * completed | failed | canceled, re-adoption after restart, steer/cancel/ack,
 * and the wall-clock deadline backstop.
 */
export class Supervisor {
  readonly kind: SurfaceKind;
  private readonly rpcs = new Map<string, OmpRpc>();
  private readonly workers = new Map<string, RunningWorker>();
  private readonly deadlines = new Map<string, Timer>();
  private readonly settleTimers = new Map<string, Timer>();
  private readonly lastAssistant = new Map<string, string>();
  private readonly pendingSteers = new Map<string, string[]>();
  private readonly settledWaiters = new Map<string, Array<(task: Task | undefined) => void>>();

  constructor(
    private readonly store: TaskStore,
    private readonly surface: Surface,
    private readonly opts: SupervisorOpts = {},
  ) {
    this.kind = surface.kind;
  }

  private get maxTimeMs(): number {
    return this.opts.maxTimeMs ?? 1_800_000;
  }

  private get settleQuietMs(): number {
    return this.opts.settleQuietMs ?? 3_000;
  }


  create(input: CreateTaskInput): Task {
    const task = this.store.create(input, this.surface.kind);
    void this.launch(task.id).catch(() => {}); // launch routes its own failures into the store
    return task;
  }

  /** Resolves once the task reaches a terminal status (test/observation seam). */
  whenSettled(id: string): Promise<Task | undefined> {
    const current = this.store.get(id);
    if (current && isTerminalStatus(current.status)) return Promise.resolve(current);
    const { promise, resolve } = Promise.withResolvers<Task | undefined>();
    const waiters = this.settledWaiters.get(id) ?? [];
    waiters.push(resolve);
    this.settledWaiters.set(id, waiters);
    return promise;
  }

  async steer(id: string, message: string): Promise<Task> {
    const task = this.store.get(id);
    if (!task) throw notFound(`task not found: ${id}`);
    if (typeof message !== "string" || message.trim().length === 0) {
      throw badRequest("message must be a non-empty string");
    }
    if (task.status === "queued") {
      // No worker yet: deliver right after the initial prompt lands.
      const pending = this.pendingSteers.get(id) ?? [];
      pending.push(message);
      this.pendingSteers.set(id, pending);
      return task;
    }
    if (task.status !== "running") throw conflict(`task is not steerable (status ${task.status})`);
    const rpc = this.rpcs.get(id);
    if (!rpc) throw conflict("worker is not attached");
    await rpc.steer(message);
    this.store.appendEvent(id, "log", { line: `steered: ${message}` });
    return this.store.get(id) ?? task;
  }

  async cancel(id: string): Promise<Task> {
    const task = this.store.get(id);
    if (!task) throw notFound(`task not found: ${id}`);
    if (isTerminalStatus(task.status)) throw conflict(`task already ${task.status}`);
    if (task.status === "finalizing") throw conflict("task is finalizing");
    this.store.update(id, { stopRequested: true });
    if (task.status === "queued") {
      // No worker was spawned yet; a launch in flight notices and backs off.
      this.transition(id, "canceled", "canceled before start");
      await this.teardown(id);
      return this.store.get(id) ?? task;
    }
    const rpc = this.rpcs.get(id);
    if (rpc) void rpc.abort().catch(() => {}); // graceful stop signal; stop() below is the hammer
    const worker = this.workers.get(id);
    if (worker) {
      try {
        await worker.stop();
      } catch {
        // best effort
      }
    }
    this.finishCancel(id, "cancel requested"); // idempotent if a close/settled handler won the race
    return this.store.get(id) ?? task;
  }

  ack(id: string): Task {
    const task = this.store.get(id);
    if (!task) throw notFound(`task not found: ${id}`);
    return this.store.ack(id);
  }

  /**
   * Re-adoption at boot. herdr workers (meta.port) are re-attached over TCP and
   * polled to completion; process workers are checked with kill(pid, 0) and
   * marked failed with a resume hint. Returns the number of workers adopted.
   */
  async adoptExisting(): Promise<number> {
    const active = this.store.all().filter((t) => isActiveStatus(t.status));
    let adopted = 0;
    for (const task of active) {
      if (await this.adoptTask(task.id)) adopted += 1;
    }
    return adopted;
  }

  /**
   * Graceful shutdown. Process-surface workers are children of this server
   * (piped stdio) and would die anyway, so they are aborted and marked
   * canceled. Herdr workers live in their own panes and are left running so
   * the next boot re-adopts them (contract: "known deliberate v1 limits").
   */
  async shutdown(): Promise<void> {
    const active = this.store.all().filter((t) => isActiveStatus(t.status));
    for (const task of active) {
      if (this.surface.kind !== "process") continue;
      this.store.update(task.id, { stopRequested: true });
      const rpc = this.rpcs.get(task.id);
      if (rpc) void rpc.abort().catch(() => {});
      const worker = this.workers.get(task.id);
      if (worker) {
        try {
          await worker.stop();
        } catch {
          // best effort
        }
      }
      this.transition(task.id, "canceled", "server shutdown");
      await this.teardown(task.id);
    }
    this.store.saveNow();
  }

  // ------------------------------------------------------------------ launch

  private async launch(id: string): Promise<void> {
    const task = this.store.get(id);
    if (!task || isTerminalStatus(task.status)) return;
    this.armDeadline(id);
    const spec: WorkerSpec = {
      taskId: id,
      name: task.name,
      cwd: task.cwd,
      sessionDir: path.join(this.store.stateDir, WORKERS_DIRNAME, id),
      ompBin: this.opts.ompBin ?? "omp",
      model: task.model ?? this.opts.model,
      maxTimeS: Math.max(1, Math.ceil(this.maxTimeMs / 1000)),
    };
    mkdirSync(spec.sessionDir, { recursive: true });
    let worker: RunningWorker;
    try {
      worker = await this.surface.start(spec);
    } catch (err) {
      this.fail(id, `failed to start worker: ${errMessage(err)}`);
      return;
    }
    if (!this.isActive(id)) {
      // Canceled (or otherwise settled) while the surface was starting.
      try {
        await worker.stop();
      } catch {
        // best effort
      }
      return;
    }
    if (!this.transition(id, "starting")) {
      try {
        await worker.stop();
      } catch {
        // best effort
      }
      return;
    }
    this.store.update(id, { worker: worker.meta });
    const rpc = new OmpRpc(worker.channel);
    this.rpcs.set(id, rpc);
    this.workers.set(id, worker);
    rpc.onEvent((event) => this.onWorkerEvent(id, event));
    rpc.onClose(() => this.onChannelClosed(id, worker));
    try {
      const ready = await rpc.start();
      if (ready.sessionFile && !this.store.get(id)?.sessionFile) {
        this.store.update(id, { sessionFile: ready.sessionFile });
      }
    } catch (err) {
      this.fail(id, `worker handshake failed: ${errMessage(err)}`);
      await this.teardown(id);
      return;
    }
    if (!this.transition(id, "running")) {
      await this.teardown(id);
      return;
    }
    try {
      await rpc.prompt(task.task);
    } catch (err) {
      // A slow/partial prompt response must not kill a healthy streaming
      // worker (agent_settled + the deadline remain the authority).
      if (this.isActive(id)) {
        this.store.appendEvent(id, "log", { line: `prompt response error: ${errMessage(err)}` });
      }
    }
    await this.flushPendingSteers(id);
  }

  /**
   * Fall back to globbing the worker's session dir when the ready frame
   * carries no sessionFile (observed on the installed omp build). Picks the
   * newest `*.jsonl` — the worker creates its session file with the first
   * message, so this must run after the turn has started. Callers settle
   * only after activity, which guarantees the file exists.
   */
  private recordSessionFile(id: string): void {
    if (this.store.get(id)?.sessionFile) return;
    const sessionDir = path.join(this.store.stateDir, WORKERS_DIRNAME, id);
    try {
      const files = readdirSync(sessionDir)
        .filter((f) => f.endsWith(".jsonl"))
        .map((f) => path.join(sessionDir, f))
        .filter((f) => {
          try {
            return statSync(f).isFile();
          } catch {
            return false;
          }
        })
        .sort((a, b) => statSync(b).mtimeMs - statSync(a).mtimeMs);
      const newest = files[0];
      if (newest) this.store.update(id, { sessionFile: newest });
    } catch {
      // Session dir unreadable — leave sessionFile unset.
    }
  }

  private async flushPendingSteers(id: string): Promise<void> {
    const pending = this.pendingSteers.get(id);
    if (!pending || pending.length === 0) return;
    this.pendingSteers.delete(id);
    const rpc = this.rpcs.get(id);
    if (!rpc) return;
    for (const message of pending) {
      try {
        await rpc.steer(message);
        this.store.appendEvent(id, "log", { line: `steered: ${message}` });
      } catch {
        // Worker already gone; the task state will reflect that shortly.
      }
    }
  }

  // ------------------------------------------------------------ rpc plumbing

  private onWorkerEvent(id: string, event: RpcEventLike): void {
    switch (event.type) {
      case "tool_execution_start": {
        const toolName = firstString(event, ["toolName", "name", "tool"]) ?? "tool";
        const args = event.args ?? event.input ?? event.arguments;
        const data: TaskEventData = { toolName };
        if (args !== undefined) data.detail = shortDetail(args);
        this.store.appendEvent(id, "activity", data);
        return;
      }
      case "message_end": {
        const text = assistantTextOf(event);
        if (text !== undefined && text.length > 0) this.lastAssistant.set(id, text);
        return;
      }
      case "agent_settled":
        this.clearSettleQuiet(id);
        void this.settleFromWorker(id);
        return;
      case "agent_end":
        this.armSettleQuiet(id);
        return;
      case "agent_start":
      case "turn_start":
      case "message_start":
        // A follow-up turn (retry, compaction, steer) began; not idle after all.
        this.clearSettleQuiet(id);
        return;
      default:
        return; // forwarded verbatim rpc noise we do not act on
    }
  }

  /**
   * Settle once the agent has been idle for `settleQuietMs`. Some omp builds
   * never emit `agent_settled`; `agent_end` plus this quiet window is then the
   * only completion signal. The window re-arms itself while `get_state` still
   * reports streaming (retry/compaction turns keep the task alive).
   */
  private armSettleQuiet(id: string): void {
    if (this.settleQuietMs <= 0) return;
    this.clearSettleQuiet(id);
    const timer = setTimeout(() => {
      this.settleTimers.delete(id);
      void this.settleQuietly(id);
    }, this.settleQuietMs);
    timer.unref?.();
    this.settleTimers.set(id, timer);
  }

  private clearSettleQuiet(id: string): void {
    const timer = this.settleTimers.get(id);
    if (timer) {
      clearTimeout(timer);
      this.settleTimers.delete(id);
    }
  }

  private async settleQuietly(id: string): Promise<void> {
    const rpc = this.rpcs.get(id);
    if (rpc) {
      try {
        const state = await rpc.getState();
        if (state.isStreaming === true) {
          this.armSettleQuiet(id);
          return;
        }
      } catch {
        // Channel already dead: settle from what we have.
      }
    }
    await this.settleFromWorker(id);
  }

  private async settleFromWorker(id: string): Promise<void> {
    const task = this.store.get(id);
    if (!task || isTerminalStatus(task.status) || task.status === "finalizing") return;
    if (task.stopRequested) {
      this.finishCancel(id, "aborted by request");
      return;
    }
    if (!this.transition(id, "finalizing")) return;
    let text: string | undefined;
    const rpc = this.rpcs.get(id);
    if (rpc) {
      try {
        const response = await rpc.getLastAssistantText();
        const reported = response.text;
        if (typeof reported === "string") text = reported;
      } catch {
        // fall back to the buffered assistant text
      }
    }
    this.recordSessionFile(id);
    const result = text ?? this.lastAssistant.get(id) ?? "";
    this.store.update(id, { result });
    this.store.appendEvent(id, "result", { text: result });
    this.transition(id, "completed");
    await this.teardown(id);
  }

  private onChannelClosed(id: string, worker: RunningWorker): void {
    const task = this.store.get(id);
    if (!task || isTerminalStatus(task.status) || task.status === "finalizing") return;
    if (task.stopRequested) {
      this.finishCancel(id, "worker stopped");
      return;
    }
    const meta = worker.meta;
    const code =
      "exitCode" in meta && typeof meta.exitCode === "number" ? ` (code ${meta.exitCode})` : "";
    this.fail(id, `worker exited${code}`);
  }

  private onAdoptedChannelClosed(id: string): void {
    const task = this.store.get(id);
    if (!task || isTerminalStatus(task.status) || task.status === "finalizing") return;
    if (task.stopRequested) {
      this.finishCancel(id, "worker stopped");
      return;
    }
    this.fail(id, "worker connection closed before completion");
  }

  // ------------------------------------------------------------ transitions

  private transition(id: string, to: TaskStatus, reason?: string): boolean {
    const task = this.store.get(id);
    if (!task) return false;
    const from = task.status;
    if (from === to || !TRANSITIONS[from].includes(to)) return false;
    const patch: Partial<Task> = { status: to };
    if (!task.startedAt) patch.startedAt = Date.now();
    if (isTerminalStatus(to)) patch.endedAt = Date.now();
    this.store.update(id, patch);
    const data: TaskEventData = { from, to };
    if (reason !== undefined) data.reason = reason;
    this.store.appendEvent(id, "state_changed", data);
    if (isTerminalStatus(to)) this.resolveSettled(id);
    return true;
  }

  private resolveSettled(id: string): void {
    const waiters = this.settledWaiters.get(id);
    if (!waiters) return;
    this.settledWaiters.delete(id);
    const task = this.store.get(id);
    for (const resolve of waiters) resolve(task);
  }

  private finishCancel(id: string, reason: string): void {
    if (this.transition(id, "canceled", reason)) void this.teardown(id);
  }

  private fail(id: string, message: string): void {
    const task = this.store.get(id);
    if (!task || isTerminalStatus(task.status)) return;
    if (task.stopRequested) {
      // A cancel landed while we were failing; cancellation wins the race.
      this.finishCancel(id, message);
      return;
    }
    this.store.update(id, { error: message });
    this.store.appendEvent(id, "error", { message });
    if (this.transition(id, "failed", message)) void this.teardown(id);
  }

  private isActive(id: string): boolean {
    const task = this.store.get(id);
    return task !== undefined && isActiveStatus(task.status);
  }

  // -------------------------------------------------------------- resources

  private armDeadline(id: string): void {
    if (this.maxTimeMs <= 0) return;
    const timer = setTimeout(() => {
      this.deadlines.delete(id);
      this.fail(id, `deadline exceeded (${this.maxTimeMs}ms wall clock)`);
    }, this.maxTimeMs);
    timer.unref?.();
    this.deadlines.set(id, timer);
  }

  private clearDeadline(id: string): void {
    const timer = this.deadlines.get(id);
    if (timer) {
      clearTimeout(timer);
      this.deadlines.delete(id);
    }
  }

  private async teardown(id: string): Promise<void> {
    this.clearDeadline(id);
    this.clearSettleQuiet(id);
    this.lastAssistant.delete(id);
    this.pendingSteers.delete(id);
    const rpc = this.rpcs.get(id);
    if (rpc) {
      this.rpcs.delete(id);
      try {
        await rpc.close();
      } catch {
        // already closed
      }
    }
    const worker = this.workers.get(id);
    if (worker) {
      this.workers.delete(id);
      try {
        await worker.stop();
      } catch {
        // best effort
      }
    }
  }

  // --------------------------------------------------------------- adoption

  private async adoptTask(id: string): Promise<boolean> {
    const task = this.store.get(id);
    if (!task || !isActiveStatus(task.status)) return false;
    const meta = task.worker;
    if (meta?.port !== undefined) return this.adoptTcp(task.id, meta.port);
    if (meta?.pid !== undefined) return this.adoptProcess(task.id, meta.pid);
    this.fail(id, `worker record missing (task was ${task.status} when the server restarted)`);
    return false;
  }

  private async adoptTcp(id: string, port: number): Promise<boolean> {
    const connectMs = this.opts.adoptionConnectMs ?? 3_000;
    let channel: Channel;
    try {
      channel = await this.connectTcp(port, connectMs);
    } catch {
      this.fail(id, `worker bridge unreachable on port ${port}`);
      return false;
    }
    const rpc = new OmpRpc(channel, {
      handshakeTimeoutMs: this.opts.adoptionHandshakeMs ?? 3_000,
    });
    this.rpcs.set(id, rpc);
    rpc.onEvent((event) => this.onWorkerEvent(id, event));
    rpc.onClose(() => this.onAdoptedChannelClosed(id));
    try {
      await rpc.start(); // bridge replays the worker's authentic ready frame
    } catch {
      // Tolerate bridges that do not replay ready; requests still work.
    }
    this.store.appendEvent(id, "log", { line: "readopted" });
    this.armDeadline(id);
    void this.pollAdopted(id);
    return true;
  }

  private adoptProcess(id: string, pid: number): boolean {
    if (pidAlive(pid)) {
      this.fail(id, `orphaned worker (pid ${pid}) — resume via session file`);
      return false;
    }
    this.fail(id, `worker process (pid ${pid}) exited before completion`);
    return false;
  }

  private async pollAdopted(id: string): Promise<void> {
    const pollMs = this.opts.adoptionPollMs ?? 1_000;
    let failures = 0;
    for (;;) {
      await sleep(pollMs);
      const task = this.store.get(id);
      if (!task || isTerminalStatus(task.status)) return;
      const rpc = this.rpcs.get(id);
      if (!rpc) return;
      try {
        const state = await rpc.getState();
        failures = 0;
        const streaming = state.isStreaming;
        if (streaming === false) {
          await this.settleFromWorker(id);
          return;
        }
      } catch {
        failures += 1;
        if (failures >= 3) {
          this.fail(id, "worker unreachable after re-attach");
          return;
        }
      }
    }
  }

  private async connectTcp(port: number, budgetMs: number): Promise<Channel> {
    const deadline = Date.now() + budgetMs;
    let lastErr: unknown;
    while (Date.now() < deadline) {
      try {
        return await tcpChannel("127.0.0.1", port);
      } catch (err) {
        lastErr = err;
        await sleep(100);
      }
    }
    throw lastErr ?? new Error(`tcp connect to 127.0.0.1:${port} timed out`);
  }
}
