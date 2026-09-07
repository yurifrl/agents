import {
  API,
  isActiveStatus,
  isTerminalStatus,
  type AttachResponse,
  type HealthResponse,
  type SseFrame,
  type TaskEvent,
} from "@remote-agents/protocol";
import path from "node:path";
import { SERVER_VERSION, type Config } from "./config";
import { badRequest, HttpError, notFound } from "./errors";
import type { Supervisor } from "./supervisor";
import type { TaskStore } from "./store";

export interface HttpOptions {
  config: Config;
  store: TaskStore;
  supervisor: Supervisor;
  /** SSE heartbeat comment interval; default 15s per contract. */
  heartbeatMs?: number;
}

const json = (data: unknown, status = 200): Response => Response.json(data, { status });

async function readJsonObject(req: Request): Promise<Record<string, unknown>> {
  let body: unknown;
  try {
    body = await req.json();
  } catch {
    throw badRequest("request body must be valid JSON");
  }
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    throw badRequest("request body must be a JSON object");
  }
  return body as Record<string, unknown>;
}

function requiredString(body: Record<string, unknown>, key: string): string {
  const value = body[key];
  if (typeof value !== "string" || value.trim().length === 0) {
    throw badRequest(`${key} must be a non-empty string`);
  }
  return value;
}

function optionalString(body: Record<string, unknown>, key: string): string | undefined {
  const value = body[key];
  if (value === undefined) return undefined;
  if (typeof value !== "string") throw badRequest(`${key} must be a string`);
  return value;
}

export function createHttpServer(options: HttpOptions): Bun.Server<undefined> {
  const { config, store, supervisor } = options;
  const heartbeatMs = options.heartbeatMs ?? 15_000;
  const bootedAt = Date.now();

  const health = (): Response => {
    const tasks = store.all();
    const body: HealthResponse = {
      ok: true,
      version: SERVER_VERSION,
      surface: supervisor.kind,
      uptimeMs: Date.now() - bootedAt,
      tasks: {
        active: tasks.filter((t) => isActiveStatus(t.status)).length,
        total: tasks.length,
      },
    };
    return json(body);
  };

  const createTask = async (req: Request): Promise<Response> => {
    const body = await readJsonObject(req);
    const owner = requiredString(body, "owner");
    const name = requiredString(body, "name");
    const task = requiredString(body, "task");
    const cwd = requiredString(body, "cwd");
    if (!path.isAbsolute(cwd)) throw badRequest("cwd must be an absolute path");
    const model = optionalString(body, "model");
    const parentSession = optionalString(body, "parentSession");
    const created = supervisor.create({ owner, name, task, cwd, model, parentSession });
    return json(created, 201);
  };

  const listTasks = (url: URL): Response => {
    const owner = url.searchParams.get("owner") ?? undefined;
    const unack = url.searchParams.get("unacknowledged");
    const unacknowledged = unack === "1" || unack === "true";
    return json(store.list({ owner, unacknowledged }));
  };

  const getTask = (id: string): Response => {
    const task = store.get(id);
    if (!task) throw notFound(`task not found: ${id}`);
    // Contract addition: detail responses embed the last 10 events so the
    // extension can print `/agents logs <id>` without a separate endpoint.
    return json({ ...task, recentEvents: store.recentEvents(id) });
  };

  const steer = async (id: string, req: Request): Promise<Response> => {
    const body = await readJsonObject(req);
    const message = requiredString(body, "message");
    const task = await supervisor.steer(id, message);
    return json(task, 202);
  };

  const cancelTask = async (id: string): Promise<Response> => json(await supervisor.cancel(id));

  const ackTask = (id: string): Response => json(supervisor.ack(id));

  const attach = async (req: Request): Promise<Response> => {
    const body = await readJsonObject(req);
    const owner = requiredString(body, "owner");
    const payload: AttachResponse = {
      tasks: store.list({ owner, unacknowledged: true }),
      server: {
        uptimeMs: Date.now() - bootedAt,
        surface: supervisor.kind,
        version: SERVER_VERSION,
      },
    };
    return json(payload);
  };

  const stream = (id: string, url: URL): Response => {
    const task = store.get(id);
    if (!task) throw notFound(`task not found: ${id}`);
    const sinceRaw = url.searchParams.get("since");
    const since = sinceRaw === null ? 0 : Number(sinceRaw);
    if (!Number.isInteger(since) || since < 0) throw badRequest("since must be a non-negative integer");
    const encoder = new TextEncoder();
    let cleanup: () => void = () => {};

    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        let closed = false;
        let heartbeat: Timer | undefined;
        let unsubscribe: (() => void) | undefined;
        const seen = new Set<number>();
        const enqueue = (text: string) => {
          if (!closed) controller.enqueue(encoder.encode(text));
        };
        const close = () => {
          if (closed) return;
          closed = true;
          clearInterval(heartbeat);
          unsubscribe?.();
          try {
            controller.close();
          } catch {
            // already closed
          }
        };
        const forward = (event: TaskEvent) => {
          if (closed || seen.has(event.seq)) return;
          seen.add(event.seq);
          const frame: SseFrame = {
            taskId: event.taskId,
            seq: event.seq,
            type: event.type,
            data: event.data,
            ts: event.ts,
          };
          enqueue(`data: ${JSON.stringify(frame)}\n\n`);
          if (event.type === "state_changed" && "to" in event.data && isTerminalStatus(event.data.to)) {
            close();
          }
        };
        // Subscribe before replaying so no event can slip between the two.
        unsubscribe = store.subscribe(id, forward);
        cleanup = () => {
          closed = true;
          clearInterval(heartbeat);
          unsubscribe?.();
        };
        for (const event of store.eventsSince(id, since)) forward(event);
        const current = store.get(id);
        if (current && isTerminalStatus(current.status)) {
          close();
        } else {
          heartbeat = setInterval(() => enqueue(": ping\n\n"), heartbeatMs);
          heartbeat.unref?.();
        }
      },
      cancel() {
        cleanup(); // client disconnect: release SSE resources only, never the worker
      },
    });

    return new Response(body, {
      headers: {
        "content-type": "text/event-stream",
        "cache-control": "no-cache, no-transform",
        connection: "keep-alive",
      },
    });
  };

  const route = async (req: Request): Promise<Response> => {
    const url = new URL(req.url);
    const pathname = url.pathname;
    const method = req.method;

    if (method === "GET" && pathname === API.health) return health();
    if (method === "POST" && pathname === API.attach) return attach(req);
    if (method === "POST" && pathname === API.tasks) return createTask(req);
    if (method === "GET" && pathname === API.tasks) return listTasks(url);

    const match = pathname.match(/^\/tasks\/([^/]+)(?:\/(steer|cancel|ack|stream))?$/);
    if (match) {
      const id = decodeURIComponent(match[1] ?? "");
      const action = match[2];
      if (action === undefined && method === "GET") return getTask(id);
      if (action === "stream" && method === "GET") return stream(id, url);
      if (method === "POST") {
        if (action === "steer") return steer(id, req);
        if (action === "cancel") return cancelTask(id);
        if (action === "ack") return ackTask(id);
      }
    }
    throw notFound(`no route: ${method} ${pathname}`);
  };

  return Bun.serve({
    port: config.port,
    idleTimeout: 255, // SSE streams are kept alive by heartbeats, not by Bun's idle timer
    async fetch(req): Promise<Response> {
      try {
        return await route(req);
      } catch (err) {
        if (err instanceof HttpError) return json({ error: err.message }, err.status);
        return json({ error: err instanceof Error ? err.message : String(err) }, 500);
      }
    },
  });
}
