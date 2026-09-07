import { describe, expect, test } from "bun:test";
import { existsSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import type { TaskEvent } from "@remote-agents/protocol";
import { TaskStore } from "../src/store";
import { cleanupDir, makeStateDir, waitUntil } from "./helpers";

const input = {
  owner: "alice",
  name: "build thing",
  task: "do the build",
  cwd: "/tmp/workspace",
};

describe("TaskStore", () => {
  test("create/get/list with filters", () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const a = store.create(input, "process");
      const b = store.create({ ...input, owner: "bob", name: "other" }, "herdr");
      expect(store.get(a.id)?.status).toBe("queued");
      expect(store.get(a.id)?.surface).toBe("process");
      expect(store.get(b.id)?.surface).toBe("herdr");

      expect(store.list({ owner: "alice" }).map((t) => t.id)).toEqual([a.id]);
      expect(store.all().length).toBe(2);

      store.ack(a.id);
      expect(store.list({ owner: "alice", unacknowledged: true })).toEqual([]);
      expect(store.list({ unacknowledged: true }).map((t) => t.id)).toEqual([b.id]);
      expect(store.get(a.id)?.acknowledged).toBe(true);
    } finally {
      cleanupDir(dir);
    }
  });

  test("update merges patches and persists via saveNow atomically", () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const task = store.create(input, "process");
      store.update(task.id, { status: "running", worker: { pid: 4242 }, startedAt: 123 });
      store.saveNow();

      const stateFile = path.join(dir, "state.json");
      expect(existsSync(`${stateFile}.tmp`)).toBe(false); // tmp+rename leaves no residue
      const parsed = JSON.parse(readFileSync(stateFile, "utf8")) as {
        tasks: Array<{ status: string; worker?: { pid: number }; startedAt?: number }>;
      };
      const persisted = parsed.tasks[0];
      expect(persisted?.status).toBe("running");
      expect(persisted?.worker?.pid).toBe(4242);
      expect(persisted?.startedAt).toBe(123);
      expect(store.get(task.id)?.name).toBe("build thing"); // untouched fields survive
    } finally {
      cleanupDir(dir);
    }
  });

  test("debounced save flushes without saveNow", async () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const task = store.create(input, "process");
      const stateFile = path.join(dir, "state.json");
      expect(existsSync(stateFile)).toBe(false); // debounced: not yet flushed
      // Real 25ms debounce inside the store — awaiting the observable signal
      // (file appears) rather than a fixed sleep.
      await waitUntil(() => (existsSync(stateFile) ? true : undefined), 2_000, 10);
      const parsed = JSON.parse(readFileSync(stateFile, "utf8")) as { tasks: Array<{ id: string }> };
      expect(parsed.tasks[0]?.id).toBe(task.id);
    } finally {
      cleanupDir(dir);
    }
  });

  test("events get monotonic per-task seq starting at 1", () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const a = store.create(input, "process");
      const b = store.create(input, "process");
      const e1 = store.appendEvent(a.id, "log", { line: "one" });
      const e2 = store.appendEvent(a.id, "log", { line: "two" });
      const e3 = store.appendEvent(b.id, "log", { line: "other task" });
      expect([e1.seq, e2.seq, e3.seq]).toEqual([1, 2, 1]);
      expect(e1.ts).toBeGreaterThan(0);

      expect(store.eventsSince(a.id, 0).map((e) => e.seq)).toEqual([1, 2]);
      expect(store.eventsSince(a.id, 1).map((e) => e.seq)).toEqual([2]);
      expect(store.eventsSince(a.id, 2)).toEqual([]);
      expect(store.recentEvents(a.id).map((e) => e.seq)).toEqual([1, 2]);
    } finally {
      cleanupDir(dir);
    }
  });

  test("events and cursor persist across restart", () => {
    const dir = makeStateDir();
    try {
      const first = new TaskStore(dir);
      const task = first.create(input, "process");
      first.appendEvent(task.id, "log", { line: "one" });
      first.appendEvent(task.id, "log", { line: "two" });
      first.update(task.id, { status: "running" });
      first.saveNow();

      const second = new TaskStore(dir); // simulates a server restart
      expect(second.get(task.id)?.status).toBe("running");
      const replayed = second.eventsSince(task.id, 0);
      expect(replayed.map((e) => ("line" in e.data ? e.data.line : ""))).toEqual(["one", "two"]);

      const appended = second.appendEvent(task.id, "log", { line: "three" });
      expect(appended.seq).toBe(3); // cursor continued, not restarted
      expect(second.recentEvents(task.id).map((e) => e.seq)).toEqual([1, 2, 3]);
    } finally {
      cleanupDir(dir);
    }
  });

  test("mirror cap falls back to the jsonl file for old ranges", () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const task = store.create(input, "process");
      for (let i = 1; i <= 5010; i++) store.appendEvent(task.id, "log", { line: `e${i}` });

      const all = store.eventsSince(task.id, 0); // since=0 < first mirrored seq → file read
      expect(all.length).toBe(5010);
      expect(all[0]?.seq).toBe(1);
      const recent = store.recentEvents(task.id, 10).map((e) => e.seq);
      expect(recent).toEqual([5001, 5002, 5003, 5004, 5005, 5006, 5007, 5008, 5009, 5010]);

      const reopened = new TaskStore(dir); // mirror re-seeds capped at 5000
      expect(reopened.eventsSince(task.id, 0).length).toBe(5010);
      expect(reopened.eventsSince(task.id, 5000).map((e) => e.seq)).toEqual(recent);
    } finally {
      cleanupDir(dir);
    }
  });

  test("appendEvent persists full JSON lines to events/<id>.jsonl", () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const task = store.create(input, "process");
      store.appendEvent(task.id, "activity", { toolName: "bash", detail: "{}" });
      const file = path.join(dir, "events", `${task.id}.jsonl`);
      expect(statSync(file).isFile()).toBe(true);
      const line = JSON.parse(readFileSync(file, "utf8").trim()) as TaskEvent;
      expect(line.seq).toBe(1);
      expect(line.taskId).toBe(task.id);
      expect(line.type).toBe("activity");
      expect(line.data).toEqual({ toolName: "bash", detail: "{}" });
    } finally {
      cleanupDir(dir);
    }
  });

  test("subscribe delivers live events until unsubscribed", () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const task = store.create(input, "process");
      const seen: number[] = [];
      const unsubscribe = store.subscribe(task.id, (event) => seen.push(event.seq));
      store.appendEvent(task.id, "log", { line: "a" });
      unsubscribe();
      store.appendEvent(task.id, "log", { line: "b" });
      expect(seen).toEqual([1]);
    } finally {
      cleanupDir(dir);
    }
  });
});
