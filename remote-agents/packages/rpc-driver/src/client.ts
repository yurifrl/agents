import type { Channel } from "@remote-agents/protocol";
import { createLineChannel } from "./framing";

/** The frame omp emits as its very first stdout line. */
export interface ReadyInfo {
  protocolVersion: number;
  sessionFile?: string;
}

/** Any non-response JSON frame received from the worker (forwarded verbatim). */
export type RpcEvent = { type: string } & Record<string, unknown>;

/** Response frame correlated with a pending request id. */
export interface RpcResponse {
  id: number;
  type: "response";
  success?: boolean;
  error?: string;
  [key: string]: unknown;
}

export type OmpRpcErrorCode =
  | "handshake_timeout"
  | "request_timeout"
  | "request_failed"
  | "channel_closed";

export class OmpRpcError extends Error {
  readonly code: OmpRpcErrorCode;

  constructor(code: OmpRpcErrorCode, message: string, options?: { cause?: unknown }) {
    super(message);
    this.name = "OmpRpcError";
    this.code = code;
    if (options?.cause !== undefined) this.cause = options.cause;
  }
}

const DEFAULT_HANDSHAKE_TIMEOUT_MS = 10_000;
const DEFAULT_REQUEST_TIMEOUT_MS = 30_000;

interface PendingRequest {
  resolve: (response: RpcResponse) => void;
  reject: (error: OmpRpcError) => void;
  timer?: Timer;
}

export interface OmpRpcOptions {
  /** Max wait for the `ready` frame in start(). Default 10s. */
  handshakeTimeoutMs?: number;
}

/**
 * Client for omp's line-delimited JSON RPC mode (see CONTRACT.md §2 and
 * docs/research/2026-09-07-omp-pi-core.md §4).
 *
 * The constructor immediately begins consuming `channel.readable`; the first
 * frame must be `type: "ready"`. Every later frame is either the response to
 * a pending request id or an event dispatched to onEvent listeners.
 */
export class OmpRpc {
  /** Resolves once the channel is closed for any reason (EOF, error, close()). */
  readonly channelClosed: Promise<void>;

  private readonly channel: Channel;
  private readonly writer: WritableStreamDefaultWriter<Uint8Array>;
  private readonly handshakeTimeoutMs: number;
  private readonly encoder = new TextEncoder();
  private readonly pending = new Map<number, PendingRequest>();
  private readonly eventCallbacks = new Set<(event: RpcEvent) => void>();
  private readonly closeCallbacks = new Set<() => void>();

  private nextId = 1;
  private started = false;
  private handshakeDone = false;
  private readyInfo: ReadyInfo | undefined;
  private finished = false;
  private closeError: OmpRpcError | undefined;
  private handshakeResolve: ((info: ReadyInfo) => void) | undefined;
  private handshakeReject: ((error: OmpRpcError) => void) | undefined;
  private resolveClosed: (() => void) | undefined;
  private rawHandle: { close(): void } | undefined;

  constructor(channel: Channel, opts?: OmpRpcOptions) {
    this.handshakeTimeoutMs = opts?.handshakeTimeoutMs ?? DEFAULT_HANDSHAKE_TIMEOUT_MS;
    this.channel = reportClosed(channel, () => this.handleClosed());
    this.writer = this.channel.writable.getWriter();
    const closed = Promise.withResolvers<void>();
    this.channelClosed = closed.promise;
    this.resolveClosed = closed.resolve;
    createLineChannel(this.channel, (line, raw) => {
      this.rawHandle ??= raw;
      this.handleLine(line);
    });
  }

  /**
   * Wait for the worker's `ready` frame. Must be called once. If the frame
   * already arrived (or the channel already closed) the promise settles
   * immediately. On timeout the channel is closed and OmpRpcError
   * ("handshake_timeout") is thrown.
   */
  start(): Promise<ReadyInfo> {
    if (this.started) throw new Error("OmpRpc.start() must be called exactly once");
    this.started = true;
    if (this.readyInfo) return Promise.resolve(this.readyInfo);
    if (this.finished) return Promise.reject(this.closureError());
    const handshake = Promise.withResolvers<ReadyInfo>();
    this.handshakeResolve = handshake.resolve;
    this.handshakeReject = handshake.reject;
    if (this.handshakeTimeoutMs > 0) {
      setTimeout(() => {
        if (this.handshakeDone || this.finished) return;
        const error = new OmpRpcError(
          "handshake_timeout",
          `no ready frame within ${this.handshakeTimeoutMs}ms`,
        );
        this.finish(error);
      }, this.handshakeTimeoutMs);
    }
    return handshake.promise;
  }

  /** Register an event listener; returns an unsubscribe function. */
  onEvent(cb: (event: RpcEvent) => void): () => void {
    this.eventCallbacks.add(cb);
    return () => {
      this.eventCallbacks.delete(cb);
    };
  }

  /** Register a channel-closed listener; returns an unsubscribe function. */
  onClose(cb: () => void): () => void {
    this.closeCallbacks.add(cb);
    return () => {
      this.closeCallbacks.delete(cb);
    };
  }

  /**
   * Send a method frame and await the response with the same id. Rejects with
   * OmpRpcError("request_failed") on `success: false`, ("request_timeout") on
   * timeout, ("channel_closed") when the channel dies first. `timeoutMs <= 0`
   * disables the timer.
   */
  request(
    method: { type: string } & Record<string, unknown>,
    timeoutMs: number = DEFAULT_REQUEST_TIMEOUT_MS,
  ): Promise<RpcResponse> {
    if (this.finished) return Promise.reject(this.closureError(`request ${method.type}`));
    const id = this.nextId++;
    const frame: Record<string, unknown> = { id, ...method };
    const { promise, resolve, reject } = Promise.withResolvers<RpcResponse>();
    const entry: PendingRequest = { resolve, reject };
    if (timeoutMs > 0) {
      entry.timer = setTimeout(() => {
        this.pending.delete(id);
        reject(
          new OmpRpcError(
            "request_timeout",
            `request ${method.type} (id ${id}) timed out after ${timeoutMs}ms`,
          ),
        );
      }, timeoutMs);
    }
    this.pending.set(id, entry);
    this.writer
      .write(this.encoder.encode(JSON.stringify(frame) + "\n"))
      .catch((cause) => {
        this.pending.delete(id);
        clearTimeout(entry.timer);
        reject(this.writeError(cause, method.type));
      });
    return promise;
  }

  prompt(message: string): Promise<RpcResponse> {
    return this.request({ type: "prompt", message });
  }

  steer(message: string): Promise<RpcResponse> {
    return this.request({ type: "steer", message });
  }

  abort(): Promise<RpcResponse> {
    return this.request({ type: "abort" });
  }

  getState(): Promise<RpcResponse> {
    return this.request({ type: "get_state" });
  }

  getLastAssistantText(): Promise<RpcResponse> {
    return this.request({ type: "get_last_assistant_text" });
  }

  setSessionName(name: string): Promise<RpcResponse> {
    return this.request({ type: "set_session_name", name });
  }

  /**
   * Close the channel. Pending requests reject with "channel_closed"; the
   * channelClosed promise and onClose listeners fire exactly once. Idempotent.
   */
  async close(): Promise<void> {
    this.finish(new OmpRpcError("channel_closed", "OmpRpc.close() called"));
    try {
      await this.channel.close();
    } catch {
      // Already closed by the underlying transport.
    }
  }

  private handleLine(line: string): void {
    const trimmed = line.trim();
    if (trimmed.length === 0) return;
    let frame: unknown;
    try {
      frame = JSON.parse(trimmed);
    } catch {
      return; // Not JSON — tolerate and ignore.
    }
    if (typeof frame !== "object" || frame === null || Array.isArray(frame)) return;
    const obj = frame as Record<string, unknown>;

    if (!this.handshakeDone) {
      // Pre-ready frames (extension UI chatter, late events) are ignored;
      // only the ready frame completes the handshake.
      const info = parseReady(obj);
      if (info) {
        this.readyInfo = info;
        this.handshakeDone = true;
        this.handshakeResolve?.(info);
      }
      return;
    }

    const id = obj.id;
    const entry = typeof id === "number" ? this.pending.get(id) : undefined;
    if (obj.type === "response" && entry) {
      this.pending.delete(id as number);
      clearTimeout(entry.timer);
      if (obj.success === false) {
        const message =
          typeof obj.error === "string" && obj.error.length > 0
            ? obj.error
            : `request ${String(id)} failed`;
        entry.reject(new OmpRpcError("request_failed", message));
      } else {
        entry.resolve(obj as unknown as RpcResponse);
      }
      return;
    }

    if (typeof obj.type !== "string") return;
    const event = obj as RpcEvent;
    for (const cb of [...this.eventCallbacks]) {
      try {
        cb(event);
      } catch {
        // Listener bugs must never kill the read pump.
      }
    }
  }

  private handleClosed(): void {
    this.finish(new OmpRpcError("channel_closed", "channel closed"));
  }

  private finish(error: OmpRpcError): void {
    if (this.finished) return;
    this.finished = true;
    this.closeError = error;
    for (const [id, entry] of [...this.pending]) {
      clearTimeout(entry.timer);
      entry.reject(this.closureError(`request id ${id}`));
    }
    this.pending.clear();
    if (!this.handshakeDone) this.handshakeReject?.(error);
    this.resolveClosed?.();
    for (const cb of [...this.closeCallbacks]) {
      try {
        cb();
      } catch {
        // Ignore listener failures.
      }
    }
    try {
      this.rawHandle?.close();
    } catch {
      // Ignore double-close races.
    }
    void this.channel.close().catch(() => {});
  }

  private closureError(context = "handshake"): OmpRpcError {
    return this.closeError ?? new OmpRpcError("channel_closed", `channel closed before ${context}`);
  }

  private writeError(cause: unknown, method: string): OmpRpcError {
    return new OmpRpcError("channel_closed", `write failed for ${method}: ${describe(cause)}`, {
      cause,
    });
  }
}

function parseReady(obj: Record<string, unknown>): ReadyInfo | undefined {
  if (obj.type !== "ready") return undefined;
  const sessionFile = typeof obj.sessionFile === "string" ? obj.sessionFile : undefined;
  return {
    protocolVersion: typeof obj.protocolVersion === "number" ? obj.protocolVersion : 0,
    ...(sessionFile !== undefined ? { sessionFile } : {}),
  };
}

/**
 * Wrap a channel so `onClosed` fires when the readable ends or errors. The
 * wrapper also decouples the pump from downstream reader attach order: chunks
 * read before a consumer attaches are buffered instead of lost.
 */
function reportClosed(channel: Channel, onClosed: () => void): Channel {
  let reported = false;
  const report = () => {
    if (!reported) {
      reported = true;
      onClosed();
    }
  };
  const readable = new ReadableStream<Uint8Array>({
    start(controller) {
      void (async () => {
        const reader = channel.readable.getReader();
        try {
          for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            controller.enqueue(value);
          }
          controller.close();
        } catch (cause) {
          try {
            controller.error(cause);
          } catch {
            // Controller already closed or cancelled.
          }
        }
        report();
      })();
    },
    cancel(reason) {
      void channel.readable.cancel(reason).catch(() => {});
    },
  });
  return {
    readable,
    writable: channel.writable,
    close: () => channel.close(),
  };
}

function describe(value: unknown): string {
  return value instanceof Error ? value.message : String(value);
}
