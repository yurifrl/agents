import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ENV } from "@remote-agents/protocol";
import remoteAgentsExtension, { startWidgetTimer, widgetTick } from "../src/index.ts";
import type { PiExtensionAPI } from "../src/index.ts";
import { ensureRuntime, resetRuntimeForTests } from "../src/runtime.ts";
import { fieldOf } from "../src/deliver.ts";
import {
  createFixtureServer,
  createMockCtx,
  createMockPi,
  runTool,
  startSession,
  useFixture,
  waitFor,
  resolveWidget,
  type FixtureServer,
  type MockPi,
} from "./helpers.ts";

const PARENT_SESSION = "/tmp/parent/session.jsonl";

let server: FixtureServer;
let mock: MockPi;
let testHome: string;
let realHome: string | undefined;

beforeEach(async () => {
  server = await createFixtureServer();
  useFixture(server);
  // Isolate remote-agents.json reads from the real agent dir.
  testHome = mkdtempSync(join(tmpdir(), "ra-runtime-home-"));
  realHome = process.env.HOME;
  process.env.HOME = testHome;
  mock = createMockPi();
  remoteAgentsExtension(mock.pi as unknown as PiExtensionAPI);
});

afterEach(async () => {
  resetRuntimeForTests();
  process.env.HOME = realHome;
  rmSync(testHome, { recursive: true, force: true });
  await server.close();
});

describe("session_start reconcile", () => {
  test("completed unacknowledged task: delivered once via steer + acked", async () => {
    server.seedTask({
      id: "t-done",
      status: "completed",
      result: "All finished",
      sessionFile: "/w/worker.jsonl",
      parentSession: PARENT_SESSION,
      startedAt: Date.now() - 60_000,
      endedAt: Date.now() - 30_000,
    });
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    await waitFor(() => mock.sent.length === 1);
    await waitFor(() => server.requests.some((r) => r.path === "/tasks/t-done/ack"));

    const sent = mock.sent[0]!;
    expect(sent.message.customType).toBe("remote_agent_result");
    expect(sent.options).toEqual({ triggerTurn: true, deliverAs: "steer" });
    expect(sent.message.display).toBe(true);
    const content = String(sent.message.content);
    expect(content).toContain("All finished");
    expect(content).toContain("/remote-agents logs t-done");
    expect(content).toContain("session: /w/worker.jsonl");
    const details = sent.message.details;
    expect(fieldOf(details, "taskId")).toBe("t-done");
    expect(fieldOf(details, "status")).toBe("completed");
    expect(fieldOf(details, "sessionFile")).toBe("/w/worker.jsonl");

    // ack marked the task server-side
    const fixtureTask = server.tasks.get("t-done")!;
    expect(fixtureTask.acknowledged).toBe(true);
  });

  test("already-acknowledged terminal task is not delivered", async () => {
    server.seedTask({ id: "t-acked", status: "completed", result: "x", acknowledged: true });
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    expect(mock.sent).toHaveLength(0);
    expect(server.requests.some((r) => r.path.endsWith("/ack"))).toBe(false);
  });

  test("running task subscribes; terminal event delivers exactly once", async () => {
    server.seedTask({ id: "t-run", status: "running", startedAt: Date.now() - 5_000, parentSession: PARENT_SESSION });
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    await waitFor(() => server.requests.some((r) => r.path === "/tasks/t-run/stream?since=0"));

    server.pushEvent("t-run", "result", { text: "the result" });
    server.pushEvent("t-run", "state_changed", { from: "running", to: "completed" });
    await waitFor(() => mock.sent.length === 1);
    await waitFor(() => server.requests.some((r) => r.path === "/tasks/t-run/ack"));

    // duplicate terminal event must not re-deliver (fenced synchronously)
    server.pushEvent("t-run", "state_changed", { from: "completed", to: "completed" });
    expect(mock.sent).toHaveLength(1);

    // a second reconcile does not re-deliver either (acked + delivered set)
    await startSession(mock, ctx);
    expect(mock.sent).toHaveLength(1);
  });

  test("tasks from another parent session are left to that parent", async () => {
    server.seedTask({ id: "t-other", status: "completed", result: "x", parentSession: "/other/session.jsonl" });
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    expect(mock.sent).toHaveLength(0);
    expect(server.requests.some((r) => r.path === "/tasks/t-other/ack")).toBe(false);
  });

  test("failed task delivers the failure text with resume hint", async () => {
    server.seedTask({
      id: "t-fail",
      status: "failed",
      error: "worker exited (code 1)",
      sessionFile: "/w/broken.jsonl",
    });
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    await waitFor(() => mock.sent.length === 1);
    const content = String(mock.sent[0]!.message.content);
    expect(content).toContain("worker exited (code 1)");
    expect(content).toContain("Resume via: /w/broken.jsonl");
    expect(content).toContain("/remote-agents logs t-fail");
  });

  test("unreachable server stays silent and marks the chip disconnected", async () => {
    process.env[ENV.url] = "http://127.0.0.1:1";
    // Guard against accidental spawns: even if reconcile tried to spawn, this
    // entry does not exist and the test would notice via the error path.
    process.env[ENV.serverBin] = "/nonexistent/agents";
    resetRuntimeForTests();
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    expect(ctx.notifications).toHaveLength(0);
    expect(ctx.statuses.get("remote-agents")).toBe("📡 disconnected — /remote-agents connect");
    expect(ensureRuntime().connected).toBe(false);
    expect(mock.sent).toHaveLength(0);
    delete process.env[ENV.serverBin];
  }, 15_000);

  test("reachable server connects and marks the chip connected", async () => {
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    expect(ctx.statuses.get("remote-agents")).toBe("📡 connected");
    expect(ensureRuntime().connected).toBe(true);
    expect(ctx.notifications).toHaveLength(0);
  });

  test("tools refuse to fire while disconnected and never spawn", async () => {
    process.env[ENV.url] = "http://127.0.0.1:1";
    resetRuntimeForTests();
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    const result = await runTool(mock.tools.get("RemoteAgent")!, { name: "x", task: "y" }, ctx);
    expect(result.content[0]?.text).toContain("not connected");
    expect(result.content[0]?.text).toContain("/remote-agents connect");
    const list = await runTool(mock.tools.get("RemoteAgentList")!, {}, ctx);
    expect(list.content[0]?.text).toContain("not connected");
  });

  test("/remote-agents connect attaches and confirms", async () => {
    server.seedTask({ id: "t-live", status: "running", startedAt: Date.now() - 1_000, parentSession: PARENT_SESSION });
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    const handler = mock.commands.get("remote-agents")!.handler;
    await handler("connect", ctx.ctx);
    expect(ctx.statuses.get("remote-agents")).toBe("📡 connected");
    expect(ctx.notifications.some((n) => n.message.includes("connected"))).toBe(true);
    expect(ctx.notifications.some((n) => n.message.includes("1 active"))).toBe(true);
  });

  test("orphan task with unknown parent is adopted by this session", async () => {
    server.seedTask({ id: "t-orphan", status: "running", startedAt: Date.now() - 1_000 });
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    await waitFor(() => server.requests.some((r) => r.path === "/tasks/t-orphan/stream?since=0"));
  });
});

describe("widget loop", () => {
  test("renders tasks into the widget above the editor", async () => {
    server.seedTask({ id: "t1", name: "builder", status: "running", startedAt: Date.now() - 30_000 });
    const ctx = createMockCtx();
    const rt = ensureRuntime();
    await widgetTick(rt, ctx.ctx);
    const payload = ctx.widgets.get("remote-agents");
    expect(typeof payload).toBe("function");
    const lines = resolveWidget(payload);
    expect(lines[0]).toContain("Remote agents");
    expect(lines[0]).toContain("1 active");
    expect(lines).toHaveLength(3);
  });

  test("auto-stops after two consecutive empty polls with no subscriptions", async () => {
    const ctx = createMockCtx();
    const rt = ensureRuntime();
    startWidgetTimer(rt, ctx.ctx, 15);
    await waitFor(() => rt.widgetTimer == null, 2_000);
    expect(ctx.widgets.has("remote-agents")).toBe(true);
    expect(rt.widgetTimer).toBeUndefined();
  });

  test("keeps running while a subscription is alive even with no visible tasks", async () => {
    server.seedTask({ id: "t-live", status: "running", startedAt: Date.now() - 1_000, parentSession: PARENT_SESSION });
    const ctx = createMockCtx();
    const rt = ensureRuntime();
    await startSession(mock, ctx);
    await waitFor(() => rt.subscriptions.has("t-live"));
    // Hide the task server-side (e.g. acked elsewhere): widget empties, but the
    // subscription keeps the loop alive.
    const live = server.tasks.get("t-live")!;
    live.acknowledged = true;
    live.status = "completed";
    live.endedAt = Date.now();
    await widgetTick(rt, ctx.ctx);
    await widgetTick(rt, ctx.ctx);
    expect(rt.widgetTimer != null || rt.emptyPolls === 0).toBe(true);
  });
});

describe("session_shutdown", () => {
  test("drops the widget timer and subscriptions, keeps delivery bookkeeping", async () => {
    server.seedTask({ id: "t-run2", status: "running", startedAt: Date.now(), parentSession: PARENT_SESSION });
    const ctx = createMockCtx();
    await startSession(mock, ctx);
    const rt = ensureRuntime();
    await waitFor(() => rt.subscriptions.has("t-run2"));

    const handler = mock.handlers.get("session_shutdown");
    expect(handler).toBeDefined();
    await handler!({ type: "session_shutdown", reason: "quit" }, ctx.ctx);

    expect(rt.widgetTimer).toBeUndefined();
    expect(rt.subscriptions.size).toBe(0);
    // delivered/lastSeq survive /reload by design
    expect(rt.delivered).toBeDefined();
    expect(rt.lastSeq).toBeDefined();
  });
});
