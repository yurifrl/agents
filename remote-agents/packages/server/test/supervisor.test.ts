import { afterEach, describe, expect, test } from "bun:test";
import type { CreateTaskInput } from "@remote-agents/protocol";
import { HttpError } from "../src/errors";
import { Supervisor, type SupervisorOpts } from "../src/supervisor";
import { TaskStore } from "../src/store";
import { FakeSurface, type FakeSurfaceOptions } from "./fake-surface";
import { cleanupDir, makeStateDir, stdioFakeOmpPath, tcpFakeOmpPath, waitUntil } from "./helpers";

// Contract §6: prefer the shared fake worker; the local throwaway fake covers
// fixtures the shared one lacks (--double-settled).
const sharedFakePath = await stdioFakeOmpPath();
const localFakePath = tcpFakeOmpPath();

const input: CreateTaskInput = { owner: "alice", name: "worker", task: "do work", cwd: "/tmp" };

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length > 0) {
    const cleanup = cleanups.pop();
    if (cleanup) await cleanup();
  }
});

function makeSup(
  extraArgs: string[] = [],
  opts: SupervisorOpts = {},
  fakePath = sharedFakePath,
  surfaceOpts: Omit<FakeSurfaceOptions, "fakePath" | "extraArgs"> = {},
) {
  const dir = makeStateDir();
  const surface = new FakeSurface({ fakePath, extraArgs, ...surfaceOpts });
  const store = new TaskStore(dir);
  const sup = new Supervisor(store, surface, { maxTimeMs: 60_000, ...opts });
  cleanups.push(async () => {
    await surface.killAll();
    cleanupDir(dir);
  });
  return { dir, surface, store, sup };
}

describe("Supervisor lifecycle", () => {
  test("queued → starting → running → finalizing → completed with result", async () => {
    const { store, sup } = makeSup();
    const task = sup.create(input);
    expect(task.status).toBe("queued");

    await waitUntil(() => {
      const current = store.get(task.id);
      // The fake settles fast: running may pass between polls, so accept any
      // post-start status and assert the exact transitions from the event log.
      return current && current.status !== "queued" && current.status !== "starting"
        ? current
        : undefined;
    });
    const done = await sup.whenSettled(task.id);
    expect(done?.status).toBe("completed");
    expect(done?.result).toBe("FAKE_RESULT=do work");
    expect(done?.startedAt).toBeGreaterThan(0);
    expect(done?.endedAt).toBeGreaterThanOrEqual(done?.startedAt ?? 0);
    expect(done?.sessionFile).toContain("fake");
    expect(done?.worker?.pid).toBeGreaterThan(0);

    const transitions = store
      .eventsSince(task.id, 0)
      .filter((e) => e.type === "state_changed" && "to" in e.data)
      .map((e) => ("to" in e.data ? e.data.to : ""));
    expect(transitions).toEqual(["starting", "running", "finalizing", "completed"]);
    const resultEvent = store.eventsSince(task.id, 0).find((e) => e.type === "result");
    expect(resultEvent && "text" in resultEvent.data ? resultEvent.data.text : "").toBe(
      "FAKE_RESULT=do work",
    );
  }, 15_000);

  test("tool_execution_start becomes activity events", async () => {
    const { store, sup } = makeSup(["--tool", "bash"]);
    const task = sup.create(input);
    await sup.whenSettled(task.id);
    const activities = store.eventsSince(task.id, 0).filter((e) => e.type === "activity");
    expect(activities.length).toBe(1);
    const data = activities[0]?.data;
    expect(data && "toolName" in data ? data.toolName : undefined).toBe("bash");
  }, 15_000);

  test("steer lands mid-run and is logged", async () => {
    const { store, sup } = makeSup(["--settled-delay", "500"]);
    const task = sup.create(input);
    await waitUntil(() => (store.get(task.id)?.status === "running" ? true : undefined));
    const steered = await sup.steer(task.id, "focus on the tests");
    expect(steered.status).toBe("running");
    const done = await sup.whenSettled(task.id);
    expect(done?.status).toBe("completed");
    const logged = store
      .eventsSince(task.id, 0)
      .some((e) => e.type === "log" && "line" in e.data && e.data.line === "steered: focus on the tests");
    expect(logged).toBe(true);
  }, 15_000);

  test("steer while queued is delivered after the prompt", async () => {
    const { store, sup } = makeSup([], {}, sharedFakePath, { startDelayMs: 200 });
    const task = sup.create(input);
    const steered = await sup.steer(task.id, "early note");
    expect(steered.status).toBe("queued");
    const done = await sup.whenSettled(task.id);
    expect(done?.status).toBe("completed");
    const logged = store
      .eventsSince(task.id, 0)
      .some((e) => e.type === "log" && "line" in e.data && e.data.line === "steered: early note");
    expect(logged).toBe(true);
  }, 15_000);

  test("steer after completion conflicts with 409", async () => {
    const { sup } = makeSup();
    const task = sup.create(input);
    await sup.whenSettled(task.id);
    const err = await sup.steer(task.id, "late").then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(HttpError);
    if (err instanceof HttpError) expect(err.status).toBe(409);
  }, 15_000);

  test("cancel mid-run marks the task canceled without a result", async () => {
    const { store, sup } = makeSup(["--settled-delay", "5000"]);
    const task = sup.create(input);
    await waitUntil(() => (store.get(task.id)?.status === "running" ? true : undefined));
    const canceled = await sup.cancel(task.id);
    expect(canceled.status).toBe("canceled");
    expect(canceled.stopRequested).toBe(true);
    const events = store.eventsSince(task.id, 0);
    expect(events.some((e) => e.type === "result")).toBe(false);

    const secondStatus = await sup.cancel(task.id).then(
      () => 0,
      (err: unknown) => (err instanceof HttpError ? err.status : 0),
    );
    expect(secondStatus).toBe(409); // double transition is guarded
  }, 15_000);

  test("cancel while queued settles the task without running it", async () => {
    const { store, sup } = makeSup();
    const task = sup.create(input);
    const canceled = await sup.cancel(task.id);
    expect(canceled.status).toBe("canceled");
    expect((await sup.whenSettled(task.id))?.status).toBe("canceled");
    expect(store.eventsSince(task.id, 0).some((e) => e.type === "result")).toBe(false);
  }, 15_000);

  test("deadline backstop fails a stuck task", async () => {
    const { store, sup } = makeSup(["--settled-delay", "60000"], { maxTimeMs: 300 });
    const task = sup.create(input);
    const done = await sup.whenSettled(task.id);
    expect(done?.status).toBe("failed");
    expect(done?.error).toContain("deadline exceeded");
    expect(store.eventsSince(task.id, 0).some((e) => e.type === "error")).toBe(true);
  }, 15_000);

  test("duplicate agent_settled is idempotent", async () => {
    const { store, sup } = makeSup(["--double-settled"], {}, localFakePath);
    const task = sup.create(input);
    const done = await sup.whenSettled(task.id);
    expect(done?.status).toBe("completed");
    const events = store.eventsSince(task.id, 0);
    expect(events.filter((e) => e.type === "result").length).toBe(1);
    expect(
      events.filter(
        (e) => e.type === "state_changed" && "to" in e.data && e.data.to === "completed",
      ).length,
    ).toBe(1);
  }, 15_000);

  test("worker that never emits agent_settled completes via agent_end quiet window", async () => {
    // Regression: installed omp builds never emit agent_settled; the quiet
    // window after agent_end is the only completion signal.
    const { store, sup } = makeSup(["--no-settled"], { settleQuietMs: 250 }, localFakePath);
    const task = sup.create(input);
    const done = await sup.whenSettled(task.id);
    expect(done?.status).toBe("completed");
    expect(done?.result).toBe("FAKE_RESULT=do work");
    const transitions = store
      .eventsSince(task.id, 0)
      .filter((e) => e.type === "state_changed" && "to" in e.data)
      .map((e) => ("to" in e.data ? e.data.to : ""));
    expect(transitions).toContain("finalizing");
    expect(transitions[transitions.length - 1]).toBe("completed");
  }, 15_000);

  test("whenSettled resolves immediately for already-terminal tasks", async () => {
    const { sup } = makeSup();
    const task = sup.create(input);
    await sup.whenSettled(task.id);
    const again = await sup.whenSettled(task.id);
    expect(again?.status).toBe("completed");
  }, 15_000);

  test("shutdown stops process workers and marks tasks canceled", async () => {
    const { store, surface, sup } = makeSup(["--settled-delay", "60000"]);
    const task = sup.create(input);
    await waitUntil(() => (store.get(task.id)?.status === "running" ? true : undefined));
    await sup.shutdown();
    expect(store.get(task.id)?.status).toBe("canceled");
    const proc = surface.procs[0];
    if (proc) await proc.exited; // worker process is dead
  }, 15_000);
});
