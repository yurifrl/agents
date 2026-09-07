// Extension-side HTTP client for the remote-agents server, the detached server
// bootstrapper, and the manual SSE parser. Imports only @remote-agents/protocol
// (contract §5); zero other runtime deps.

import {
  API,
  DEFAULT_PORT,
  ENV,
  LOGS_DIRNAME,
  type AttachResponse,
  type CreateTaskInput,
  type HealthResponse,
  type SseFrame,
  type Task,
} from "@remote-agents/protocol";
import { closeSync, mkdirSync, openSync } from "node:fs";
import { join } from "node:path";

/** A request reached the server but answered with an error status. */
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

/** The server could not be reached or started within the ensureServer budget. */
export class ServerUnreachable extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ServerUnreachable";
  }
}

export interface RemoteAgentsClient {
  readonly baseUrl: string;
  health(timeoutMs?: number): Promise<HealthResponse>;
  create(input: CreateTaskInput): Promise<Task>;
  list(owner: string): Promise<Task[]>;
  get(id: string): Promise<Task>;
  steer(id: string, message: string): Promise<Task>;
  cancel(id: string): Promise<Task>;
  ack(id: string): Promise<Task>;
  attach(owner: string): Promise<AttachResponse>;
  /** Long-lived SSE subscription; resolves when aborted or the task disappears. */
  stream(id: string, sinceSeq: number, onEvent: (frame: SseFrame) => void, signal?: AbortSignal): Promise<void>;
}

export interface EnsureServerOptions {
  /** Server entry script (packages/server/src/main.ts). */
  serverEntry: string;
  /** State dir; server logs are appended to <stateDir>/logs/server.log. */
  stateDir: string;
}

/** Base URL from an explicit value, else REMOTE_AGENTS_URL, else localhost:8679. */
export function resolveBaseUrl(env?: string): string {
  const raw = env ?? process.env[ENV.url];
  const base = raw != null && raw.trim() !== "" ? raw : `http://127.0.0.1:${DEFAULT_PORT}`;
  return base.replace(/\/+$/, "");
}

export function createClient(baseUrl: string = resolveBaseUrl()): RemoteAgentsClient {
  const root = baseUrl.replace(/\/+$/, "");

  async function request<T>(
    path: string,
    init?: { method?: string; body?: unknown; timeoutMs?: number },
  ): Promise<T> {
    const hasBody = init?.body !== undefined;
    const res = await fetch(`${root}${path}`, {
      method: init?.method ?? "GET",
      headers: hasBody ? { "content-type": "application/json" } : undefined,
      body: hasBody ? JSON.stringify(init?.body) : undefined,
      signal: AbortSignal.timeout(init?.timeoutMs ?? 10_000),
    });
    const text = await res.text();
    if (!res.ok) {
      let message = `HTTP ${res.status}`;
      try {
        const parsed = JSON.parse(text) as { error?: string };
        if (parsed.error != null && parsed.error !== "") message = parsed.error;
      } catch {
        // keep the generic status message
      }
      throw new ApiError(res.status, message);
    }
    if (text === "") throw new ApiError(res.status, "empty response body");
    return JSON.parse(text) as T;
  }

  async function stream(
    id: string,
    sinceSeq: number,
    onEvent: (frame: SseFrame) => void,
    signal?: AbortSignal,
  ): Promise<void> {
    let resume = sinceSeq;
    let backoffMs = 500;
    for (;;) {
      if (signal?.aborted) return;
      let receivedFrame = false;
      try {
        const res = await fetch(`${root}${API.stream(id, resume)}`, {
          headers: { accept: "text/event-stream" },
          signal,
        });
        // Task gone (server restarted without it, task deleted): stop retrying.
        if (res.status === 404 || res.status === 410) return;
        if (!res.ok) throw new ApiError(res.status, `stream HTTP ${res.status}`);
        const body = res.body;
        if (body == null) return;
        const parser = createSseParser((frame) => {
          resume = frame.seq;
          receivedFrame = true;
          onEvent(frame);
        });
        const reader = body.getReader();
        const decoder = new TextDecoder();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          parser.push(decoder.decode(value, { stream: true }));
        }
        parser.push(decoder.decode());
        parser.end();
      } catch {
        if (signal?.aborted) return;
      }
      if (signal?.aborted) return;
      // Reset the backoff after a connection that delivered at least one frame;
      // grow it (capped at 5s) while the connection keeps failing early.
      if (!receivedFrame) backoffMs = Math.min(backoffMs * 2, 5_000);
      else backoffMs = 500;
      await sleepAbortable(backoffMs, signal);
    }
  }
  return {
    baseUrl: root,
    health: (timeoutMs) => request<HealthResponse>(API.health, { timeoutMs }),
    create: (input) => request<Task>(API.tasks, { method: "POST", body: input }),
    list: (owner) => request<Task[]>(`${API.tasks}?owner=${encodeURIComponent(owner)}`),
    get: (id) => request<Task>(API.task(id)),
    steer: (id, message) => request<Task>(API.steer(id), { method: "POST", body: { message } }),
    cancel: (id) => request<Task>(API.cancel(id), { method: "POST" }),
    ack: (id) => request<Task>(API.ack(id), { method: "POST" }),
    attach: (owner) => request<AttachResponse>(API.attach, { method: "POST", body: { owner } }),
    stream,
  };
}

function sleepAbortable(ms: number, signal?: AbortSignal): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  if (signal?.aborted) return Promise.resolve();
  const timer = setTimeout(() => {
    signal?.removeEventListener("abort", onAbort);
    resolve();
  }, ms);
  const onAbort = (): void => {
    clearTimeout(timer);
    resolve();
  };
  signal?.addEventListener("abort", onAbort, { once: true });
  return promise;
}

/**
 * Make sure a server is listening at the client's URL: health-check first
 * (1s timeout); when it does not answer, spawn `bun <serverEntry>` detached
 * with logs appended to <stateDir>/logs/server.log, then poll health for up
 * to 5s. Never spawns when the health check already passes.
 */

export async function ensureServer(client: RemoteAgentsClient, opts: EnsureServerOptions): Promise<void> {
  if (await isHealthy(client, 1_000)) return;

  const logsDir = join(opts.stateDir, LOGS_DIRNAME);
  const logPath = join(logsDir, "server.log");
  mkdirSync(logsDir, { recursive: true });
  const logFd = openSync(logPath, "a");
  try {
    const child = Bun.spawn([process.execPath, opts.serverEntry], {
      stdin: "ignore",
      stdout: logFd,
      stderr: logFd,
      env: process.env,
      detached: true,
    });
    child.unref();
  } finally {
    closeSync(logFd);
  }

  const deadline = Date.now() + 5_000;
  for (;;) {
    const tick = Promise.withResolvers<void>();
    setTimeout(tick.resolve, 250);
    await tick.promise;
    if (await isHealthy(client, 500)) return;
    if (Date.now() >= deadline) break;
  }
  throw new ServerUnreachable(
    `no remote-agents server at ${client.baseUrl} within 5s (spawned ${opts.serverEntry}; logs: ${logPath})`,
  );
}

async function isHealthy(client: RemoteAgentsClient, timeoutMs: number): Promise<boolean> {
  try {
    const health = await client.health(timeoutMs);
    return health.ok === true;
  } catch {
    return false;
  }
}

// ---------------------------------------------------------------------------
// SSE parsing
// ---------------------------------------------------------------------------

export interface SseParser {
  /** Feed the next chunk (may split frames / CRLFs at arbitrary points). */
  push(chunk: string): void;
  /** Flush any trailing complete frame that lacks its final blank line. */
  end(): void;
}

/**
 * Incremental `data:` frame parser. Frames are separated by a blank line
 * (`\n\n` or `\r\n\r\n`); `:`-prefixed lines are comments/heartbeats and are
 * ignored. Tolerates chunk boundaries anywhere, including mid-CRLF.
 */
export function createSseParser(onFrame: (frame: SseFrame) => void): SseParser {
  let buffer = "";

  function findFrameEnd(text: string): { end: number; next: number } | undefined {
    const lf = text.indexOf("\n\n");
    const crlf = text.indexOf("\r\n\r\n");
    if (crlf !== -1 && (lf === -1 || crlf < lf)) return { end: crlf, next: crlf + 4 };
    if (lf !== -1) return { end: lf, next: lf + 2 };
    return undefined;
  }

  function dispatch(raw: string): void {
    const dataLines: string[] = [];
    for (const line of raw.split("\n")) {
      const trimmed = line.endsWith("\r") ? line.slice(0, -1) : line;
      if (trimmed.startsWith(":")) continue; // comment / heartbeat
      if (trimmed.startsWith("data:")) {
        const value = trimmed.slice(5);
        dataLines.push(value.startsWith(" ") ? value.slice(1) : value);
      }
      // `event:` / `id:` / unknown fields are ignored.
    }
    if (dataLines.length === 0) return;
    const payload = dataLines.join("\n");
    try {
      onFrame(JSON.parse(payload) as SseFrame);
    } catch {
      // Tolerate non-JSON payloads (forward compatibility).
    }
  }

  function drain(final: boolean): void {
    for (;;) {
      const found = findFrameEnd(buffer);
      if (found == null) break;
      const raw = buffer.slice(0, found.end);
      buffer = buffer.slice(found.next);
      dispatch(raw);
    }
    if (final && buffer.trim() !== "") {
      const raw = buffer;
      buffer = "";
      dispatch(raw);
    }
  }

  return {
    push(chunk: string): void {
      buffer += chunk;
      drain(false);
    },
    end(): void {
      drain(true);
    },
  };
}
