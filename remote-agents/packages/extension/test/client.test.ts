import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { ENV, type SseFrame } from "@remote-agents/protocol";
import { existsSync, readFileSync } from "node:fs";
import { rm } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { ApiError, createClient, ensureServer, resolveBaseUrl } from "../src/api.ts";
import { createFixtureServer, waitFor, type FixtureServer } from "./helpers.ts";

let server: FixtureServer;

beforeEach(async () => {
  server = await createFixtureServer();
  process.env[ENV.url] = server.url;
});

afterEach(async () => {
  await server.close();
});

describe("client round trips", () => {
  test("resolveBaseUrl falls back to localhost:8679", () => {
    const previous = process.env[ENV.url];
    delete process.env[ENV.url];
    expect(resolveBaseUrl()).toBe(`http://127.0.0.1:${8679}`);
    if (previous === undefined) delete process.env[ENV.url];
    else process.env[ENV.url] = previous;
  });

  test("health returns the fixture payload", async () => {
    const health = await createClient().health();
    expect(health.ok).toBe(true);
    expect(health.surface).toBe("process");
  });

  test("create posts a task and returns the 201 body", async () => {
    const client = createClient();
    const task = await client.create({ owner: "local", name: "build", task: "go build", cwd: "/w" });
    expect(task.id).toBe("fx1");
    expect(task.status).toBe("queued");
    expect(task.name).toBe("build");
    expect(server.requests.some((r) => r.method === "POST" && r.path === "/tasks")).toBe(true);
  });

  test("list filters by owner and get returns the task detail", async () => {
    const client = createClient();
    server.seedTask({ id: "t1", owner: "local" });
    server.seedTask({ id: "t2", owner: "someone-else" });
    const listed = await client.list("local");
    expect(listed.map((t) => t.id)).toEqual(["t1"]);
    const got = await client.get("t1");
    expect(got.id).toBe("t1");
    expect(got.recentEvents).toEqual([]);
  });

  test("attach returns owner tasks and server info", async () => {
    const client = createClient();
    server.seedTask({ id: "t1", owner: "local", status: "completed" });
    const attached = await client.attach("local");
    expect(attached.tasks.map((t) => t.id)).toEqual(["t1"]);
    expect(attached.server.surface).toBe("process");
  });

  test("steer records the message on active tasks", async () => {
    const client = createClient();
    server.seedTask({ id: "t1", status: "running" });
    const steered = await client.steer("t1", "focus on tests");
    expect(steered.status).toBe("running");
    expect(server.steerMessages).toEqual([{ id: "t1", message: "focus on tests" }]);
  });

  test("cancel and ack mutate the fixture task", async () => {
    const client = createClient();
    server.seedTask({ id: "t1", status: "running" });
    const canceled = await client.cancel("t1");
    expect(canceled.status).toBe("canceled");
    const acked = await client.ack("t1");
    expect(acked.acknowledged).toBe(true);
  });

  test("404 surfaces as ApiError with the server message", async () => {
    const client = createClient();
    const caught = await client.get("ghost").catch((error: unknown) => error);
    expect(caught).toBeInstanceOf(ApiError);
    if (caught instanceof ApiError) {
      expect(caught.status).toBe(404);
      expect(caught.message).toBe("task ghost not found");
    }
  });

  test("409 surfaces as ApiError with the server message", async () => {
    const client = createClient();
    server.seedTask({ id: "t-done", status: "completed" });
    const caught = await client.steer("t-done", "x").catch((error: unknown) => error);
    expect(caught).toBeInstanceOf(ApiError);
    if (caught instanceof ApiError) {
      expect(caught.status).toBe(409);
      expect(caught.message).toContain("not steerable");
    }
  });
});

describe("stream", () => {
  test("replays events, reconnects with the last seq, and stays live", async () => {
    const client = createClient();
    server.seedTask({ id: "t-stream" });
    server.pushEvent("t-stream", "log", { line: "one" });
    server.setStreamBehavior("t-stream", "close");
    const frames: SseFrame[] = [];
    const controller = new AbortController();
    const done = client.stream("t-stream", 0, (f) => frames.push(f), controller.signal);
    await waitFor(() => server.requests.filter((r) => r.path.includes("/stream")).length >= 2, 8_000);
    expect(server.requests.some((r) => r.path === "/tasks/t-stream/stream?since=1")).toBe(true);
    server.setStreamBehavior("t-stream", "hold");
    server.pushEvent("t-stream", "log", { line: "two" });
    await waitFor(() => frames.length >= 2, 8_000);
    expect(frames.map((f) => f.seq)).toEqual([1, 2]);
    controller.abort();
    await done;
  });

  test("stops instead of retrying when the task is gone", async () => {
    const client = createClient();
    const frames: SseFrame[] = [];
    await client.stream("ghost", 0, (f) => frames.push(f));
    expect(frames).toEqual([]);
    expect(server.requests.filter((r) => r.path.includes("/tasks/ghost/stream"))).toHaveLength(1);
  });

  test("aborts promptly on signal", async () => {
    const client = createClient();
    server.seedTask({ id: "t-hold" });
    const controller = new AbortController();
    const done = client.stream("t-hold", 0, () => undefined, controller.signal);
    await waitFor(() => server.requests.some((r) => r.path.startsWith("/tasks/t-hold/stream")));
    controller.abort();
    await done;
    expect(controller.signal.aborted).toBe(true);
  });
});

describe("ensureServer", () => {
  test("does not spawn when health already passes", async () => {
    const client = createClient();
    const before = server.requests.length;
    await ensureServer(client, { serverEntry: "/nonexistent.ts", stateDir: "/tmp/ra-healthy" });
    expect(server.requests.length).toBe(before + 1);
  });

  test("boots the entry detached when nothing listens", async () => {
    const port = await freePort();
    const stateDir = join(tmpdir(), `ra-ext-${Date.now()}-${Math.random().toString(36).slice(2)}`);
    const pidFile = join(stateDir, "entry.pid");
    const previousUrl = process.env[ENV.url];
    const previousPort = process.env[ENV.port];
    process.env[ENV.url] = `http://127.0.0.1:${port}`;
    process.env[ENV.port] = String(port);
    process.env.REMOTE_AGENTS_TEST_PID_FILE = pidFile;
    try {
      const client = createClient();
      await ensureServer(client, {
        serverEntry: join(import.meta.dir, "fixtures", "server-entry.ts"),
        stateDir,
      });
      const health = await client.health();
      expect(health.ok).toBe(true);
      expect(existsSync(join(stateDir, "logs", "server.log"))).toBe(true);
      const pid = Number(readFileSync(pidFile, "utf8"));
      expect(Number.isFinite(pid)).toBe(true);
      process.kill(pid, "SIGKILL");
      await waitFor(() => !pidAlive(pid), 2_000);
    } finally {
      if (previousUrl === undefined) delete process.env[ENV.url];
      else process.env[ENV.url] = previousUrl;
      if (previousPort === undefined) delete process.env[ENV.port];
      else process.env[ENV.port] = previousPort;
      delete process.env.REMOTE_AGENTS_TEST_PID_FILE;
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  test("homedir default state dir stays untouched by the healthy path", () => {
    expect(join(homedir(), ".remote-agents")).toContain(".remote-agents");
  });
});

async function freePort(): Promise<number> {
  for (;;) {
    const probe = Bun.serve({ port: 0, fetch: () => new Response("ok") });
    const port = probe.port;
    if (port == null) throw new Error("fixture: Bun.serve returned no port");
    await Promise.resolve(probe.stop(true));
    try {
      const rebind = Bun.serve({ port, fetch: () => new Response("ok") });
      await Promise.resolve(rebind.stop(true));
      return port;
    } catch {
      await Bun.sleep(20);
    }
  }
}

function pidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}
