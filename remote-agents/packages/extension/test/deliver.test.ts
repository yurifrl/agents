import { expect, test } from "bun:test";
import type { Task, TaskEventData } from "@remote-agents/protocol";
import { describeTask, formatElapsed, formatEventLine, formatFailure, formatResult } from "../src/deliver.ts";

const NOW = 1_000_000_000_000;

function task(partial: Partial<Task> & { id: string }): Task {
  return {
    owner: "local",
    name: partial.id,
    task: "work",
    cwd: "/w",
    status: "running",
    surface: "process",
    acknowledged: false,
    createdAt: NOW - 60_000,
    ...partial,
  };
}

test("formatElapsed composes compact units", () => {
  expect(formatElapsed(45_000)).toBe("45s");
  expect(formatElapsed(65_000)).toBe("1m05s");
  expect(formatElapsed(3_723_000)).toBe("1h02m");
  expect(formatElapsed(0)).toBe("00s");
});

test("formatResult: success text with result, logs hint, and details", () => {
  const t = task({
    id: "t1",
    name: "builder",
    status: "completed",
    result: "  Build passed.  ",
    sessionFile: "/w/worker.jsonl",
    startedAt: NOW - 60_000,
    endedAt: NOW - 15_000,
  });
  const out = formatResult(t);
  expect(out.display).toBe('Remote agent "builder" completed');
  expect(out.content).toContain('Remote agent "builder" completed in 45s.');
  expect(out.content).toContain("Build passed.");
  expect(out.content).toContain("/remote-agents logs t1");
  expect(out.content).toContain("session: /w/worker.jsonl");
  expect(out.details).toEqual({
    taskId: "t1",
    name: "builder",
    status: "completed",
    elapsedMs: 45_000,
    sessionFile: "/w/worker.jsonl",
  });
});

test("formatResult without result text uses a placeholder", () => {
  const out = formatResult(task({ id: "t2", status: "completed" }));
  expect(out.content).toContain("(no result text)");
});

test("formatFailure: error reason, resume hint, details", () => {
  const t = task({
    id: "t3",
    name: "broken",
    status: "failed",
    error: "worker exited (code 1)",
    sessionFile: "/w/broken.jsonl",
    startedAt: NOW - 60_000,
    endedAt: NOW,
  });
  const out = formatFailure(t);
  expect(out.display).toBe('Remote agent "broken" failed');
  expect(out.content).toContain("failed after 1m00s: worker exited (code 1)");
  expect(out.content).toContain("Resume via: /w/broken.jsonl");
  expect(out.content).toContain("/remote-agents logs t3");
  expect(out.details.status).toBe("failed");
});

test("formatFailure for cancel without error text", () => {
  const out = formatFailure(task({ id: "t4", status: "canceled", startedAt: NOW - 60_000, endedAt: NOW }));
  expect(out.content).toContain("canceled after 1m00s: canceled by request");
  expect(out.content).toContain("Resume via: (session file unknown)");
});

test("describeTask renders id, name, status, elapsed, ack marker", () => {
  const t = task({ id: "t5", name: "scout", status: "running", startedAt: NOW - 30_000 });
  expect(describeTask(t, NOW)).toBe('t5  "scout"  running  (30s)');
  const acked = task({ id: "t6", status: "completed", acknowledged: true, endedAt: NOW });
  expect(describeTask(acked, NOW)).toContain("· acked");
});

test("formatEventLine renders each event kind", () => {
  const e = (type: "state_changed" | "activity" | "result" | "error" | "log", data: TaskEventData) => ({
    seq: 3,
    taskId: "t1",
    type,
    data,
    ts: NOW,
  });
  expect(formatEventLine(e("state_changed", { from: "queued", to: "running" }))).toBe("3 state_changed queued → running");
  expect(formatEventLine(e("activity", { toolName: "bash", detail: "ls -la" }))).toBe("3 activity bash ls -la");
  expect(formatEventLine(e("result", { text: "done" }))).toBe("3 result done");
  expect(formatEventLine(e("error", { message: "boom" }))).toBe("3 error boom");
  expect(formatEventLine(e("log", { line: "readopted" }))).toBe("3 log readopted");
});
