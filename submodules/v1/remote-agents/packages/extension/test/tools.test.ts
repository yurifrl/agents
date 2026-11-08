import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ENV } from "@remote-agents/protocol";
import remoteAgentsExtension from "../src/index.ts";
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
  type FixtureServer,
  type MockCtx,
  type MockPi,
  type RenderedComponent,
} from "./helpers.ts";

let server: FixtureServer;
let mock: MockPi;
let testHome: string;
let realHome: string | undefined;

beforeEach(async () => {
  server = await createFixtureServer();
  useFixture(server);
  // Isolate remote-agents.json reads from the real agent dir.
  testHome = mkdtempSync(join(tmpdir(), "ra-tools-home-"));
  realHome = process.env.HOME;
  process.env.HOME = testHome;
  mock = createMockPi();
  remoteAgentsExtension(mock.pi as unknown as PiExtensionAPI);
  // New contract: tools fire only after a connect. The fixture server is up,
  // so the session_start health probe connects once for every test.
  await startSession(mock, createMockCtx());
});

afterEach(async () => {
  resetRuntimeForTests();
  process.env.HOME = realHome;
  rmSync(testHome, { recursive: true, force: true });
  await server.close();
});

function deadServerEnv(): void {
  process.env[ENV.url] = "http://127.0.0.1:1";
  resetRuntimeForTests();
}

describe("registration", () => {
  test("registers 5 tools, the /remote-agents command, and a result renderer", () => {
    expect([...mock.tools.keys()].sort()).toEqual([
      "RemoteAgent",
      "RemoteAgentGet",
      "RemoteAgentKill",
      "RemoteAgentList",
      "RemoteAgentSteer",
    ]);
    expect(mock.commands.has("remote-agents")).toBe(true);
    expect(mock.renderers.has("remote_agent_result")).toBe(true);
    const spawn = mock.tools.get("RemoteAgent")!;
    expect(spawn.description).toContain("delivered");
  });
});

describe("RemoteAgent tool", () => {
  test("spawns a task, subscribes, and steers the result back", async () => {
    const ctx = createMockCtx();
    const result = await runTool(mock.tools.get("RemoteAgent")!, { name: "builder", task: "build it" }, ctx);
    const created = server.tasks.get("fx1")!;
    expect(created.name).toBe("builder");
    expect(created.owner).toBe("local");
    expect(created.parentSession).toBe("/tmp/parent/session.jsonl");
    expect(created.cwd).toBe("/tmp/parent");
    const text = result.content[0]?.text ?? "";
    expect(text).toContain(created.id);
    expect(text).toContain("delivered automatically");
    expect(fieldOf(result.details, "taskId")).toBe(created.id);

    // result steer-back through the subscription
    server.pushEvent(created.id, "result", { text: "build ok" });
    server.pushEvent(created.id, "state_changed", { from: "queued", to: "completed" });
    await waitFor(() => mock.sent.length === 1);
    await waitFor(() => server.requests.some((r) => r.path === `/tasks/${created.id}/ack`));
    expect(String(mock.sent[0]!.message.content)).toContain("build ok");
  });

  test("explicit cwd and model are forwarded", async () => {
    const ctx = createMockCtx();
    await runTool(
      mock.tools.get("RemoteAgent")!,
      { name: "m", task: "t", cwd: "/explicit", model: "anthropic/claude-x" },
      ctx,
    );
    const created = server.tasks.get("fx1")!;
    expect(created.cwd).toBe("/explicit");
    expect(created.model).toBe("anthropic/claude-x");
  });

  test("spawn refuses with a hint when disconnected (no request, no spawn)", async () => {
    deadServerEnv();
    const ctx = createMockCtx();
    const result = await runTool(mock.tools.get("RemoteAgent")!, { name: "x", task: "y" }, ctx);
    expect(result.content[0]?.text).toContain("not connected");
    expect(result.content[0]?.text).toContain("/remote-agents connect");
    // No create request reached anywhere: the guard returns before I/O.
    expect(server.requests.some((r) => r.method === "POST" && r.path === "/tasks")).toBe(false);
  });
});

describe("query tools", () => {
  test("RemoteAgentList reports empty and populated lists", async () => {
    const ctx = createMockCtx();
    const empty = await runTool(mock.tools.get("RemoteAgentList")!, {}, ctx);
    expect(empty.content[0]?.text).toBe("No remote agents.");
    server.seedTask({ id: "t1", name: "scout", status: "running", startedAt: Date.now() - 30_000 });
    const populated = await runTool(mock.tools.get("RemoteAgentList")!, {}, ctx);
    expect(populated.content[0]?.text).toContain('t1  "scout"  running');
  });

  test("RemoteAgentList refuses with the connect hint when the server is down", async () => {
    deadServerEnv();
    const ctx = createMockCtx();
    const result = await runTool(mock.tools.get("RemoteAgentList")!, {}, ctx);
    expect(result.content[0]?.text).toContain("not connected");
  });

  test("RemoteAgentGet renders detail with events; 404 throws", async () => {
    const ctx = createMockCtx();
    const seeded = server.seedTask({ id: "t1", status: "running", sessionFile: "/w/s.jsonl" });
    server.pushEvent("t1", "state_changed", { from: "queued", to: "running" });
    server.pushEvent("t1", "activity", { toolName: "bash", detail: "ls" });
    const result = await runTool(mock.tools.get("RemoteAgentGet")!, { id: "t1" }, ctx);
    const text = result.content[0]?.text ?? "";
    expect(text).toContain("t1");
    expect(text).toContain("session: /w/s.jsonl");
    expect(text).toContain("1 state_changed queued → running");
    expect(text).toContain("2 activity bash ls");
    void seeded;

    await expect(runTool(mock.tools.get("RemoteAgentGet")!, { id: "ghost" }, ctx)).rejects.toThrow(/not found/);
  });
});

describe("mutation tools", () => {
  test("RemoteAgentSteer sends the message; 409 throws", async () => {
    const ctx = createMockCtx();
    server.seedTask({ id: "t1", status: "running" });
    const result = await runTool(mock.tools.get("RemoteAgentSteer")!, { id: "t1", message: "do X instead" }, ctx);
    expect(result.content[0]?.text).toContain("do X instead");
    expect(server.steerMessages).toEqual([{ id: "t1", message: "do X instead" }]);

    server.seedTask({ id: "t-done", status: "completed" });
    await expect(
      runTool(mock.tools.get("RemoteAgentSteer")!, { id: "t-done", message: "x" }, ctx),
    ).rejects.toThrow(/not steerable/);
  });

  test("RemoteAgentKill cancels the task", async () => {
    const ctx = createMockCtx();
    server.seedTask({ id: "t1", status: "running" });
    const result = await runTool(mock.tools.get("RemoteAgentKill")!, { id: "t1" }, ctx);
    expect(result.content[0]?.text).toContain("Kill requested");
    expect(server.tasks.get("t1")!.status).toBe("canceled");
  });
});

describe("/remote-agents command", () => {
  function handler(): (args: string, ctx: unknown) => Promise<void> | void {
    return mock.commands.get("remote-agents")!.handler;
  }

  test("bare /remote-agents lists tasks via notify", async () => {
    const ctx = createMockCtx();
    server.seedTask({ id: "t1", name: "scout", status: "running", startedAt: Date.now() - 10_000 });
    await handler()("", ctx.ctx);
    const info = ctx.notifications.find((n) => n.level === "info");
    expect(info?.message).toContain("t1");
  });

  test("steer/kill/logs routes hit the client", async () => {
    const ctx = createMockCtx();
    server.seedTask({ id: "t1", status: "running" });
    server.pushEvent("t1", "state_changed", { from: "queued", to: "running" });
    server.pushEvent("t1", "activity", { toolName: "bash", detail: "ls" });

    await handler()("steer t1 change course now", ctx.ctx);
    expect(server.steerMessages).toEqual([{ id: "t1", message: "change course now" }]);

    await handler()("kill t1", ctx.ctx);
    expect(server.tasks.get("t1")!.status).toBe("canceled");

    await handler()("logs t1", ctx.ctx);
    const logs = ctx.notifications.filter((n) => n.level === "info").at(-1);
    expect(logs?.message).toContain("1 state_changed queued → running");
    expect(logs?.message).toContain("2 activity bash ls");
  });

  test("attach subscribes a running task for delivery here", async () => {
    const ctx = createMockCtx();
    server.seedTask({ id: "t1", status: "running", startedAt: Date.now() });
    await handler()("attach t1", ctx.ctx);
    await waitFor(() => server.requests.some((r) => r.path === "/tasks/t1/stream?since=0"));
    server.pushEvent("t1", "state_changed", { from: "running", to: "completed" });
    await waitFor(() => mock.sent.length === 1);
  });

  test("server-hitting verbs warn with the connect hint while disconnected", async () => {
    deadServerEnv();
    const ctx = createMockCtx();
    await handler()("", ctx.ctx); // bare = list
    expect(ctx.notifications.some((n) => n.message.includes("not connected"))).toBe(true);
    await handler()("steer t1 hi", ctx.ctx);
    expect(ctx.notifications.some((n) => n.message.includes("not connected"))).toBe(true);
  });

  test("unknown verbs and bad args notify a warning", async () => {
    const ctx = createMockCtx();
    await handler()("bogus", ctx.ctx);
    expect(ctx.notifications.some((n) => n.level === "warning")).toBe(true);
    await handler()("steer t1", ctx.ctx);
    expect(ctx.notifications.some((n) => n.message.includes("usage: /remote-agents steer"))).toBe(true);
  });
});

describe("result renderer", () => {
  test("boxes the message at the requested width", () => {
    const renderer = mock.renderers.get("remote_agent_result")!;
    const component = renderer(
      {
        customType: "remote_agent_result",
        content: 'Remote agent "builder" completed in 45s.\n\nBuild passed.\n\n[t1 · logs: /remote-agents logs t1]',
        display: true,
        details: { taskId: "t1", name: "builder", status: "completed", elapsedMs: 45_000, sessionFile: "/w/s.jsonl" },
      },
      { expanded: true },
      undefined,
    ) as RenderedComponent | undefined;
    expect(component).toBeDefined();
    const lines = component!.render(60);
    expect(lines[0]).toContain("remote agent: builder · completed · 45s");
    for (const line of lines) expect(line.length).toBe(60);
    expect(lines.join("\n")).toContain("Build passed.");
  });

  test("truncates oversized result text to the render limit", () => {
    const renderer = mock.renderers.get("remote_agent_result")!;
    const component = renderer(
      { customType: "remote_agent_result", content: "x".repeat(3_000), display: true, details: undefined },
      { expanded: true },
      undefined,
    ) as RenderedComponent;
    const rendered = component.render(80);
    const body = rendered.slice(1, -1).join("\n");
    expect(body.length).toBeLessThanOrEqual(2_001);
  });

  test("handles non-string content defensively", () => {
    const renderer = mock.renderers.get("remote_agent_result")!;
    const component = renderer(
      { customType: "remote_agent_result", content: [{ type: "image" }], display: true, details: undefined },
      { expanded: false },
      undefined,
    ) as RenderedComponent;
    expect(component.render(40).join("\n")).toContain("non-text result content");
  });
});
