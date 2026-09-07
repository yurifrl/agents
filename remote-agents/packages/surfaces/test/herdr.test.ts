import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, describe, expect, test } from "bun:test";
import { OmpRpc, type RpcEvent } from "@remote-agents/rpc-driver";
import type { WorkerSpec } from "@remote-agents/protocol";
import { HerdrSurface, createSurface } from "../src";
import { FAKE_OMP_PATH, sleep, waitForEvent } from "./helpers";

/**
 * Live-server guard (CONTRACT.md §6): the suite is skipped entirely unless a
 * herdr server is running locally. Everything below drives the REAL herdr
 * CLI in a throwaway workspace and closes that workspace best-effort.
 */
function herdrServerRunning(): boolean {
  try {
    const proc = Bun.spawnSync(["herdr", "status"], {
      stdout: "pipe",
      stderr: "pipe",
      stdin: "ignore",
    });
    return proc.exitCode === 0 && /status: running/.test(proc.stdout.toString());
  } catch {
    return false;
  }

}

const hasServer = herdrServerRunning();
const workspaceName = `remote-agents-test-${process.pid}`;

describe.skipIf(!hasServer)("HerdrSurface (live herdr server)", () => {
  const surface = new HerdrSurface({ workspace: workspaceName });
  const workspaceIds = new Set<string>();
  afterAll(async () => {
    // Best-effort cleanup: close the throwaway test workspaces. A workspace
    // close issued right after pane close can lose a server-side race, so
    // retry once after a real settle delay (integration-test exception to
    // fake timers: this waits on the actual herdr server).
    for (const id of workspaceIds) {
      for (let attempt = 0; attempt < 2; attempt++) {
        try {
          const proc = Bun.spawnSync(["herdr", "workspace", "close", id], {
            stdout: "pipe",
            stderr: "pipe",
            stdin: "ignore",
          });
          if (proc.exitCode === 0) break;
          if (attempt === 1) {
            console.warn(
              `cleanup: herdr workspace close ${id} failed: ${proc.stderr.toString().trim()}`,
            );
          }
        } catch (err) {
          console.warn(`cleanup: herdr workspace close ${id} threw: ${String(err)}`);
          break;
        }
        await sleep(500);
      }
    }
  });

  function makeSpec(taskId: string): WorkerSpec {
    return {
      taskId,
      name: "herdr surface test",
      cwd: "/tmp",
      sessionDir: mkdtempSync(join(tmpdir(), "ra-herdr-")),
      ompBin: FAKE_OMP_PATH,
      maxTimeS: 600,
    };
  }

  test("createSurface('herdr') wires opts through", () => {
    const surfaceFromFactory = createSurface("herdr", {
      ompBin: "omp",
      bridgePath: "/tmp/ra/bridge.mjs",
      stateDir: "/tmp/ra/state",
      workspace: workspaceName,
    });
    expect(surfaceFromFactory.kind).toBe("herdr");
  });

  test("start → handshake → prompt → result over the pane bridge → stop", async () => {
    const spec = makeSpec(`task-herdr-${process.pid}`);
    const worker = await surface.start(spec);

    expect(worker.meta.paneId).toMatch(/^[^:]+:p\d+$/);
    expect(typeof worker.meta.workspaceId).toBe("string");
    expect(worker.meta.port).toBeGreaterThan(0);
    expect(worker.meta.portFile).toBe(join(spec.sessionDir, "port"));
    workspaceIds.add(worker.meta.workspaceId as string);

    const rpc = new OmpRpc(worker.channel);
    const ready = await rpc.start();
    expect(ready.protocolVersion).toBe(1);

    // env vars reached the pane via the `env` prefix in the pane command.
    const state = await rpc.getState();
    expect(state.taskId).toBe(spec.taskId);

    const events: RpcEvent[] = [];
    rpc.onEvent((event) => events.push(event));
    await rpc.prompt("hello herdr surface");
    await waitForEvent(events, "agent_settled", 15_000);
    expect((await rpc.getLastAssistantText()).text).toBe("FAKE_RESULT=hello herdr surface");

    await worker.stop(); // socket close + herdr pane close
  }, 120_000);

  test("second start reuses the existing workspace", async () => {
    const workerA = await surface.start(makeSpec(`task-herdr-a-${process.pid}`));
    const workerB = await surface.start(makeSpec(`task-herdr-b-${process.pid}`));
    workspaceIds.add(workerA.meta.workspaceId as string);
    workspaceIds.add(workerB.meta.workspaceId as string);
    expect(workerB.meta.workspaceId).toBe(workerA.meta.workspaceId);
    expect(workerB.meta.paneId).not.toBe(workerA.meta.paneId);
    await workerA.stop();
    await workerB.stop();
  }, 180_000);
});

describe("HerdrSurface availability guard", () => {
  test("a missing herdr CLI yields SurfaceError('herdr_unavailable')", async () => {
    const surface = new HerdrSurface({
      herdrBin: "definitely-not-a-real-herdr-binary",
      workspace: "unused",
    });
    await expect(
      surface.start({
        taskId: "task-x",
        name: "n",
        cwd: "/tmp",
        sessionDir: mkdtempSync(join(tmpdir(), "ra-herdr-missing-")),
        ompBin: FAKE_OMP_PATH,
        maxTimeS: 60,
      }),
    ).rejects.toMatchObject({ name: "SurfaceError", code: "herdr_unavailable" });
  });
});
