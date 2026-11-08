import { expect, test } from "bun:test";
import type { Task } from "@remote-agents/protocol";
import { renderWidget } from "../src/widget.ts";
import { resolveWidget } from "./helpers";

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

test("empty input and fully-acknowledged tasks render nothing", () => {
  expect(renderWidget([])).toEqual([]);
  const done = task({ id: "t1", status: "completed", acknowledged: true });
  expect(renderWidget([done], NOW)).toEqual([]);
});

test("bordered box: title with active/open counts, one row per task", () => {
  const tasks = [
    task({ id: "a" }),
    task({ id: "b", status: "queued" }),
    task({ id: "c", status: "completed" }),
    task({ id: "d", status: "failed" }),
    task({ id: "e", status: "completed", acknowledged: true }),
  ];
  const lines = renderWidget(tasks, NOW).map((l) => l.replace(/\x1b\[[0-9;]*m/g, ""));
  expect(lines).toHaveLength(1 + 4 + 1); // top + rows + bottom
  expect(lines[0]).toContain("Remote agents");
  expect(lines[0]).toContain("2 active · 2 open");
  expect(lines[0]?.startsWith("╭")).toBe(true);
  expect(lines[lines.length - 1]?.startsWith("╰")).toBe(true);
});

test("row: elapsed MM:SS, name, surface, state, activity suffix", () => {
  const t = task({
    id: "build",
    name: "build",
    surface: "herdr",
    startedAt: NOW - 65_000,
    recentEvents: [
      { seq: 1, taskId: "build", type: "activity", data: { toolName: "bash", detail: "ls" }, ts: NOW - 5_000 },
    ],
  });
  const lines = renderWidget([t], NOW);
  expect(lines).toHaveLength(3); // top + row + bottom
  const row = lines[1];
  expect(row).toContain("01:05");
  expect(row).toContain("build");
  expect(row).toContain("herdr · running");
  expect(row).toContain("bash 5s");
});

test("elapsed freezes at endedAt for terminal tasks", () => {
  const t = task({
    id: "done",
    status: "completed",
    acknowledged: false,
    startedAt: NOW - 60_000,
    endedAt: NOW - 30_000,
  });
  const before = renderWidget([t], NOW);
  const later = renderWidget([t], NOW + 5 * 60_000);
  expect(before[1]).toContain("00:30");
  expect(later[1]).toContain("00:30");
  expect(later[1]).toContain("completed");
});

test("long names truncate, never overflow the box", () => {
  const long = task({ id: "l", name: "x".repeat(60) });
  const plain = renderWidget([long], NOW, 40)[1]?.replace(/\x1b\[[0-9;]*m/g, "") ?? "";
  expect(plain.startsWith("│")).toBe(true);
  expect(plain.endsWith("│")).toBe(true);
  expect([...plain].length).toBe(40);
});

test("rows cap at 8 with a +N more line", () => {
  const tasks: Task[] = [];
  for (let i = 0; i < 10; i++) tasks.push(task({ id: `t${i}`, name: `agent-${i}` }));
  const lines = renderWidget(tasks, NOW);
  expect(lines).toHaveLength(1 + 8 + 1 + 1); // top + rows + more + bottom
  expect(lines[lines.length - 2]).toContain("+2 more");
});

test("active tasks sort before terminal ones", () => {
  const tasks = [
    task({ id: "old-done", status: "completed", startedAt: NOW - 600_000, endedAt: NOW - 500_000 }),
    task({ id: "live", startedAt: NOW - 10_000 }),
  ];
  const lines = renderWidget(tasks, NOW);
  expect(lines[1]).toContain("live");
  expect(lines[2]).toContain("old-done");
});

test("resolved factory output matches direct render (ANSI stripped)", () => {
  const t = task({ id: "w", name: "watch", startedAt: NOW - 30_000 });
  const styled = renderWidget([t], NOW, 50);
  const direct = resolveWidget(styled, 50);
  expect(direct[0]).toContain("Remote agents");
  expect(direct[1]).toContain("watch");
});
