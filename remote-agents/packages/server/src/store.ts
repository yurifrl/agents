import {
  EVENTS_DIRNAME,
  STATE_DIR_FILENAME,
  type CreateTaskInput,
  type SurfaceKind,
  type Task,
  type TaskEvent,
  type TaskEventData,
  type TaskEventType,
} from "@remote-agents/protocol";
import { randomUUID } from "node:crypto";
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";

const MIRROR_CAP = 5000;
const SAVE_DEBOUNCE_MS = 25;

export interface TaskFilter {
  owner?: string;
  unacknowledged?: boolean;
}

export type TaskListener = (event: TaskEvent) => void;

/**
 * Durable task store: `state.json` (all tasks, atomic tmp+rename, debounced)
 * plus one append-only `events/<taskId>.jsonl` per task with a per-task
 * monotonic `seq` starting at 1. A bounded in-memory mirror backs
 * `recentEvents` and cheap `eventsSince` reads; the jsonl file is the source
 * of truth when the mirror has been capped.
 */
export class TaskStore {
  readonly stateDir: string;
  private readonly eventsDir: string;
  private readonly stateFile: string;
  private readonly tasks = new Map<string, Task>();
  private readonly mirror = new Map<string, TaskEvent[]>();
  private readonly seqs = new Map<string, number>();
  private readonly listeners = new Map<string, Set<TaskListener>>();
  private saveTimer: Timer | null = null;

  constructor(stateDir: string) {
    this.stateDir = stateDir;
    this.eventsDir = path.join(stateDir, EVENTS_DIRNAME);
    this.stateFile = path.join(stateDir, STATE_DIR_FILENAME);
    mkdirSync(this.eventsDir, { recursive: true });
    this.load();
  }

  private load(): void {
    if (existsSync(this.stateFile)) {
      try {
        const parsed = JSON.parse(readFileSync(this.stateFile, "utf8")) as { tasks?: Task[] };
        for (const task of parsed.tasks ?? []) this.tasks.set(task.id, task);
      } catch {
        // Corrupted state file: start clean rather than crash; the events dir stays intact.
      }
    }
    let names: string[] = [];
    try {
      names = readdirSync(this.eventsDir);
    } catch {
      names = [];
    }
    for (const name of names) {
      if (!name.endsWith(".jsonl")) continue;
      const taskId = name.slice(0, -".jsonl".length);
      try {
        const events = this.readEventsFile(taskId);
        const last = events.at(-1);
        this.seqs.set(taskId, last ? last.seq : 0);
        this.mirror.set(taskId, events.slice(-MIRROR_CAP));
      } catch {
        // Unreadable event file: skip.
      }
    }
  }

  create(input: CreateTaskInput, surface: SurfaceKind): Task {
    const task: Task = {
      id: randomUUID(),
      owner: input.owner,
      name: input.name,
      task: input.task,
      cwd: input.cwd,
      model: input.model,
      parentSession: input.parentSession,
      status: "queued",
      surface,
      acknowledged: false,
      createdAt: Date.now(),
    };
    this.tasks.set(task.id, task);
    this.scheduleSave();
    return structuredClone(task);
  }

  get(id: string): Task | undefined {
    const task = this.tasks.get(id);
    return task ? structuredClone(task) : undefined;
  }

  list(filter: TaskFilter = {}): Task[] {
    const out: Task[] = [];
    for (const task of this.tasks.values()) {
      if (filter.owner !== undefined && task.owner !== filter.owner) continue;
      if (filter.unacknowledged && task.acknowledged) continue;
      out.push(structuredClone(task));
    }
    out.sort((a, b) => a.createdAt - b.createdAt || (a.id < b.id ? -1 : 1));
    return out;
  }

  all(): Task[] {
    return this.list();
  }

  update(id: string, patch: Partial<Task>): Task {
    const task = this.tasks.get(id);
    if (!task) throw new Error(`task not found: ${id}`);
    Object.assign(task, patch);
    this.scheduleSave();
    return structuredClone(task);
  }

  ack(id: string): Task {
    const task = this.tasks.get(id);
    if (!task) throw new Error(`task not found: ${id}`);
    return this.update(id, { acknowledged: true });
  }

  appendEvent(taskId: string, type: TaskEventType, data: TaskEventData): TaskEvent {
    const task = this.tasks.get(taskId);
    if (!task) throw new Error(`task not found: ${taskId}`);
    const seq = (this.seqs.get(taskId) ?? 0) + 1;
    const event: TaskEvent = { seq, taskId, type, data, ts: Date.now() };
    appendFileSync(path.join(this.eventsDir, `${taskId}.jsonl`), `${JSON.stringify(event)}\n`);
    this.seqs.set(taskId, seq);
    const buf = this.mirror.get(taskId) ?? [];
    buf.push(event);
    if (buf.length > MIRROR_CAP) buf.splice(0, buf.length - MIRROR_CAP);
    this.mirror.set(taskId, buf);
    const subs = this.listeners.get(taskId);
    if (subs) {
      for (const cb of [...subs]) {
        try {
          cb(event);
        } catch {
          // A broken subscriber must not break the store.
        }
      }
    }
    return structuredClone(event);
  }

  eventsSince(taskId: string, sinceSeq: number): TaskEvent[] {
    const buf = this.mirror.get(taskId);
    const first = buf?.[0];
    if (buf && buf.length > 0 && first && sinceSeq >= first.seq - 1) {
      return buf.filter((e) => e.seq > sinceSeq).map((e) => structuredClone(e));
    }
    return this.readEventsFile(taskId).filter((e) => e.seq > sinceSeq);
  }

  /** Last `n` events (default 10) — mirrors the file tail. */
  recentEvents(taskId: string, n = 10): TaskEvent[] {
    const buf = this.mirror.get(taskId);
    if (buf && buf.length > 0) return buf.slice(-n).map((e) => structuredClone(e));
    return this.readEventsFile(taskId).slice(-n);
  }

  /** Subscribe to live events for one task; returns an unsubscribe function. */
  subscribe(taskId: string, cb: TaskListener): () => void {
    let set = this.listeners.get(taskId);
    if (!set) {
      set = new Set();
      this.listeners.set(taskId, set);
    }
    set.add(cb);
    return () => {
      const s = this.listeners.get(taskId);
      if (!s) return;
      s.delete(cb);
      if (s.size === 0) this.listeners.delete(taskId);
    };
  }

  saveNow(): void {
    if (this.saveTimer) {
      clearTimeout(this.saveTimer);
      this.saveTimer = null;
    }
    mkdirSync(this.stateDir, { recursive: true });
    const tmp = `${this.stateFile}.tmp`;
    const payload = `${JSON.stringify({ version: 1, tasks: [...this.tasks.values()] }, null, 2)}\n`;
    writeFileSync(tmp, payload);
    renameSync(tmp, this.stateFile);
  }

  private scheduleSave(): void {
    if (this.saveTimer) return;
    const timer = setTimeout(() => {
      this.saveTimer = null;
      this.saveNow();
    }, SAVE_DEBOUNCE_MS);
    timer.unref?.();
    this.saveTimer = timer;
  }

  private readEventsFile(taskId: string): TaskEvent[] {
    const file = path.join(this.eventsDir, `${taskId}.jsonl`);
    if (!existsSync(file)) return [];
    try {
      const lines = readFileSync(file, "utf8").split("\n").filter((l) => l.trim().length > 0);
      return lines.map((l) => JSON.parse(l) as TaskEvent);
    } catch {
      return [];
    }
  }
}
