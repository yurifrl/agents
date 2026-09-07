// omp extension: native-feeling remote subagents — widget, model tools,
// /remote-agents commands, result steer-back, and resume-after-restart reconcile.
//
// Runs inside omp via jiti. Per contract §5 this package imports ONLY
// @remote-agents/protocol + typebox, so the omp ExtensionAPI surface is
// mirrored below as a minimal structural interface (matches the omp 18.1.13
// typings; real omp objects fit structurally). The factory only registers;
// all IO is deferred to session_start.

import {
  ACTIVE_STATUSES,
  ENV,
  isActiveStatus,
  isTerminalStatus,
  type CreateTaskInput,
  type SseFrame,
  type Task,
  type TaskStatus,
} from "@remote-agents/protocol";
import { Type, type Static, type TSchema } from "typebox";
import { dirname, join } from "node:path";
import { ApiError, ensureServer } from "./api.ts";
import {
  describeTask,
  formatElapsed,
  formatEventLine,
  formatFailure,
  formatResult,
  fieldOf,
} from "./deliver.ts";
import { ensureRuntime, teardownRuntime, type RemoteAgentsRuntime } from "./runtime.ts";
import { parseAgentsArgs } from "./parse.ts";
import { renderWidget } from "./widget.ts";

// ---------------------------------------------------------------------------
// omp extension API (structural subset of omp 18.1.13 typings)
// ---------------------------------------------------------------------------

/** Minimal render-only component (omp's Component interface). */
interface PiComponent {
  render(width: number): string[];
}

interface PiMessage {
  customType: string;
  content: string | Array<{ type: string }>;
  display: boolean;
  details?: unknown;
}

type PiMessageRenderer = (
  message: PiMessage,
  options: { expanded: boolean },
  theme: unknown,
) => PiComponent | undefined;

interface PiUI {
  setWidget(
    key: string,
    content:
      | string[]
      | ((tui: unknown, theme: unknown) => PiComponent & { dispose?(): void })
      | undefined,
    options?: { placement?: "aboveEditor" | "belowEditor" },
  ): void;
  notify(message: string, level?: "info" | "warning" | "error"): void;
}

interface PiContext {
  ui: PiUI;
  mode: "tui" | "rpc" | "json" | "print";
  hasUI: boolean;
  cwd: string;
  sessionManager: { getSessionFile(): string | undefined };
}

interface PiToolDef<TParams extends TSchema = TSchema> {
  name: string;
  label: string;
  description: string;
  promptSnippet?: string;
  promptGuidelines?: string[];
  parameters: TParams;
  execute(
    toolCallId: string,
    params: Static<TParams>,
    signal: AbortSignal | undefined,
    onUpdate: ((details: unknown) => void) | undefined,
    ctx: PiContext,
  ): Promise<{ content: Array<{ type: "text"; text: string }>; details?: unknown }>;
}

interface PiEvent {
  type: string;
  reason?: string;
}

type PiEventHandler = (event: PiEvent, ctx: PiContext) => void | Promise<void>;

interface PiExtensionAPI {
  on(event: "session_start" | "session_shutdown", handler: PiEventHandler): void;
  registerTool<TParams extends TSchema>(tool: PiToolDef<TParams>): void;
  registerCommand(
    name: string,
    options: {
      description?: string;
      getArgumentCompletions?: (argumentPrefix: string) => unknown;
      handler: (args: string, ctx: PiContext) => Promise<void> | void;
    },
  ): void;
  registerMessageRenderer(customType: string, renderer: PiMessageRenderer): void;
  sendMessage(
    message: PiMessage,
    options?: { triggerTurn?: boolean; deliverAs?: "steer" | "followUp" | "nextTurn" },
  ): void;
}

// ---------------------------------------------------------------------------
// Constants + tool schemas
// ---------------------------------------------------------------------------

const CUSTOM_TYPE = "remote_agent_result";
const WIDGET_KEY = "remote-agents";
export const WIDGET_INTERVAL_MS = 2_000;
const WIDGET_EMPTY_STOP = 2;
const RESULT_RENDER_LIMIT = 2_000;

const SpawnParams = Type.Object({
  name: Type.String({ description: "Short display name for the agent (widget row / worker pane label)." }),
  task: Type.String({ description: "The full prompt the remote agent should work on." }),
  cwd: Type.Optional(
    Type.String({ description: "Absolute working directory for the agent. Defaults to this session's cwd." }),
  ),
  model: Type.Optional(Type.String({ description: "Model id override for the worker (server default otherwise)." })),
});

const EmptyParams = Type.Object({});

const IdParams = Type.Object({
  id: Type.String({ description: "Remote agent task id (from RemoteAgentList or the spawn result)." }),
});

const SteerParams = Type.Object({
  id: Type.String({ description: "Remote agent task id." }),
  message: Type.String({ description: "Message to steer the running agent with." }),
});

// ---------------------------------------------------------------------------
// Factory
// ---------------------------------------------------------------------------

export default function remoteAgentsExtension(pi: PiExtensionAPI): void {
  registerTools(pi);
  registerCommand(pi);
  registerRenderer(pi);

  pi.on("session_start", (_event, ctx) => reconcile(pi, ctx));
  pi.on("session_shutdown", () => {
    // Deliberate (contract §5): the server and its workers keep running — they
    // belong to the remote-agents server, not this parent session, and the
    // next session_start re-adopts them. We only drop this session's handles.
    teardownRuntime();
  });
}

// ---------------------------------------------------------------------------
// session_start reconcile
// ---------------------------------------------------------------------------

/**
 * ensureServer -> attach(owner) -> subscribe active tasks -> deliver+ack
 * terminal unacknowledged ones -> widget loop.
 */
async function reconcile(pi: PiExtensionAPI, ctx: PiContext): Promise<void> {
  const rt = ensureRuntime();
  try {
    await ensureServer(rt.client, { serverEntry: resolveServerEntry(), stateDir: resolveStateDir() });
  } catch (error) {
    // Server unavailable: disable quietly for this session, never throw.
    ctx.ui.notify(`remote-agents disabled: ${errorMessage(error)}`, "error");
    return;
  }
  let tasks: Task[];
  try {
    tasks = (await rt.client.attach(resolveOwner())).tasks;
  } catch (error) {
    ctx.ui.notify(`remote-agents: attach failed: ${errorMessage(error)}`, "error");
    return;
  }
  for (const task of tasks) {
    if (isActiveStatus(task.status)) {
      subscribeTask(pi, rt, task.id);
    } else if (isTerminalStatus(task.status) && !task.acknowledged) {
      void deliverAndAck(pi, rt, task.id);
    }
  }
  if (ctx.hasUI) startWidgetTimer(rt, ctx);
}

/**
 * Owner-scoped delivery (contract §5): every non-acknowledged task of the
 * owner is reconciled by whoever attaches — a fresh parent session picks up
 * both live and finished work. Double-delivery across two concurrent parents
 * is self-healing: the first delivery acks, the other skips.
 */

// ---------------------------------------------------------------------------
// Result delivery + SSE subscriptions
// ---------------------------------------------------------------------------

/**
 * Deliver one task outcome to this parent (steer + custom message) and ack it
 * server-side. At most once per task per runtime: the delivered set is marked
 * synchronously to fence concurrent result/state events, then confirmed after
 * the task fetch (transient fetch failure un-marks so a later event retries).
 */
async function deliverAndAck(pi: PiExtensionAPI, rt: RemoteAgentsRuntime, taskId: string): Promise<void> {
  if (rt.delivered.has(taskId)) return;
  rt.delivered.add(taskId);
  const task = await rt.client.get(taskId).catch(() => undefined);
  if (task == null) {
    rt.delivered.delete(taskId);
    return;
  }
  if (task.acknowledged) return; // consumed elsewhere; nothing to deliver
  const deliverable = task.status === "completed" ? formatResult(task) : formatFailure(task);
  pi.sendMessage(
    { customType: CUSTOM_TYPE, content: deliverable.content, display: true, details: deliverable.details },
    { triggerTurn: true, deliverAs: "steer" },
  );
  await rt.client.ack(taskId).catch(() => undefined);
}

function subscribeTask(pi: PiExtensionAPI, rt: RemoteAgentsRuntime, taskId: string): void {
  if (rt.subscriptions.has(taskId)) return;
  const controller = new AbortController();
  rt.subscriptions.set(taskId, controller);
  void rt.client
    .stream(taskId, rt.lastSeq.get(taskId) ?? 0, (frame) => onFrame(pi, rt, taskId, controller, frame), controller.signal)
    .catch(() => undefined)
    .finally(() => {
      if (rt.subscriptions.get(taskId) === controller) rt.subscriptions.delete(taskId);
    });
}

function onFrame(
  pi: PiExtensionAPI,
  rt: RemoteAgentsRuntime,
  taskId: string,
  controller: AbortController,
  frame: SseFrame,
): void {
  rt.lastSeq.set(taskId, frame.seq);
  if (frame.type !== "state_changed") return;
  const to = transitionTarget(frame);
  if (to == null || !isTerminalStatus(to)) return;
  // Terminal: deliver once, then stop listening (server keeps the worker).
  void deliverAndAck(pi, rt, taskId).finally(() => controller.abort());
}

function transitionTarget(frame: SseFrame): TaskStatus | undefined {
  const to = fieldOf(frame.data, "to");
  if (typeof to !== "string" || !isKnownStatus(to)) return undefined;
  return to;
}

/** Narrow a wire-provided status string onto the protocol union. */
function isKnownStatus(value: string): value is TaskStatus {
  return (
    (ACTIVE_STATUSES as readonly string[]).includes(value) ||
    value === "completed" ||
    value === "failed" ||
    value === "canceled"
  );
}

// ---------------------------------------------------------------------------
// Widget loop
// ---------------------------------------------------------------------------

export function startWidgetTimer(
  rt: RemoteAgentsRuntime,
  ctx: PiContext,
  intervalMs: number = WIDGET_INTERVAL_MS,
): void {
  if (rt.widgetTimer != null) return;
  rt.emptyPolls = 0;
  rt.widgetTimer = setInterval(() => {
    void widgetTick(rt, ctx);
  }, intervalMs);
  void widgetTick(rt, ctx);
}

export function stopWidgetTimer(rt: RemoteAgentsRuntime): void {
  if (rt.widgetTimer != null) {
    clearInterval(rt.widgetTimer);
    rt.widgetTimer = undefined;
  }
  rt.emptyPolls = 0;
}

/** One widget poll; exported as a test seam (self-stops after 2 empty polls). */
export async function widgetTick(rt: RemoteAgentsRuntime, ctx: PiContext): Promise<void> {
  let tasks: Task[];
  try {
    tasks = await rt.client.list(resolveOwner());
  } catch {
    return; // transient failure: keep the last render
  }
  rt.lastTasks = tasks;
  const shown = tasks.filter(
    (task) => isActiveStatus(task.status) || !task.acknowledged,
  );
  if (ctx.hasUI) {
    // Component factory: omp re-invokes render(width) on every redraw, so the
    // elapsed clocks tick between polls without extra server round-trips.
    ctx.ui.setWidget(
      WIDGET_KEY,
      shown.length > 0
        ? (_tui, _theme) => ({
            invalidate() {},
            render(width: number) {
              return renderWidget(rt.lastTasks, Date.now(), width);
            },
          })
        : undefined,
      { placement: "aboveEditor" },
    );
  }
  if (shown.length === 0 && rt.subscriptions.size === 0) {
    rt.emptyPolls += 1;
    if (rt.emptyPolls >= WIDGET_EMPTY_STOP) stopWidgetTimer(rt);
  } else {
    rt.emptyPolls = 0;
  }
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

function registerTools(pi: PiExtensionAPI): void {
  pi.registerTool(spawnTool(pi));
  pi.registerTool(listTool());
  pi.registerTool(getTool());
  pi.registerTool(steerTool());
  pi.registerTool(killTool());
}

function toolText(
  text: string,
  details?: unknown,
): { content: Array<{ type: "text"; text: string }>; details?: unknown } {
  return { content: [{ type: "text", text }], details };
}

function spawnTool(pi: PiExtensionAPI): PiToolDef<typeof SpawnParams> {
  return {
    name: "RemoteAgent",
    label: "RemoteAgent",
    description:
      "Spawn a remote coding agent that runs as its own worker process on the remote-agents server. Use it for background work; the result is delivered back to this session automatically as a message when the agent finishes.",
    promptSnippet: "RemoteAgent: spawn a background remote coding agent; results are delivered automatically.",
    promptGuidelines: [
      "Results are delivered automatically as messages; do not poll RemoteAgentGet to wait for completion.",
    ],
    parameters: SpawnParams,
    async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
      const rt = ensureRuntime();
      const input: CreateTaskInput = {
        owner: resolveOwner(),
        name: params.name,
        task: params.task,
        cwd: params.cwd != null && params.cwd.trim() !== "" ? params.cwd : ctx.cwd,
        model: params.model,
        parentSession: ctx.sessionManager.getSessionFile(),
      };
      let task: Task;
      try {
        task = await rt.client.create(input);
      } catch (error) {
        throw new Error(
          `RemoteAgent spawn failed: ${errorMessage(error)} (the server auto-starts on session_start; check /remote-agents)`,
        );
      }
      subscribeTask(pi, rt, task.id);
      void widgetTick(rt, ctx);
      return toolText(
        `Spawned remote agent "${task.name}" (id: ${task.id}, status: ${task.status}, surface: ${task.surface}). ` +
          "Its result will be delivered automatically as a message — no polling needed.",
        { taskId: task.id, status: task.status },
      );
    },
  };
}

function listTool(): PiToolDef<typeof EmptyParams> {
  return {
    name: "RemoteAgentList",
    label: "RemoteAgentList",
    description: "List remote agents for this owner: id, name, status, elapsed time.",
    parameters: EmptyParams,
    async execute() {
      const rt = ensureRuntime();
      try {
        const tasks = await rt.client.list(resolveOwner());
        if (tasks.length === 0) return toolText("No remote agents.");
        const lines = tasks.map((task) => `- ${describeTask(task)}`);
        return toolText(`${tasks.length} remote agent(s):\n${lines.join("\n")}`);
      } catch (error) {
        return toolText(`remote-agents: cannot list agents: ${errorMessage(error)}`);
      }
    },
  };
}

function getTool(): PiToolDef<typeof IdParams> {
  return {
    name: "RemoteAgentGet",
    label: "RemoteAgentGet",
    description: "Inspect one remote agent: status, elapsed time, result/error, session file, recent events.",
    parameters: IdParams,
    async execute(_toolCallId, params) {
      const rt = ensureRuntime();
      try {
        const task = await rt.client.get(params.id);
        const lines = [
          describeTask(task),
          `cwd: ${task.cwd}${task.model != null ? ` · model: ${task.model}` : ""}`,
        ];
        if (task.sessionFile != null) lines.push(`session: ${task.sessionFile}`);
        if (task.result != null && task.result.trim() !== "") lines.push(`result: ${task.result}`);
        if (task.error != null && task.error.trim() !== "") lines.push(`error: ${task.error}`);
        for (const event of task.recentEvents ?? []) lines.push(`  ${formatEventLine(event)}`);
        return toolText(lines.join("\n"));
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) {
          throw new Error(`Remote agent ${params.id} not found.`);
        }
        return toolText(`remote-agents: cannot read ${params.id}: ${errorMessage(error)}`);
      }
    },
  };
}

function steerTool(): PiToolDef<typeof SteerParams> {
  return {
    name: "RemoteAgentSteer",
    label: "RemoteAgentSteer",
    description: "Send a steering message to a queued/running remote agent.",
    parameters: SteerParams,
    async execute(_toolCallId, params) {
      const rt = ensureRuntime();
      try {
        const task = await rt.client.steer(params.id, params.message);
        return toolText(`Steered "${task.name}" (${params.id}): ${params.message}`);
      } catch (error) {
        if (error instanceof ApiError && (error.status === 404 || error.status === 409)) {
          throw new Error(`remote-agents steer ${params.id}: ${error.message}`);
        }
        return toolText(`remote-agents: cannot steer ${params.id}: ${errorMessage(error)}`);
      }
    },
  };
}

function killTool(): PiToolDef<typeof IdParams> {
  return {
    name: "RemoteAgentKill",
    label: "RemoteAgentKill",
    description: "Kill (cancel) a remote agent. The worker is aborted and the task marked canceled.",
    parameters: IdParams,
    async execute(_toolCallId, params) {
      const rt = ensureRuntime();
      try {
        const task = await rt.client.cancel(params.id);
        return toolText(`Kill requested for "${task.name}" (${params.id}); it will be canceled shortly.`);
      } catch (error) {
        if (error instanceof ApiError && (error.status === 404 || error.status === 409)) {
          throw new Error(`remote-agents kill ${params.id}: ${error.message}`);
        }
        return toolText(`remote-agents: cannot kill ${params.id}: ${errorMessage(error)}`);
      }
    },
  };
}

// ---------------------------------------------------------------------------
// /remote-agents command
// ---------------------------------------------------------------------------

function registerCommand(pi: PiExtensionAPI): void {
  pi.registerCommand("remote-agents", {
    description: "Manage remote agents: /remote-agents [steer <id> <message> | kill <id> | logs <id> | attach <id>]",
    handler: (args, ctx) => handleAgentsCommand(pi, args, ctx),
  });
}

async function handleAgentsCommand(pi: PiExtensionAPI, args: string, ctx: PiContext): Promise<void> {
  const rt = ensureRuntime();
  const command = parseAgentsArgs(args);
  if (command.verb === "error") {
    ctx.ui.notify(`remote-agents: ${command.message}`, "warning");
    return;
  }
  if (command.verb === "list") {
    try {
      const tasks = await rt.client.list(resolveOwner());
      if (tasks.length === 0) {
        ctx.ui.notify("No remote agents.", "info");
        return;
      }
      ctx.ui.notify(tasks.map((task) => describeTask(task)).join("\n"), "info");
    } catch (error) {
      ctx.ui.notify(`remote-agents: ${errorMessage(error)}`, "error");
    }
    return;
  }
  if (command.verb === "steer") {
    try {
      await rt.client.steer(command.id, command.message);
      ctx.ui.notify(`Steered ${command.id}.`, "info");
    } catch (error) {
      ctx.ui.notify(`remote-agents: steer failed: ${errorMessage(error)}`, "error");
    }
    return;
  }
  if (command.verb === "kill") {
    try {
      await rt.client.cancel(command.id);
      ctx.ui.notify(`Kill requested for ${command.id}.`, "info");
    } catch (error) {
      ctx.ui.notify(`remote-agents: kill failed: ${errorMessage(error)}`, "error");
    }
    return;
  }
  if (command.verb === "logs") {
    try {
      const task = await rt.client.get(command.id);
      const seen = new Set<string>();
      const lines: string[] = [];
      let terminal = isTerminalStatus(task.status);
      if (!terminal) {
        // Live tail: replay recent history, then follow until terminal or the
        // follow window closes. Reconnects are handled inside client.stream.
        const deadline = Date.now() + 60_000;
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), Math.max(1, deadline - Date.now()));
        timer.unref?.();
        try {
          await rt.client.stream(
            command.id,
            0,
            (frame) => {
              const key = `${frame.seq}`;
              if (seen.has(key)) return;
              seen.add(key);
              lines.push(formatEventLine({ type: frame.type, data: frame.data } as never));
              if (frame.type === "state_changed" && fieldOf(frame.data, "to") !== undefined && ["completed", "failed", "canceled"].includes(String(fieldOf(frame.data, "to")))) {
                controller.abort();
              }
            },
            controller.signal,
          );
          terminal = true;
        } catch {
          // Follow window closed or connection dropped: show what we have.
        } finally {
          clearTimeout(timer);
        }
      } else {
        for (const event of task.recentEvents ?? []) {
          lines.push(formatEventLine(event));
        }
      }
      if (lines.length === 0) {
        ctx.ui.notify(`No events for ${command.id}.`, "info");
        return;
      }
      const suffix = terminal ? "" : "\n(still running — /remote-agents logs <id> to follow again)";
      ctx.ui.notify(lines.slice(-40).join("\n") + suffix, "info");
    } catch (error) {
      ctx.ui.notify(`remote-agents: ${errorMessage(error)}`, "error");
    }
    return;
  }
  // attach: bind this session to the task so its outcome lands here.
  try {
    const task = await rt.client.get(command.id);
    if (isTerminalStatus(task.status)) {
      void deliverAndAck(pi, rt, task.id);
      ctx.ui.notify(
        task.acknowledged || rt.delivered.has(task.id)
          ? `${task.id} already delivered/acknowledged.`
          : `Attached ${task.id} — delivering its result.`,
        "info",
      );
    } else {
      subscribeTask(pi, rt, task.id);
      ctx.ui.notify(`Attached ${task.id} — its result will be delivered here.`, "info");
    }
  } catch (error) {
    ctx.ui.notify(`remote-agents: ${errorMessage(error)}`, "error");
  }
}

// ---------------------------------------------------------------------------
// Custom message renderer
// ---------------------------------------------------------------------------

/**
 * Renderer decision (contract §5): omp's Component is just a `render(width)`
 * interface, so this returns a minimal theme-agnostic boxed component object
 * instead of pulling in TUI classes — safe in every mode and theme.
 */
function registerRenderer(pi: PiExtensionAPI): void {
  pi.registerMessageRenderer(CUSTOM_TYPE, (message) => renderResultBox(message));
}

function renderResultBox(message: PiMessage): PiComponent {
  const raw = typeof message.content === "string" ? message.content : "(non-text result content)";
  const text = raw.length > RESULT_RENDER_LIMIT ? `${raw.slice(0, RESULT_RENDER_LIMIT)}…` : raw;
  const name = fieldOf(message.details, "name");
  const status = fieldOf(message.details, "status");
  const elapsedMs = fieldOf(message.details, "elapsedMs");
  const elapsed = typeof elapsedMs === "number" ? ` · ${formatElapsed(elapsedMs)}` : "";
  const title = ` remote agent: ${typeof name === "string" ? name : "unknown"} · ${
    typeof status === "string" ? status : "?"
  }${elapsed} `;
  return {
    render(width: number): string[] {
      const inner = Math.max(width - 4, 24);
      const head = title.length > inner ? `${title.slice(0, inner - 1)}…` : title;
      const top = `╭─${head.padEnd(inner, "─")}─╮`;
      const rows = text.split("\n").map((line) => {
        const clipped = line.length > inner ? `${line.slice(0, inner - 1)}…` : line;
        return `│ ${clipped.padEnd(inner)} │`;
      });
      return [top, ...rows, `╰─${"─".repeat(inner)}─╯`];
    },
  };
}

// ---------------------------------------------------------------------------
// Resolution helpers
// ---------------------------------------------------------------------------

function resolveOwner(): string {
  return process.env[ENV.owner] ?? process.env.USER ?? "local";
}

function resolveServerEntry(): string {
  const fromEnv = process.env[ENV.serverEntry];
  if (fromEnv != null && fromEnv !== "") return fromEnv;
  // packages/extension/src/index.ts -> packages/server/src/main.ts
  return join(dirname(Bun.fileURLToPath(import.meta.url)), "..", "..", "server", "src", "main.ts");
}

function resolveStateDir(): string {
  const fromEnv = process.env[ENV.stateDir];
  return fromEnv != null && fromEnv !== "" ? fromEnv : join(homeDir(), ".remote-agents");
}

/** Home directory without node:os (contract limits builtins to node:path/node:fs). */
function homeDir(): string {
  return process.env.HOME ?? process.env.USERPROFILE ?? ".";
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export type { PiContext, PiExtensionAPI };
