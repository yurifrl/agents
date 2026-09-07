import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { API, isTerminalStatus, type Task } from "@remote-agents/protocol";
import {
  captureSse,
  cleanupDir,
  makeStateDir,
  startTestServer,
  stdioFakeOmpPath,
  waitUntil,
  type TestServer,
} from "./helpers";

const fakePath = await stdioFakeOmpPath();

async function postJson(url: string, body: unknown): Promise<Response> {
  return fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

describe("HTTP API (fast fake: CRUD + replay)", () => {
  let server: TestServer;
  let dir = "";

  beforeAll(async () => {
    dir = makeStateDir();
    server = await startTestServer({ stateDir: dir, fakePath });
  });
  afterAll(async () => {
    await server.cleanup();
    cleanupDir(dir);
  });

  const input = { owner: "alice", name: "task one", task: "run the thing", cwd: "/tmp/ws" };

  async function createAndAwait(overrides: Partial<typeof input> = {}): Promise<Task> {
    const created = await postJson(`${server.baseUrl}${API.tasks}`, {
      ...input,
      cwd: dir, // must exist: the worker spawns with this working directory
      ...overrides,
    });
    expect(created.status).toBe(201);
    const task = (await created.json()) as Task;
    await waitUntil(() => {
      const current = server.app.store.get(task.id);
      return current && isTerminalStatus(current.status) ? current : undefined;
    });
    return server.app.store.get(task.id) as Task;
  }

  test("health reports counts, surface and version", async () => {
    await createAndAwait({ owner: "health-check", name: "health task" });
    const res = await fetch(`${server.baseUrl}${API.health}`);
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      ok: boolean;
      surface: string;
      version: string;
      uptimeMs: number;
      tasks: { active: number; total: number };
    };
    expect(body.ok).toBe(true);
    expect(body.surface).toBe("process");
    expect(body.version).toBe("0.1.0");
    expect(body.uptimeMs).toBeGreaterThanOrEqual(0);
    expect(body.tasks.total).toBe(1);
    expect(body.tasks.active).toBe(0); // the task already completed
  });

  test("POST /tasks validates input", async () => {
    expect((await postJson(`${server.baseUrl}${API.tasks}`, {})).status).toBe(400);
    expect((await postJson(`${server.baseUrl}${API.tasks}`, { ...input, name: " " })).status).toBe(400);
    expect((await postJson(`${server.baseUrl}${API.tasks}`, { ...input, task: "" })).status).toBe(400);
    expect((await postJson(`${server.baseUrl}${API.tasks}`, { ...input, owner: "" })).status).toBe(400);
    expect((await postJson(`${server.baseUrl}${API.tasks}`, { ...input, cwd: "relative/path" })).status).toBe(400);
    const malformed = await fetch(`${server.baseUrl}${API.tasks}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: "not json",
    });
    expect(malformed.status).toBe(400);
  });

  test("POST /tasks returns 201 queued; GET /tasks filters by owner/unacknowledged", async () => {
    const created = await postJson(`${server.baseUrl}${API.tasks}`, {
      ...input,
      owner: "bob",
      name: "task two",
    });
    expect(created.status).toBe(201);
    const task = (await created.json()) as Task;
    expect(task.status).toBe("queued");
    expect(task.acknowledged).toBe(false);
    expect(task.task).toBe("run the thing"); // the body text is included

    const all = (await (await fetch(`${server.baseUrl}${API.tasks}`)).json()) as Task[];
    expect(all.length).toBe(2);
    const mine = (await (await fetch(`${server.baseUrl}${API.tasks}?owner=bob`)).json()) as Task[];
    expect(mine.map((t) => t.id)).toEqual([task.id]);
    const unacked = (await (
      await fetch(`${server.baseUrl}${API.tasks}?owner=bob&unacknowledged=1`)
    ).json()) as Task[];
    expect(unacked.map((t) => t.id)).toEqual([task.id]);
  });

  test("GET /tasks/:id embeds recentEvents; 404 for unknown ids", async () => {
    const task = await createAndAwait();
    const detail = await fetch(`${server.baseUrl}${API.task(task.id)}`);
    expect(detail.status).toBe(200);
    const body = (await detail.json()) as Task & {
      recentEvents: Array<{ seq: number; type: string; data: Record<string, unknown> }>;
    };
    const expected = server.app.store.recentEvents(task.id);
    expect(body.recentEvents.map((e) => e.seq)).toEqual(expected.map((e) => e.seq));
    expect(body.recentEvents.length).toBeLessThanOrEqual(10);
    expect(body.recentEvents.at(-1)?.type).toBe("state_changed");

    const missing = await fetch(`${server.baseUrl}${API.task("no-such-id")}`);
    expect(missing.status).toBe(404);
    const errBody = (await missing.json()) as { error: string };
    expect(errBody.error).toContain("not found");
  });

  test("ack is idempotent and drives the unacknowledged filter", async () => {
    const task = await createAndAwait({ owner: "carol", name: "ack me" });
    const first = await postJson(`${server.baseUrl}${API.ack(task.id)}`, {});
    expect(first.status).toBe(200);
    expect(((await first.json()) as Task).acknowledged).toBe(true);
    const second = await postJson(`${server.baseUrl}${API.ack(task.id)}`, {});
    expect(second.status).toBe(200); // idempotent by design
    const unacked = (await (
      await fetch(`${server.baseUrl}${API.tasks}?owner=carol&unacknowledged=1`)
    ).json()) as Task[];
    expect(unacked).toEqual([]);
    const unknown = await postJson(`${server.baseUrl}${API.ack("no-such-id")}`, {});
    expect(unknown.status).toBe(404);
  });

  test("attach returns unacknowledged tasks of the owner plus server info", async () => {
    const t1 = await createAndAwait({ owner: "dave", name: "attach one" });
    await createAndAwait({ owner: "dave", name: "attach two" });
    await postJson(`${server.baseUrl}${API.ack(t1.id)}`, {});

    const res = await postJson(`${server.baseUrl}${API.attach}`, { owner: "dave" });
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      tasks: Task[];
      server: { uptimeMs: number; surface: string; version: string };
    };
    expect(body.tasks.length).toBe(1);
    expect(body.tasks[0]?.name).toBe("attach two");
    expect(body.server.surface).toBe("process");
    expect(body.server.version).toBe("0.1.0");
    expect(body.server.uptimeMs).toBeGreaterThanOrEqual(0);

    expect((await postJson(`${server.baseUrl}${API.attach}`, {})).status).toBe(400);
  });

  test("terminal tasks reject steer and cancel with 409", async () => {
    const task = await createAndAwait({ owner: "erin", name: "done already" });
    const steer = await postJson(`${server.baseUrl}${API.steer(task.id)}`, { message: "late" });
    expect(steer.status).toBe(409);
    const cancel = await postJson(`${server.baseUrl}${API.cancel(task.id)}`, {});
    expect(cancel.status).toBe(409);
  });

  test("steer with an empty message is 400; unknown id is 404", async () => {
    const task = await createAndAwait();
    expect((await postJson(`${server.baseUrl}${API.steer(task.id)}`, { message: "" })).status).toBe(400);
    expect((await postJson(`${server.baseUrl}${API.steer("no-such-id")}`, { message: "x" })).status).toBe(404);
  });

  test("SSE: since=0 replays the full history then closes on a terminal task", async () => {
    const task = await createAndAwait();
    const capture = await captureSse(`${server.baseUrl}${API.stream(task.id, 0)}`);
    expect(capture.closed).toBe(true);
    const expectedSeqs = server.app.store.eventsSince(task.id, 0).map((e) => e.seq);
    expect(capture.frames.map((f) => f.seq)).toEqual(expectedSeqs);
    expect(capture.frames.length).toBeGreaterThan(0);
    const last = capture.frames.at(-1);
    expect(last?.type).toBe("state_changed");
    expect(last && "to" in last.data ? last.data.to : "").toBe("completed");
    for (const frame of capture.frames) {
      expect(frame.taskId).toBe(task.id);
      expect(frame.ts).toBeGreaterThan(0);
    }
  }, 20_000);

  test("SSE: since beyond the last seq yields an empty stream that closes", async () => {
    const task = await createAndAwait();
    const capture = await captureSse(`${server.baseUrl}${API.stream(task.id, 999_999)}`);
    expect(capture.closed).toBe(true);
    expect(capture.frames).toEqual([]);
  }, 20_000);

  test("SSE: 404 for unknown task, 400 for a negative since", async () => {
    const missing = await fetch(`${server.baseUrl}/tasks/no-such-id/stream?since=0`);
    expect(missing.status).toBe(404);
    const task = await createAndAwait();
    const badSince = await fetch(`${server.baseUrl}${API.stream(task.id, -5)}`);
    expect(badSince.status).toBe(400);
  }, 20_000);

  test("unknown routes and methods 404", async () => {
    expect((await fetch(`${server.baseUrl}/nope`)).status).toBe(404);
    expect((await fetch(`${server.baseUrl}${API.task("x")}`, { method: "DELETE" })).status).toBe(404);
  });
});

describe("HTTP API (delayed fake: steer/cancel + live SSE)", () => {
  let server: TestServer;
  let dir = "";

  beforeAll(async () => {
    dir = makeStateDir();
    server = await startTestServer({
      stateDir: dir,
      fakePath,
      extraArgs: ["--settled-delay", "700"],
    });
  });
  afterAll(async () => {
    await server.cleanup();
    cleanupDir(dir);
  });

  const input = { owner: "alice", name: "live task", task: "long running work", cwd: "/tmp/ws" };

  async function createRunning(): Promise<Task> {
    const created = await postJson(`${server.baseUrl}${API.tasks}`, { ...input, cwd: dir });
    expect(created.status).toBe(201);
    const task = (await created.json()) as Task;
    await waitUntil(() => (server.app.store.get(task.id)?.status === "running" ? true : undefined));
    return task;
  }

  test("steer returns 202 while running", async () => {
    const task = await createRunning();
    const res = await postJson(`${server.baseUrl}${API.steer(task.id)}`, { message: "go left" });
    expect(res.status).toBe(202);
    const body = (await res.json()) as Task;
    expect(body.status).toBe("running");
    await waitUntil(() => {
      const current = server.app.store.get(task.id);
      return current && isTerminalStatus(current.status) ? current : undefined;
    });
  }, 15_000);

  test("cancel returns the canceled task; a second cancel is 409", async () => {
    const task = await createRunning();
    const res = await postJson(`${server.baseUrl}${API.cancel(task.id)}`, {});
    expect(res.status).toBe(200);
    const body = (await res.json()) as Task;
    expect(body.status).toBe("canceled");
    expect(body.stopRequested).toBe(true);
    const second = await postJson(`${server.baseUrl}${API.cancel(task.id)}`, {});
    expect(second.status).toBe(409);
  }, 15_000);

  test("SSE delivers live events after `since` and closes on completion", async () => {
    const task = await createRunning();
    const lastSeq = server.app.store.recentEvents(task.id, 1)[0]?.seq ?? 0;
    const capture = await captureSse(`${server.baseUrl}/tasks/${task.id}/stream?since=${lastSeq}`, 20_000);
    expect(capture.closed).toBe(true);
    expect(capture.frames.length).toBeGreaterThan(0);
    expect(capture.frames[0] ? capture.frames[0].seq > lastSeq : false).toBe(true);
    const last = capture.frames.at(-1);
    expect(last?.type).toBe("state_changed");
    expect(last && "to" in last.data ? last.data.to : "").toBe("completed");
  }, 20_000);
});
