// Shared fixtures for extension tests: a Bun.serve fixture implementing the
// remote-agents API surface (contract §4 paths), mock pi/ctx objects, and
// small polling helpers. No real omp is ever spawned.

import {
  ENV,
  isActiveStatus,
  isTerminalStatus,
  type CreateTaskInput,
  type SseFrame,
  type SurfaceKind,
  type Task,
  type TaskEvent,
  type TaskEventData,
  type TaskEventType,
  type TaskStatus,
} from "@remote-agents/protocol";
import { fieldOf } from "../src/deliver.ts";
import { resetRuntimeForTests } from "../src/runtime.ts";

/** Explicit union guard for fixture event payloads (protocol union is not runtime-checkable). */
function isTaskStatus(value: string): value is TaskStatus {
  return (
    value === "queued" ||
    value === "starting" ||
    value === "running" ||
    value === "finalizing" ||
    value === "completed" ||
    value === "failed" ||
    value === "canceled"
  );
}

export interface FixtureTask extends Task {
  events: TaskEvent[];
}

export interface FixtureRequest {
  method: string;
  path: string;
}

export interface FixtureServer {
  url: string;
  port: number;
  requests: FixtureRequest[];
  steerMessages: Array<{ id: string; message: string }>;
  tasks: Map<string, FixtureTask>;
  seedTask(partial: Partial<Task> & { id: string }): FixtureTask;
  pushEvent(taskId: string, type: TaskEventType, data: TaskEventData): TaskEvent;
  setStreamBehavior(taskId: string, behavior: "hold" | "close"): void;
  close(): Promise<void>;
}

interface StreamWriter {
  write(frame: SseFrame): void;
}

export async function createFixtureServer(): Promise<FixtureServer> {
  const tasks = new Map<string, FixtureTask>();
  const requests: FixtureRequest[] = [];
  const steerMessages: Array<{ id: string; message: string }> = [];
  const behavior = new Map<string, "hold" | "close">();
  const writers = new Map<string, Set<StreamWriter>>();
  const encoder = new TextEncoder();
  let autoId = 0;

  function taskDetail(task: FixtureTask): Task {
    const { events, ...rest } = task;
    void events;
    return { ...rest, recentEvents: task.events.slice(-10) };
  }

  function appendEvent(task: FixtureTask, type: TaskEventType, data: TaskEventData): TaskEvent {
    const event: TaskEvent = { seq: task.events.length + 1, taskId: task.id, type, data, ts: Date.now() };
    task.events.push(event);
    for (const writer of writers.get(task.id) ?? []) writer.write(event);
    return event;
  }

  function seedTask(partial: Partial<Task> & { id: string }): FixtureTask {
    const task: FixtureTask = {
      id: partial.id,
      owner: partial.owner ?? "local",
      name: partial.name ?? partial.id,
      task: partial.task ?? "do the thing",
      cwd: partial.cwd ?? "/tmp/work",
      status: partial.status ?? "running",
      surface: partial.surface ?? "process",
      acknowledged: partial.acknowledged ?? false,
      createdAt: partial.createdAt ?? Date.now() - 60_000,
      events: [],
    };
    if (partial.model !== undefined) task.model = partial.model;
    if (partial.startedAt !== undefined) task.startedAt = partial.startedAt;
    if (partial.endedAt !== undefined) task.endedAt = partial.endedAt;
    if (partial.result !== undefined) task.result = partial.result;
    if (partial.error !== undefined) task.error = partial.error;
    if (partial.sessionFile !== undefined) task.sessionFile = partial.sessionFile;
    if (partial.parentSession !== undefined) task.parentSession = partial.parentSession;
    if (partial.stopRequested !== undefined) task.stopRequested = partial.stopRequested;
    tasks.set(task.id, task);
    return task;
  }

  function pushEvent(taskId: string, type: TaskEventType, data: TaskEventData): TaskEvent {
    const task = tasks.get(taskId);
    if (task == null) throw new Error(`fixture: unknown task ${taskId}`);
    const event = appendEvent(task, type, data);
    if (type === "state_changed") {
      const to = fieldOf(data, "to");
      if (typeof to === "string" && isTaskStatus(to)) {
        task.status = to;
        if (isTerminalStatus(to) && task.endedAt == null) task.endedAt = Date.now();
      }
    }
    if (type === "result") {
      const text = fieldOf(data, "text");
      if (typeof text === "string") task.result = text;
    }
    if (type === "error") {
      const message = fieldOf(data, "message");
      if (typeof message === "string") task.error = message;
    }
    return event;
  }

  function json(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
  }

  function sseResponse(taskId: string, task: FixtureTask, since: number): Response {
    let open = true;
    let controllerRef: ReadableStreamDefaultController<Uint8Array> | undefined;
    const writer: StreamWriter = {
      write(frame: SseFrame): void {
        if (!open || controllerRef == null) return;
        try {
          controllerRef.enqueue(encoder.encode(`data: ${JSON.stringify(frame)}\n\n`));
        } catch {
          open = false;
        }
      },
    };
    const stream = new ReadableStream<Uint8Array>({
      start(controller: ReadableStreamDefaultController<Uint8Array>): void {
        controllerRef = controller;
        const set = writers.get(taskId) ?? new Set<StreamWriter>();
        set.add(writer);
        writers.set(taskId, set);
        for (const event of task.events) {
          if (event.seq > since) writer.write(event);
        }
        if (behavior.get(taskId) === "close") {
          open = false;
          set.delete(writer);
          controller.close();
        }
      },
      cancel(): void {
        open = false;
        const set = writers.get(taskId);
        set?.delete(writer);
      },
    });
    return new Response(stream, { headers: { "content-type": "text/event-stream" } });
  }

  async function route(req: Request, url: URL): Promise<Response> {
    const path = url.pathname;
    if (path === "/health") {
      return json({
        ok: true,
        version: "fixture",
        surface: "process",
        uptimeMs: 1,
        tasks: {
          active: [...tasks.values()].filter((task) => isActiveStatus(task.status)).length,
          total: tasks.size,
        },
      });
    }
    if (path === "/tasks" && req.method === "POST") {
      const input = (await req.json()) as CreateTaskInput;
      autoId += 1;
      const id = `fx${autoId}`;
      seedTask({
        id,
        owner: input.owner,
        name: input.name,
        task: input.task,
        cwd: input.cwd,
        model: input.model,
        parentSession: input.parentSession,
        status: "queued",
      });
      const created = tasks.get(id) as FixtureTask;
      return json(taskDetail(created), 201);
    }
    if (path === "/tasks" && req.method === "GET") {
      const owner = url.searchParams.get("owner");
      const listed = [...tasks.values()]
        .filter((task) => owner == null || task.owner === owner)
        .map(taskDetail);
      return json(listed);
    }
    if (path === "/attach" && req.method === "POST") {
      const owner = fieldOf(await req.json(), "owner");
      const attached = [...tasks.values()]
        .filter((task) => typeof owner !== "string" || task.owner === owner)
        .map(taskDetail);
      return json({
        tasks: attached,
        server: { uptimeMs: 1, surface: "process", version: "fixture" },
      });
    }
    const taskMatch = /^\/tasks\/([^/]+)$/.exec(path);
    if (taskMatch?.[1] != null && req.method === "GET") {
      const task = tasks.get(decodeURIComponent(taskMatch[1]));
      if (task == null) return json({ error: `task ${taskMatch[1]} not found` }, 404);
      return json(taskDetail(task));
    }
    const steerMatch = /^\/tasks\/([^/]+)\/steer$/.exec(path);
    if (steerMatch?.[1] != null && req.method === "POST") {
      const id = decodeURIComponent(steerMatch[1]);
      const task = tasks.get(id);
      if (task == null) return json({ error: `task ${id} not found` }, 404);
      if (!isActiveStatus(task.status)) {
        return json({ error: `task ${id} is not steerable (status ${task.status})` }, 409);
      }
      const message = fieldOf(await req.json(), "message");
      steerMessages.push({ id, message: typeof message === "string" ? message : "" });
      return json(taskDetail(task));
    }
    const cancelMatch = /^\/tasks\/([^/]+)\/cancel$/.exec(path);
    if (cancelMatch?.[1] != null && req.method === "POST") {
      const id = decodeURIComponent(cancelMatch[1]);
      const task = tasks.get(id);
      if (task == null) return json({ error: `task ${id} not found` }, 404);
      const from = task.status;
      task.status = "canceled";
      task.stopRequested = true;
      if (task.endedAt == null) task.endedAt = Date.now();
      appendEvent(task, "state_changed", { from, to: "canceled" });
      return json(taskDetail(task));
    }
    const ackMatch = /^\/tasks\/([^/]+)\/ack$/.exec(path);
    if (ackMatch?.[1] != null && req.method === "POST") {
      const id = decodeURIComponent(ackMatch[1]);
      const task = tasks.get(id);
      if (task == null) return json({ error: `task ${id} not found` }, 404);
      if (!isTerminalStatus(task.status)) {
        return json({ error: `task ${id} is not terminal (status ${task.status})` }, 409);
      }
      task.acknowledged = true;
      return json(taskDetail(task));
    }
    const streamMatch = /^\/tasks\/([^/]+)\/stream$/.exec(path);
    if (streamMatch?.[1] != null && req.method === "GET") {
      const id = decodeURIComponent(streamMatch[1]);
      const task = tasks.get(id);
      if (task == null) return json({ error: `task ${id} not found` }, 404);
      const since = Number(url.searchParams.get("since") ?? "0");
      return sseResponse(id, task, Number.isFinite(since) ? since : 0);
    }
    return json({ error: `no route: ${req.method} ${path}` }, 404);
  }

  const server = Bun.serve({
    port: 0,
    async fetch(req): Promise<Response> {
      const url = new URL(req.url);
      requests.push({ method: req.method, path: `${url.pathname}${url.search}` });
      return route(req, url);
    },
  });

  const port = server.port;
  if (port == null) throw new Error("fixture: Bun.serve returned no port");
  return {
    url: `http://127.0.0.1:${port}`,
    port,
    requests,
    steerMessages,
    tasks,
    seedTask,
    pushEvent,
    setStreamBehavior(taskId: string, mode: "hold" | "close"): void {
      behavior.set(taskId, mode);
    },
    close(): Promise<void> {
      return Promise.resolve(server.stop(true));
    },
  };
}

export function useFixture(server: FixtureServer): void {
  process.env[ENV.url] = server.url;
  process.env[ENV.owner] = "local";
  delete process.env[ENV.serverEntry];
  resetRuntimeForTests();
}

// ---------------------------------------------------------------------------
// mock omp objects
// ---------------------------------------------------------------------------

export interface SentMessage {
  message: { customType: string; content: unknown; display: boolean; details?: unknown };
  options?: { triggerTurn?: boolean; deliverAs?: string };
}

export interface CapturedTool {
  name: string;
  description: string;
  parameters: unknown;
  execute: (...args: unknown[]) => Promise<unknown>;
}

export interface ToolCallResult {
  content: Array<{ type: string; text: string }>;
  details?: unknown;
}

/** Boxed component returned by the extension's message renderer. */
export interface RenderedComponent {
  render(width: number): string[];
}

export interface MockCommand {
  description?: string;
  handler: (args: string, ctx: unknown) => Promise<void> | void;
}

export type MockRenderer = (message: unknown, options: unknown, theme: unknown) => unknown;
export type MockHandler = (event: unknown, ctx: unknown) => unknown;

export interface MockPi {
  pi: {
    on(event: string, handler: MockHandler): void;
    registerTool(tool: CapturedTool): void;
    registerCommand(name: string, options: MockCommand): void;
    registerMessageRenderer(customType: string, renderer: MockRenderer): void;
    sendMessage(message: SentMessage["message"], options?: SentMessage["options"]): void;
  };
  sent: SentMessage[];
  tools: Map<string, CapturedTool>;
  commands: Map<string, MockCommand>;
  renderers: Map<string, MockRenderer>;
  handlers: Map<string, MockHandler>;
}

export function createMockPi(): MockPi {
  const sent: SentMessage[] = [];
  const tools = new Map<string, CapturedTool>();
  const commands = new Map<string, MockCommand>();
  const renderers = new Map<string, MockRenderer>();
  const handlers = new Map<string, MockHandler>();
  const pi: MockPi["pi"] = {
    on(event: string, handler: MockHandler): void {
      handlers.set(event, handler);
    },
    registerTool(tool: CapturedTool): void {
      tools.set(tool.name, tool);
    },
    registerCommand(name: string, options: MockCommand): void {
      commands.set(name, options);
    },
    registerMessageRenderer(customType: string, renderer: MockRenderer): void {
      renderers.set(customType, renderer);
    },
    sendMessage(message: SentMessage["message"], options?: SentMessage["options"]): void {
      sent.push({ message, options });
    },
  };
  return { pi, sent, tools, commands, renderers, handlers };
}

export interface MockCtx {
  ctx: {
    ui: {
      setWidget(key: string, content: string[] | undefined, options?: { placement?: string }): void;
      notify(message: string, level?: "info" | "warning" | "error"): void;
    };
    mode: "tui";
    hasUI: boolean;
    cwd: string;
    sessionManager: { getSessionFile(): string | undefined };
  };
  widgets: Map<string,
    string[] | ((tui: unknown, theme: unknown) => { render(width: number): string[] }) | undefined
  >;
  notifications: Array<{ message: string; level: "info" | "warning" | "error" | undefined }>;
}

export function createMockCtx(opts?: {
  hasUI?: boolean;
  sessionFile?: string | undefined;
  cwd?: string;
}): MockCtx {
  const widgets = new Map<
    string,
    string[] | ((tui: unknown, theme: unknown) => { render(width: number): string[] }) | undefined
  >();
  const notifications: Array<{ message: string; level: "info" | "warning" | "error" | undefined }> = [];
  const sessionFile = opts?.sessionFile !== undefined ? opts.sessionFile : "/tmp/parent/session.jsonl";
  const ctx: MockCtx["ctx"] = {
    ui: {
      setWidget(
        key: string,
        content:
          | string[]
          | ((tui: unknown, theme: unknown) => { render(width: number): string[] })
          | undefined,
      ): void {
        widgets.set(key, content);
      },
      notify(message: string, level?: "info" | "warning" | "error"): void {
        notifications.push({ message, level });
      },
    },
    mode: "tui",
    hasUI: opts?.hasUI ?? true,
    cwd: opts?.cwd ?? "/tmp/parent",
    sessionManager: {
      getSessionFile(): string | undefined {
        return sessionFile;
      },
    },
  };
  return { ctx, widgets, notifications };
}

/** Invoke a captured tool the way omp would. */
export async function runTool(
  tool: CapturedTool,
  params: Record<string, unknown>,
  ctx: MockCtx,
): Promise<ToolCallResult> {
  const execute = tool.execute as unknown as (
    toolCallId: string,
    params: Record<string, unknown>,
    signal: AbortSignal | undefined,
    onUpdate: unknown,
    ctx: unknown,
  ) => Promise<ToolCallResult>;
  return execute("call-1", params, undefined, undefined, ctx.ctx);
}

/** Poll until the check passes, with a bounded budget. */
export async function waitFor(check: () => boolean, timeoutMs = 2_000, stepMs = 10): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (check()) return;
    if (Date.now() >= deadline) throw new Error("waitFor: condition not met in time");
    await Bun.sleep(stepMs);
  }
}

/** Fire the captured session_start handler and await its reconcile. */
export async function startSession(mock: MockPi, ctx: MockCtx): Promise<void> {
  const handler = mock.handlers.get("session_start");
  if (handler == null) throw new Error("session_start handler not registered");
  await handler({ type: "session_start", reason: "startup" }, ctx.ctx);
}

export type { SurfaceKind, Task, TaskEvent, TaskStatus };

/** Resolve a widget payload (plain or factory) and strip ANSI styling. */
export function resolveWidget(
  content: string[] | ((tui: unknown, theme: unknown) => { render(width: number): string[] }) | undefined,
  width = 80,
): string[] {
  const raw = typeof content === "function" ? content(undefined, undefined).render(width) : (content ?? []);
  return raw.map((line) => line.replace(/\x1b\[[0-9;]*m/g, ""));
}
