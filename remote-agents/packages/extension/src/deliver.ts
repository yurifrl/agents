// Pure formatting for result/failure delivery messages and human-readable
// task/event lines. No IO, no clock injection required beyond the optional
// `now` parameter (tests pass explicit values).

import { isTerminalStatus, type Task, type TaskEvent, type TaskStatus } from "@remote-agents/protocol";

export interface ResultDetails {
  taskId: string;
  name: string;
  status: TaskStatus;
  elapsedMs: number | undefined;
  sessionFile: string | undefined;
}

export interface Deliverable {
  /** Message body shown to the model / user. */
  content: string;
  /** One-liner for compact surfaces. */
  display: string;
  details: ResultDetails;
}

/** Elapsed time; frozen at endedAt once the task is terminal. */
export function taskElapsedMs(task: Task, now = Date.now()): number {
  const start = task.startedAt ?? task.createdAt;
  const end = isTerminalStatus(task.status) ? (task.endedAt ?? now) : now;
  return Math.max(0, end - start);
}

/** Compact human duration: 45s, 2m05s, 1h02m. */
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const mm = String(m).padStart(2, "0");
  const ss = String(s).padStart(2, "0");
  if (h > 0) return `${h}h${mm}m`;
  if (m > 0) return `${m}m${ss}s`;
  return `${ss}s`;
}

export function detailsOf(task: Task): ResultDetails {
  return {
    taskId: task.id,
    name: task.name,
    status: task.status,
    elapsedMs: taskElapsedMs(task),
    sessionFile: task.sessionFile,
  };
}

export function formatResult(task: Task): Deliverable {
  const elapsed = formatElapsed(taskElapsedMs(task));
  const result = (task.result ?? "").trim();
  const session = task.sessionFile != null ? ` · session: ${task.sessionFile}` : "";
  const content = [
    `Remote agent "${task.name}" completed in ${elapsed}.`,
    "",
    result !== "" ? result : "(no result text)",
    "",
    `[${task.id} · logs: /remote-agents logs ${task.id}${session}]`,
  ].join("\n");
  return { content, display: `Remote agent "${task.name}" completed`, details: detailsOf(task) };
}

export function formatFailure(task: Task): Deliverable {
  const error = (task.error ?? "").trim();
  const reason = error !== "" ? error : task.status === "canceled" ? "canceled by request" : "unknown error";
  const elapsed = formatElapsed(taskElapsedMs(task));
  const content = [
    `Remote agent "${task.name}" ${task.status} after ${elapsed}: ${reason}`,
    "",
    `Resume via: ${task.sessionFile ?? "(session file unknown)"} · history: /remote-agents logs ${task.id}`,
  ].join("\n");
  return { content, display: `Remote agent "${task.name}" ${task.status}`, details: detailsOf(task) };
}

/** Compact one-line task summary for listings (tool + /remote-agents list). */
export function describeTask(task: Task, now = Date.now()): string {
  const acked = task.acknowledged ? " · acked" : "";
  return `${task.id}  "${task.name}"  ${task.status}  (${formatElapsed(taskElapsedMs(task, now))})${acked}`;
}

/** One compact line per task event, for /remote-agents logs and RemoteAgentGet. */
export function formatEventLine(event: TaskEvent): string {
  return `${event.seq} ${event.type} ${describeEventData(event)}`;
}

/**
 * Read one field off an untyped payload defensively (TaskEventData is a
 * non-discriminated union on the wire; custom-message details are unknown).
 * `in` + typeof checks keep every access checked — no shape casts.
 */
export function fieldOf(value: unknown, key: string): unknown {
  if (typeof value !== "object" || value === null || !(key in value)) return undefined;
  const record = value as Record<string, unknown>; // guarded by typeof + `in` above
  return record[key];
}

function describeEventData(event: TaskEvent): string {
  switch (event.type) {
    case "state_changed": {
      const reason = fieldOf(event.data, "reason");
      return `${statusText(fieldOf(event.data, "from"))} → ${statusText(fieldOf(event.data, "to"))}${
        typeof reason === "string" ? ` (${reason})` : ""
      }`;
    }
    case "activity": {
      const toolName = fieldOf(event.data, "toolName");
      const detail = fieldOf(event.data, "detail");
      return `${typeof toolName === "string" ? toolName : "tool"}${typeof detail === "string" ? ` ${detail}` : ""}`;
    }
    case "result":
      return eventText(fieldOf(event.data, "text"));
    case "error":
      return eventText(fieldOf(event.data, "message")) || "error";
    case "log":
      return eventText(fieldOf(event.data, "line"));
  }
}

function statusText(value: unknown): string {
  return typeof value === "string" ? value : "?";
}

function eventText(value: unknown): string {
  return typeof value === "string" ? truncate(value, 200) : "";
}

function truncate(text: string, max: number): string {
  const trimmed = text.replace(/\s+$/, "");
  return trimmed.length > max ? `${trimmed.slice(0, max - 1)}…` : trimmed;
}
