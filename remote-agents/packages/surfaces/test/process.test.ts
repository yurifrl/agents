import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, test } from "bun:test";
import { OmpRpc, type RpcEvent } from "@remote-agents/rpc-driver";
import type { WorkerSpec } from "@remote-agents/protocol";
import { ProcessSurface, SurfaceError, createSurface } from "../src";
import { FAKE_OMP_PATH, sleep, waitForEvent } from "./helpers";

function makeSpec(overrides: Partial<WorkerSpec> = {}): WorkerSpec {
  return {
    taskId: "task-proc-1",
    name: "process surface test",
    cwd: "/tmp",
    sessionDir: mkdtempSync(join(tmpdir(), "ra-proc-")),
    ompBin: FAKE_OMP_PATH,
    maxTimeS: 600,
    ...overrides,
  };
}

function pidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

describe("ProcessSurface", () => {
  test("spawn → handshake → prompt → settled → result over real stdio", async () => {
    const spec = makeSpec({ model: "test-model" });
    const surface = new ProcessSurface();
    const worker = await surface.start(spec);

    expect(worker.meta.pid).toBeGreaterThan(0);
    const rpc = new OmpRpc(worker.channel);
    const ready = await rpc.start();
    expect(ready.protocolVersion).toBe(1);
    expect(ready.sessionFile).toContain(spec.sessionDir); // --session-dir propagated

    const state = await rpc.getState();
    expect(state.isStreaming).toBe(false);
    expect(state.taskId).toBe("task-proc-1"); // REMOTE_AGENTS_TASK_ID env reached the worker
    expect(state.model).toBe("test-model"); // --model flag reached the worker

    const events: RpcEvent[] = [];
    rpc.onEvent((event) => events.push(event));
    await rpc.prompt("run the process test");
    await waitForEvent(events, "agent_settled");
    const last = await rpc.getLastAssistantText();
    expect(last.text).toBe("FAKE_RESULT=run the process test");

    // stop(): SIGTERM → 3s grace → SIGKILL; the pid must be gone afterwards.
    const pid = worker.meta.pid as number;
    await worker.stop();
    expect(pidAlive(pid)).toBe(false);
  });

  test("stop() is idempotent and safe on an already-stopped worker", async () => {
    const surface = new ProcessSurface();
    const worker = await surface.start(makeSpec());
    await worker.stop();
    await worker.stop();
    const rpc = new OmpRpc(worker.channel);
    await rpc.close();
  });

  test("channel close ends stdin so the worker exits cleanly", async () => {
    const surface = new ProcessSurface();
    const worker = await surface.start(makeSpec());
    await worker.channel.close();
    // Worker sees EOF on stdin and exits on its own — no SIGTERM needed. The
    // pid lives behind the surface boundary (meta.pid only), so teardown is
    // observed by liveness polling; fake timers cannot control a real child.
    const pid = worker.meta.pid as number;
    for (let i = 0; i < 100 && pidAlive(pid); i++) {
      await sleep(20);
    }
    expect(pidAlive(pid)).toBe(false);
  });

  test("createSurface('process') returns a process-kind surface", () => {
    const surface = createSurface("process", {
      ompBin: "omp",
      bridgePath: "/tmp/ra/bridge.mjs",
      stateDir: "/tmp/ra/state",
    });
    expect(surface.kind).toBe("process");
  });

  test("createSurface rejects unknown kinds", () => {
    expect(() =>
      createSurface("teleport" as never, {
        ompBin: "omp",
        bridgePath: "/tmp/ra/bridge.mjs",
        stateDir: "/tmp/ra/state",
      }),
    ).toThrow(/unknown surface kind/);
    expect(SurfaceError).toBeDefined();
  });
});
