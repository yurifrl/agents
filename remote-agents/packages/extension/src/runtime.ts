// Session-scoped extension state, parked on globalThis so it survives /reload
// (the reloaded module adopts the same runtime object; timers/subscriptions are
// torn down and rebuilt, while delivery bookkeeping persists).

import { createClient, type RemoteAgentsClient } from "./api.ts";
import type { Task } from "@remote-agents/protocol";

export interface RemoteAgentsRuntime {
  client: RemoteAgentsClient;
  /** Widget refresh interval, when the widget loop is running. */
  widgetTimer: ReturnType<typeof setInterval> | undefined;
  /** Consecutive empty widget polls; the loop self-stops after 2. */
  emptyPolls: number;
  /** Live SSE subscriptions per task. */
  subscriptions: Map<string, AbortController>;
  /** Highest SSE seq seen per task; resume point after a resubscribe. */
  lastSeq: Map<string, number>;
  /** Tasks whose result was already delivered to this parent. */
  delivered: Set<string>;
  /** Last fetched task list; the widget factory re-renders this live. */
  lastTasks: Task[];
}

const RUNTIME_KEY = Symbol.for("remote-agents/runtime");

export function ensureRuntime(): RemoteAgentsRuntime {
  const global = globalThis as unknown as Record<symbol, unknown>;
  const existing = global[RUNTIME_KEY];
  if (existing != null) return existing as RemoteAgentsRuntime;
  const created: RemoteAgentsRuntime = {
    client: createClient(),
    widgetTimer: undefined,
    emptyPolls: 0,
    subscriptions: new Map(),
    lastSeq: new Map(),
    delivered: new Set(),
    lastTasks: [],
  };
  global[RUNTIME_KEY] = created;
  return created;
}

/**
 * Drop this session's handles only: widget timer + SSE subscriptions.
 *
 * Deliberately keeps the server and its workers running (they belong to the
 * remote-agents server, not to this parent session — workers are re-adopted by
 * the next session_start), and keeps `delivered` / `lastSeq` so a /reload
 * adopts the same bookkeeping instead of re-delivering results.
 */
export function teardownRuntime(): void {
  const global = globalThis as unknown as Record<symbol, unknown>;
  const rt = global[RUNTIME_KEY] as RemoteAgentsRuntime | undefined;
  if (rt == null) return;
  if (rt.widgetTimer != null) {
    clearInterval(rt.widgetTimer);
    rt.widgetTimer = undefined;
  }
  for (const controller of rt.subscriptions.values()) controller.abort();
  rt.subscriptions.clear();
  rt.emptyPolls = 0;
}

/** Test seam: drop the singleton entirely so the next ensureRuntime rebuilds. */
export function resetRuntimeForTests(): void {
  teardownRuntime();
  delete (globalThis as unknown as Record<symbol, unknown>)[RUNTIME_KEY];
}
