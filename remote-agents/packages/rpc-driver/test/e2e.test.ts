import { describe, expect, test } from "bun:test";
import { OmpRpc, type RpcEvent } from "../src/client";
import { FAKE_OMP_PATH, spawnFakeOmp, waitForEvent } from "./helpers";

describe("fake-omp end-to-end over a real subprocess", () => {
  test("ready -> prompt -> tool events -> message_end -> agent_end -> agent_settled -> result", async () => {
    const PROMPT = "Fix the login bug in the auth service today";
    const EXPECTED = `FAKE_RESULT=${PROMPT.slice(0, 32)}`;
    const fake = spawnFakeOmp(["--tool", "bash"]);
    const rpc = new OmpRpc(fake.channel);
    const events: RpcEvent[] = [];
    rpc.onEvent((event) => events.push(event));

    const ready = await rpc.start();
    expect(ready.protocolVersion).toBe(1);
    expect(ready.sessionFile).toMatch(/fake-omp-\d+\.jsonl$/);

    const prompt = rpc.prompt(PROMPT);
    const settled = await waitForEvent(events, "agent_settled");
    await prompt; // the response frame precedes the settled event

    const types = events.map((event) => event.type);
    expect(types).toEqual([
      "agent_start",
      "tool_execution_start",
      "tool_execution_end",
      "message_end",
      "agent_end",
      "agent_settled",
    ]);

    const messageEnd = events[types.indexOf("message_end")] as {
      message?: { role?: string; content?: Array<{ type?: string; text?: string }> };
    };
    const text = messageEnd.message?.content?.find((part) => part.type === "text")?.text;
    expect(text).toBe(EXPECTED); // first 32 chars of the prompt

    const state = await rpc.getState();
    expect(state.isStreaming).toBe(false);
    expect(state.messageCount).toBe(1);

    const last = await rpc.getLastAssistantText();
    expect(last.text).toBe(EXPECTED);
    expect(settled).toBeDefined();

    await rpc.close();
    expect(await fake.exited).toBe(0);
  });

  test("short prompts are not truncated in FAKE_RESULT", async () => {
    const fake = spawnFakeOmp();
    const rpc = new OmpRpc(fake.channel);
    const events: RpcEvent[] = [];
    rpc.onEvent((event) => events.push(event));
    await rpc.start();
    await rpc.prompt("short task");
    await waitForEvent(events, "agent_settled");
    const last = await rpc.getLastAssistantText();
    expect(last.text).toBe("FAKE_RESULT=short task");
    await rpc.close();
    await fake.exited;
  });

  test("--settled-delay defers agent_settled", async () => {
    const fake = spawnFakeOmp(["--settled-delay", "120"]);
    const rpc = new OmpRpc(fake.channel);
    const events: RpcEvent[] = [];
    rpc.onEvent((event) => events.push(event));
    await rpc.start();
    const prompt = rpc.prompt("delayed settle");
    const before = Date.now();
    await prompt;
    await waitForEvent(events, "agent_settled");
    expect(Date.now() - before).toBeGreaterThanOrEqual(100);
    await rpc.close();
    await fake.exited;
  });

  test("steer is acknowledged and abort settles immediately", async () => {
    const fake = spawnFakeOmp(["--settled-delay", "400"]);
    const rpc = new OmpRpc(fake.channel);
    const events: RpcEvent[] = [];
    rpc.onEvent((event) => events.push(event));
    await rpc.start();

    void rpc.prompt("long running task");
    await expect(rpc.steer("actually focus on tests")).resolves.toMatchObject({
      success: true,
      delivered: true,
    });

    const before = Date.now();
    await expect(rpc.abort()).resolves.toMatchObject({ success: true });
    await waitForEvent(events, "agent_settled");
    expect(Date.now() - before).toBeLessThan(350); // immediate, not the 400ms delay

    await rpc.close();
    await fake.exited;
  });

  test("--exit-after-settled 1 exits the worker after the first settle", async () => {
    const fake = spawnFakeOmp(["--exit-after-settled", "1"]);
    const rpc = new OmpRpc(fake.channel);
    const events: RpcEvent[] = [];
    rpc.onEvent((event) => events.push(event));
    await rpc.start();
    await rpc.prompt("only task");
    await waitForEvent(events, "agent_settled");
    await rpc.channelClosed; // worker exit EOFs the channel
    expect(await fake.exited).toBe(0);
  });

  test("channel close (stdin EOF) terminates the worker cleanly", async () => {
    const fake = spawnFakeOmp();
    const rpc = new OmpRpc(fake.channel);
    await rpc.start();
    await rpc.close();
    expect(await fake.exited).toBe(0);
  });

  test("--resume-crash crashes after ready only when --resume is present", async () => {
    // First spawn (no --resume): normal startup.
    const normal = spawnFakeOmp(["--resume-crash"]);
    const rpc = new OmpRpc(normal.channel);
    await rpc.start(); // ready arrived
    await rpc.close();
    expect(await normal.exited).toBe(0);

    // Resume spawn: crashes right after ready.
    const crashed = spawnFakeOmp(["--resume-crash", "--resume", "/tmp/some-session.jsonl"]);
    const rpc2 = new OmpRpc(crashed.channel, { handshakeTimeoutMs: 2_000 });
    await rpc2.start(); // ready made it out before the crash
    await rpc2.channelClosed; // exit(1) EOFs the channel
    expect(await crashed.exited).toBe(1);
    expect(await crashed.stderr()).toContain("simulated resume crash");
  });

  test("get_state echoes REMOTE_AGENTS_TASK_ID and --model", async () => {
    const fake = spawnFakeOmp(["--model", "test-model"], {
      REMOTE_AGENTS_TASK_ID: "task-env-42",
    });
    const rpc = new OmpRpc(fake.channel);
    await rpc.start();
    const state = await rpc.getState();
    expect(state.taskId).toBe("task-env-42");
    expect(state.model).toBe("test-model");
    expect(state.sessionFile).toContain("fake-omp-");
    await rpc.close();
    await fake.exited;
  });
});

describe("fake-omp fixture hygiene", () => {
  test("fixture lives at the contract path and emits ready standalone", async () => {
    expect(FAKE_OMP_PATH.endsWith("packages/rpc-driver/test/fake-omp.ts")).toBe(true);
    const proc = Bun.spawn([process.execPath, FAKE_OMP_PATH]);
    const out = await new Response(proc.stdout).text();
    const firstLine = out.split("\n")[0] ?? "";
    expect(JSON.parse(firstLine)).toMatchObject({ type: "ready", protocolVersion: 1 });
    await proc.exited; // stdin defaults to "ignore" → fake exits after ready
  });
});
