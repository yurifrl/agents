import { existsSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, test } from "bun:test";
import { OmpRpc, type RpcEvent } from "@remote-agents/rpc-driver";
import { renderBridgeScript } from "../src";
import { FAKE_OMP_PATH, connectChannel, exitedWithin, sleep, waitForEvent, waitForPort } from "./helpers";

function spawnBridge(args: string[]) {
  return Bun.spawn([process.execPath, ...args], {
    stdin: "ignore",
    stdout: "ignore",
    stderr: "pipe",
  });
}

describe("renderBridgeScript", () => {
  test("deterministic, self-contained, no interpolation", () => {
    const first = renderBridgeScript();
    const second = renderBridgeScript();
    expect(first).toBe(second);
    expect(first).toContain("Bun.listen");
    expect(first).toContain("--port-file");
    expect(first).toContain('type === "ready"'); // ready capture + replay
    expect(first).not.toContain("${");
    expect(first.startsWith("// remote-agents")).toBe(true);
  });
});

describe("bridge round-trip", () => {
  test("TCP transport: prompt→result, disconnect, reconnect, same live worker", async () => {
    const dir = mkdtempSync(join(tmpdir(), "ra-bridge-"));
    const portFile = join(dir, "port");
    await Bun.write(join(dir, "bridge.mjs"), renderBridgeScript());

    // Fake exits after its 2nd settle so the test can watch the bridge follow.
    const bridge = spawnBridge([
      join(dir, "bridge.mjs"),
      "--port-file",
      portFile,
      "--",
      process.execPath,
      FAKE_OMP_PATH,
      "--session-dir",
      dir,
      "--exit-after-settled",
      "2",
    ]);

    try {
      const port = await waitForPort(portFile, 15_000);
      expect(port).toBeGreaterThan(0);

      // --- first client: normal session -------------------------------
      const rpc1 = new OmpRpc(await connectChannel(port));
      const ready1 = await rpc1.start();
      expect(ready1.protocolVersion).toBe(1);
      expect(ready1.sessionFile).toContain(dir);

      const events1: RpcEvent[] = [];
      rpc1.onEvent((event) => events1.push(event));
      await rpc1.prompt("first bridge prompt");
      await waitForEvent(events1, "agent_settled");
      expect((await rpc1.getLastAssistantText()).text).toBe("FAKE_RESULT=first bridge prompt");
      await rpc1.close(); // disconnect — worker must keep running

      // --- reconnect: same live worker, fresh handshake ----------------
      const rpc2 = new OmpRpc(await connectChannel(port));
      await rpc2.start(); // bridge replays the captured ready frame
      const state = await rpc2.getState();
      expect(state.isStreaming).toBe(false);
      expect(state.messageCount).toBe(1); // same worker: prompt #1 counted

      const events2: RpcEvent[] = [];
      rpc2.onEvent((event) => events2.push(event));
      await rpc2.prompt("second bridge prompt");
      await waitForEvent(events2, "agent_settled");
      expect((await rpc2.getLastAssistantText()).text).toBe("FAKE_RESULT=second bridge prompt");
      await rpc2.close();

      // --- worker settles twice → exits → bridge exits, port file gone -
      expect(await exitedWithin(bridge.exited, 15_000)).toBe(0);
      await sleep(50);
      expect(existsSync(portFile)).toBe(false);
    } finally {
      try {
        bridge.kill("SIGKILL");
      } catch {
        // Already exited.
      }
    }
  }, 60_000);

  test("bridge exits with the worker's code and cleans the port file (no client ever)", async () => {
    const dir = mkdtempSync(join(tmpdir(), "ra-bridge2-"));
    const portFile = join(dir, "port");
    await Bun.write(join(dir, "bridge.mjs"), renderBridgeScript());

    // --resume-crash --resume: the fake exits(1) right after ready.
    const bridge = spawnBridge([
      join(dir, "bridge.mjs"),
      "--port-file",
      portFile,
      "--",
      process.execPath,
      FAKE_OMP_PATH,
      "--resume-crash",
      "--resume",
      "/tmp/unused.jsonl",
    ]);

    expect(await exitedWithin(bridge.exited, 15_000)).toBe(1);
    // Prove the exit came from the worker crash, not a missing script.
    expect(await new Response(bridge.stderr).text()).toContain("simulated resume crash");
    await sleep(50);
    expect(existsSync(portFile)).toBe(false);
  }, 30_000);
});
