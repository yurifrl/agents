// Shared types + constants for remote-agents. PURE: no runtime logic, no deps.

// ---------------------------------------------------------------------------
// Task domain
// ---------------------------------------------------------------------------

/** A2A-aligned task lifecycle. Terminal: completed | failed | canceled. */
export type TaskStatus =
  | "queued"
  | "starting"
  | "running"
  | "finalizing"
  | "completed"
  | "failed"
  | "canceled";

export const ACTIVE_STATUSES: readonly TaskStatus[] = [
  "queued",
  "starting",
  "running",
  "finalizing",
];

export function isActiveStatus(status: TaskStatus): boolean {
  return (ACTIVE_STATUSES as readonly string[]).includes(status);
}

export function isTerminalStatus(status: TaskStatus): boolean {
  return status === "completed" || status === "failed" || status === "canceled";
}

/** How the worker process is hosted. */
export type SurfaceKind = "process" | "herdr" | "k8s";

export interface TaskWorkerMeta {
  pid?: number;
  /** herdr surface: pane id ("w1:p3") */
  paneId?: string;
  workspaceId?: string;
  /** herdr surface: TCP bridge port */
  port?: number;
  /** herdr surface: bound-port file path */
  portFile?: string;
  /** k8s surface: pod IP (dial target; default loopback) */
  host?: string;
}

export interface Task {
  id: string;
  owner: string;
  /** short display name (widget row + pane label) */
  name: string;
  /** the prompt given to the worker */
  task: string;
  cwd: string;
  model?: string;
  status: TaskStatus;
  surface: SurfaceKind;
  /** true when the parent has consumed the result */
  acknowledged: boolean;
  createdAt: number;
  startedAt?: number;
  endedAt?: number;
  result?: string;
  error?: string;
  /** worker session file (resume handle) — known once the RPC handshake lands */
  sessionFile?: string;
  /** parent omp session file that spawned this task */
  parentSession?: string;
  worker?: TaskWorkerMeta;
  stopRequested?: boolean;
  /** last 10 events (detail responses only) */
  recentEvents?: TaskEvent[];
}

export interface CreateTaskInput {
  owner: string;
  name: string;
  task: string;
  cwd: string;
  model?: string;
  parentSession?: string;
}

// ---------------------------------------------------------------------------
// Task events (event log + SSE wire)
// ---------------------------------------------------------------------------

export type TaskEventType =
  | "state_changed"
  | "activity"
  | "result"
  | "error"
  | "log";

export type TaskEventData =
  | { from: TaskStatus; to: TaskStatus; reason?: string }
  | { toolName: string; detail?: string }
  | { text: string }
  | { message: string }
  | { line: string };

export interface TaskEvent {
  /** monotonic per task, starts at 1 */
  seq: number;
  taskId: string;
  type: TaskEventType;
  data: TaskEventData;
  ts: number;
}

/** SSE frame payload (data: {...}). */
export interface SseFrame {
  taskId: string;
  seq: number;
  type: TaskEventType;
  data: TaskEventData;
  ts: number;
}

// ---------------------------------------------------------------------------
// HTTP API DTOs
// ---------------------------------------------------------------------------

export interface AttachResponse {
  tasks: Task[];
  server: { uptimeMs: number; surface: SurfaceKind; version: string };
}

export interface HealthResponse {
  ok: true;
  version: string;
  surface: SurfaceKind;
  uptimeMs: number;
  tasks: { active: number; total: number };
}

export interface ApiError {
  error: string;
}

/** REST paths (single source of truth). */
export const API = {
  health: "/health",
  tasks: "/tasks",
  task: (id: string) => `/tasks/${encodeURIComponent(id)}`,
  steer: (id: string) => `/tasks/${encodeURIComponent(id)}/steer`,
  cancel: (id: string) => `/tasks/${encodeURIComponent(id)}/cancel`,
  ack: (id: string) => `/tasks/${encodeURIComponent(id)}/ack`,
  stream: (id: string, sinceSeq: number) =>
    `/tasks/${encodeURIComponent(id)}/stream?since=${sinceSeq}`,
  attach: "/attach",
} as const;

// ---------------------------------------------------------------------------
// Worker transport (the surface seam)
// ---------------------------------------------------------------------------

/**
 * Byte-transport to a worker's stdio. Every surface reduces to this.
 * readable/writable follow the web streams shape (Bun.spawn, Bun.connect,
 * and node stream conversions all provide it).
 */
export interface Channel {
  readonly readable: ReadableStream<Uint8Array>;
  readonly writable: WritableStream<Uint8Array>;
  close(): Promise<void>;
}

/** Everything a surface needs to launch one worker. */
export interface WorkerSpec {
  taskId: string;
  name: string;
  cwd: string;
  /** directory for session files, bridge artifacts, port files */
  sessionDir: string;
  ompBin: string;
  model?: string;
  /** hard wall clock for the worker process (--max-time), seconds */
  maxTimeS: number;
}

export interface RunningWorker {
  channel: Channel;
  meta: TaskWorkerMeta;
  stop(): Promise<void>;
}

export interface Surface {
  readonly kind: SurfaceKind;
  start(spec: WorkerSpec): Promise<RunningWorker>;
}

// ---------------------------------------------------------------------------
// Config + constants
// ---------------------------------------------------------------------------

export const PROTOCOL_VERSION = 1;
export const DEFAULT_PORT = 8679;

export const ENV = {
  url: "REMOTE_AGENTS_URL",
  port: "REMOTE_AGENTS_PORT",
  stateDir: "REMOTE_AGENTS_STATE_DIR",
  surface: "REMOTE_AGENTS_SURFACE",
  ompBin: "REMOTE_AGENTS_OMP_BIN",
  model: "REMOTE_AGENTS_MODEL",
  maxTimeS: "REMOTE_AGENTS_MAX_TIME_S",
  owner: "REMOTE_AGENTS_OWNER",
  /** Server binary override; default resolves `agents` from PATH. */
  serverBin: "REMOTE_AGENTS_SERVER_BIN",
} as const;

export const STATE_DIR_FILENAME = "state.json";
export const EVENTS_DIRNAME = "events";
export const WORKERS_DIRNAME = "workers";
export const LOGS_DIRNAME = "logs";
