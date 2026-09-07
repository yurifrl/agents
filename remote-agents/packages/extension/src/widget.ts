// Widget rendering: Task[] -> ANSI-styled bordered lines (pi-herdr-subagents
// visual language): accent-colored box, title + counts in the top border,
// left-aligned elapsed/name rows, right-aligned state.

import {
  isActiveStatus,
  isTerminalStatus,
  type Task,
  type TaskStatus,
  type TaskEvent,
} from "@remote-agents/protocol";
import { fieldOf } from "./deliver.ts";

export const WIDGET_MAX_ROWS = 8;

const ACTIVE_ACCENT = "\x1b[38;2;77;163;255m"; // blue
const OPEN_ACCENT = "\x1b[38;2;214;158;46m"; // amber
const RST = "\x1b[0m";

const ANSI_RE = /\x1b\[[0-9;]*m/g;

/** Printable width of a string (ANSI escapes stripped, code points counted). */
function visibleWidth(text: string): number {
  return [...text.replace(ANSI_RE, "")].length;
}

/** Truncate plain text to a printable width with an ellipsis. */
function truncate(text: string, width: number): string {
  if (width <= 0) return "";
  const chars = [...text];
  if (chars.length <= width) return text;
  return width === 1 ? "…" : `${chars.slice(0, width - 1).join("")}…`;
}

/** Bordered content line: accent│left………right│accent (right-aligned right). */
function borderLine(left: string, right: string, width: number, accent: string): string {
  if (width <= 2) return `${accent}│${RST}`;
  const contentWidth = width - 2;
  const rightVis = visibleWidth(right);
  if (rightVis >= contentWidth) {
    const truncRight = truncate(right, contentWidth);
    const pad = Math.max(0, contentWidth - visibleWidth(truncRight));
    return `${accent}│${RST}${truncRight}${" ".repeat(pad)}${accent}│${RST}`;
  }
  const maxLeft = Math.max(0, contentWidth - rightVis);
  const truncLeft = truncate(left, maxLeft);
  const pad = Math.max(0, contentWidth - visibleWidth(truncLeft) - rightVis);
  return `${accent}│${RST}${truncLeft}${" ".repeat(pad)}${right}${accent}│${RST}`;
}

/** Bordered top line: accent╭─ Title ───── info ─╮accent */
function borderTop(title: string, info: string, width: number, accent: string): string {
  if (width <= 2) return `${accent}╭${RST}`;
  const inner = width - 2;
  const titlePart = `─ ${title} `;
  const infoPart = ` ${info} ─`;
  const fill = Math.max(0, inner - titlePart.length - infoPart.length);
  const content = `${titlePart}${"─".repeat(fill)}${infoPart}`.slice(0, inner).padEnd(inner, "─");
  return `${accent}╭${content}╮${RST}`;
}

function borderBottom(width: number, accent: string): string {
  if (width <= 2) return `${accent}╰${RST}`;
  return `${accent}╰${"─".repeat(width - 2)}╯${RST}`;
}

function formatMMSS(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`;
}

/** Human label per status; in-flight states get the trailing ellipsis. */
function statusLabel(status: TaskStatus): string {
  switch (status) {
    case "queued":
      return "queued…";
    case "starting":
      return "starting…";
    case "running":
      return "running";
    case "finalizing":
      return "finalizing…";
    default:
      return status;
  }
}

function lastActivity(task: Task): TaskEvent | undefined {
  const events = task.recentEvents;
  if (events == null) return undefined;
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const event = events[i];
    if (event?.type === "activity") return event;
  }
  return undefined;
}

function startKey(task: Task): number {
  return task.startedAt ?? task.createdAt;
}

/**
 * Render the widget for the given tasks. Returns [] when there is nothing to
 * show (the caller then hides the widget). Rows show active tasks plus open
 * (terminal, unacknowledged) ones; acknowledged terminal tasks disappear.
 */
export function renderWidget(tasks: Task[], now = Date.now(), width = 80): string[] {
  const shown = tasks.filter((task) => isActiveStatus(task.status) || !task.acknowledged);
  if (shown.length === 0) return [];
  const activeCount = shown.filter((task) => isActiveStatus(task.status)).length;
  const openCount = shown.length - activeCount;
  const ordered = [...shown].sort((a, b) => {
    const aActive = isActiveStatus(a.status) ? 0 : 1;
    const bActive = isActiveStatus(b.status) ? 0 : 1;
    if (aActive !== bActive) return aActive - bActive;
    return startKey(a) - startKey(b);
  });

  const info = activeCount > 0
    ? openCount > 0
      ? `${activeCount} active · ${openCount} open`
      : `${activeCount} active`
    : `${openCount} open`;
  const accent = activeCount > 0 ? ACTIVE_ACCENT : OPEN_ACCENT;

  const lines: string[] = [borderTop("Remote agents", info, width, accent)];
  for (const task of ordered.slice(0, WIDGET_MAX_ROWS)) {
    const start = task.startedAt ?? task.createdAt;
    const end = isTerminalStatus(task.status) ? (task.endedAt ?? now) : now;
    const elapsed = formatMMSS(Math.max(0, end - start));
    const left = ` ${elapsed}  ${task.name} `;
    let right = ` ${task.surface} · ${statusLabel(task.status)} `;
    const activity = lastActivity(task);
    if (activity != null && isActiveStatus(task.status)) {
      const seconds = Math.max(0, Math.floor((now - activity.ts) / 1000));
      const toolName = fieldOf(activity.data, "toolName");
      right += `· ${typeof toolName === "string" ? toolName : "tool"} ${seconds}s `;
    }
    lines.push(borderLine(left, right, width, accent));
  }
  if (ordered.length > WIDGET_MAX_ROWS) {
    lines.push(borderLine(` +${ordered.length - WIDGET_MAX_ROWS} more `, "", width, accent));
  }
  lines.push(borderBottom(width, accent));
  return lines;
}
