import { describe, expect, test } from "bun:test";
import type { CreateTaskInput } from "@remote-agents/protocol";
import { Supervisor } from "../src/supervisor";
import { TaskStore } from "../src/store";
import { FakeSurface } from "./fake-surface";
import { cleanupDir, makeStateDir, tcpFakeOmpPath, waitUntil } from "./helpers";

// Adoption tests need a TCP-speaking worker (the shared fake-omp is stdio
// only), so they use the local fake's --tcp mode, which emulates the herdr
// bridge: ready frame per connection, events broadcast to all clients.
const localFake = tcpFakeOmpPath();
const input: CreateTaskInput = { owner: "alice", name: "adopt", task: "hello adoption", cwd: "/tmp" };

describe("re-adoption on boot", () => {
  test("tcp worker survives restart: new Supervisor re-attaches and completes", async () => {
    const dir = makeStateDir();
    const surface1 = new FakeSurface({
      fakePath: localFake,
      kind: "herdr",
      extraArgs: ["--settled-delay", "1500"],
      stopKillsProc: false, // worker must survive the abandoned "crashed" server
    });
    const store1 = new TaskStore(dir);
    const sup1 = new Supervisor(store1, surface1, { maxTimeMs: 60_000 });
    const task = sup1.create(input);
    await waitUntil(() => (store1.get(task.id)?.status === "running" ? true : undefined), 20_000);
    store1.saveNow();
    // From here sup1 is abandoned mid-flight: no shutdown, no cancel — the
    // "crashed" server. Its TCP worker keeps running in the background.

    const store2 = new TaskStore(dir);
    expect(store2.get(task.id)?.status).toBe("running"); // persisted state round-trips
    expect(store2.get(task.id)?.worker?.port).toBeGreaterThan(0);

    const surface2 = new FakeSurface({ fakePath: localFake, kind: "herdr" });
    const sup2 = new Supervisor(store2, surface2, { maxTimeMs: 60_000, adoptionPollMs: 200 });
    try {
      const adopted = await sup2.adoptExisting();
      expect(adopted).toBe(1);

      const done = await sup2.whenSettled(task.id);
      expect(done?.status).toBe("completed");
      expect(done?.result).toBe("FAKE_RESULT=hello adoption");

      const readopted = store2
        .eventsSince(task.id, 0)
        .some((e) => e.type === "log" && "line" in e.data && e.data.line === "readopted");
      expect(readopted).toBe(true);
    } finally {
      await surface1.killAll(); // reap the surviving worker
      await surface2.killAll();
      cleanupDir(dir);
    }
  }, 30_000);

  test("live process worker becomes a failed orphan with a resume hint", async () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const task = store.create(input, "process");
      // The test runner's own pid is guaranteed alive — and never killed here.
      store.update(task.id, { status: "running", startedAt: Date.now(), worker: { pid: process.pid } });
      const sup = new Supervisor(store, new FakeSurface({ fakePath: localFake }), {});
      const adopted = await sup.adoptExisting();
      expect(adopted).toBe(0);
      const after = store.get(task.id);
      expect(after?.status).toBe("failed");
      expect(after?.error).toBe(`orphaned worker (pid ${process.pid}) — resume via session file`);
    } finally {
      cleanupDir(dir);
    }
  });

  test("dead process worker is marked failed", async () => {
    const dir = makeStateDir();
    try {
      const doomed = Bun.spawn(["bun", "-e", "process.exit(0)"]);
      await doomed.exited;
      const store = new TaskStore(dir);
      const task = store.create(input, "process");
      store.update(task.id, { status: "starting", startedAt: Date.now(), worker: { pid: doomed.pid } });
      const sup = new Supervisor(store, new FakeSurface({ fakePath: localFake }), {});
      const adopted = await sup.adoptExisting();
      expect(adopted).toBe(0);
      const after = store.get(task.id);
      expect(after?.status).toBe("failed");
      expect(after?.error).toBe(`worker process (pid ${doomed.pid}) exited before completion`);
    } finally {
      cleanupDir(dir);
    }
  });

  test("queued task without a worker record fails with a clear message", async () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const task = store.create(input, "process"); // stays queued, never spawned
      const sup = new Supervisor(store, new FakeSurface({ fakePath: localFake }), {});
      const adopted = await sup.adoptExisting();
      expect(adopted).toBe(0);
      const after = store.get(task.id);
      expect(after?.status).toBe("failed");
      expect(after?.error).toContain("worker record missing");
    } finally {
      cleanupDir(dir);
    }
  });

  test("terminal tasks are not re-adopted", async () => {
    const dir = makeStateDir();
    try {
      const store = new TaskStore(dir);
      const task = store.create(input, "process");
      store.update(task.id, { status: "completed", endedAt: Date.now(), result: "done" });
      const sup = new Supervisor(store, new FakeSurface({ fakePath: localFake }), {});
      const adopted = await sup.adoptExisting();
      expect(adopted).toBe(0);
      expect(store.get(task.id)?.status).toBe("completed");
    } finally {
      cleanupDir(dir);
    }
  });

  test("unreachable herdr bridge fails the task", async () => {
    const dir = makeStateDir();
    try {
      // Bind and immediately close a port to get a guaranteed-closed number.
      const probe = Bun.listen({
        hostname: "127.0.0.1",
        port: 0,
        socket: {
          open() {},
          data() {},
          close() {},
        },
      });
      const port = probe.port;
      probe.stop(true);

      const store = new TaskStore(dir);
      const task = store.create(input, "herdr");
      store.update(task.id, { status: "running", startedAt: Date.now(), worker: { port } });
      const sup = new Supervisor(store, new FakeSurface({ fakePath: localFake, kind: "herdr" }), {
        adoptionConnectMs: 500,
      });
      const adopted = await sup.adoptExisting();
      expect(adopted).toBe(0);
      const after = store.get(task.id);
      expect(after?.status).toBe("failed");
      expect(after?.error).toContain(`worker bridge unreachable on port ${port}`);
    } finally {
      cleanupDir(dir);
    }
  }, 15_000);
});
