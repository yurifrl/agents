import { describe, expect, test } from "bun:test";
import { OmpRpc, OmpRpcError, type OmpRpcOptions } from "../src/client";
import { encode, makeMemChannel, sleep, type MemChannel } from "./helpers";

const READY = JSON.stringify({
  type: "ready",
  protocolVersion: 1,
  sessionFile: "/tmp/fake-session.jsonl",
});

/** Mem channel + rpc whose handshake completed via a pushed ready frame. */
async function makeStartedRpc(
  opts?: OmpRpcOptions,
  onWrite?: (text: string, mem: MemChannel) => void,
): Promise<{ mem: MemChannel; rpc: OmpRpc }> {
  const mem = makeMemChannel(onWrite ? { onWrite: (text) => onWrite(text, mem) } : {});
  const rpc = new OmpRpc(mem.channel, opts);
  const ready = rpc.start();
  mem.push(encode(`${READY}\n`));
  await ready;
  return { mem, rpc };
}

function respondOk(mem: MemChannel, id: unknown, extra: Record<string, unknown> = {}): void {
  mem.push(encode(`${JSON.stringify({ id, type: "response", success: true, ...extra })}\n`));
}

describe("OmpRpc handshake", () => {
  test("start() resolves with the ready frame contents", async () => {
    const mem = makeMemChannel();
    const rpc = new OmpRpc(mem.channel);
    const pending = rpc.start();
    mem.push(encode(`${READY}\n`));
    const info = await pending;
    expect(info.protocolVersion).toBe(1);
    expect(info.sessionFile).toBe("/tmp/fake-session.jsonl");
    await rpc.close();
  });

  test("start() resolves even when ready arrived before start() was called", async () => {
    const mem = makeMemChannel();
    const rpc = new OmpRpc(mem.channel);
    mem.push(encode(`${READY}\n`));
    await sleep(20); // let the read pump consume the frame first
    const info = await rpc.start();
    expect(info.protocolVersion).toBe(1);
    await rpc.close();
  });

  test("ignores non-ready pre-handshake frames, then completes on ready", async () => {
    const mem = makeMemChannel();
    const rpc = new OmpRpc(mem.channel);
    const pending = rpc.start();
    mem.push(encode('{"type":"available_commands_update","commands":[]}\n'));
    mem.push(encode('{"type":"extension_ui_request","id":"x"}\n'));
    mem.push(encode(`${READY}\n`));
    const info = await pending;
    expect(info.protocolVersion).toBe(1);
    await rpc.close();
  });

  test("times out with handshake_timeout and closes the channel", async () => {
    const mem = makeMemChannel();
    const rpc = new OmpRpc(mem.channel, { handshakeTimeoutMs: 40 });
    await expect(rpc.start()).rejects.toMatchObject({
      name: "OmpRpcError",
      code: "handshake_timeout",
    });
    await mem.channelClosed; // start() must tear the channel down
  });

  test("start() rejects with channel_closed when the worker dies pre-ready", async () => {
    const mem = makeMemChannel();
    const rpc = new OmpRpc(mem.channel, { handshakeTimeoutMs: 2_000 });
    const pending = rpc.start();
    mem.endReadable(); // worker exited without ever emitting ready
    await expect(pending).rejects.toMatchObject({
      name: "OmpRpcError",
      code: "channel_closed",
    });
  });

  test("start() twice throws (programming error)", async () => {
    const mem = makeMemChannel();
    const rpc = new OmpRpc(mem.channel);
    const first = rpc.start();
    mem.push(encode(`${READY}\n`));
    await first;
    expect(() => rpc.start()).toThrow(/exactly once/);
    await rpc.close();
  });
});

describe("OmpRpc requests", () => {
  test("frames carry a sequence id and the raw method fields", async () => {
    const { mem, rpc } = await makeStartedRpc();
    const pending = rpc.prompt("hello world");
    await sleep(5);
    expect(mem.writes()).toEqual(['{"id":1,"type":"prompt","message":"hello world"}\n']);
    respondOk(mem, 1);
    await pending;
    await rpc.close();
  });

  test("correlates two concurrent requests by id", async () => {
    const seenIds: unknown[] = [];
    const { rpc } = await makeStartedRpc(undefined, (text, channel) => {
      const frame = JSON.parse(text) as { id?: unknown; type?: string };
      seenIds.push(frame.id);
      channel.push(
        encode(
          `${JSON.stringify({
            id: frame.id,
            type: "response",
            success: true,
            echo: frame.type,
          })}\n`,
        ),
      );
    });
    const [state, text] = await Promise.all([rpc.getState(), rpc.getLastAssistantText()]);
    expect(state.echo).toBe("get_state");
    expect(text.echo).toBe("get_last_assistant_text");
    expect(new Set(seenIds)).toEqual(new Set([1, 2])); // distinct correlation ids
    await rpc.close();
  });

  test("rejects with request_failed and the server error message", async () => {
    const { rpc } = await makeStartedRpc(undefined, (text, channel) => {
      const frame = JSON.parse(text) as { id?: unknown };
      channel.push(
        encode(
          `${JSON.stringify({
            id: frame.id,
            type: "response",
            success: false,
            error: "no such tool",
          })}\n`,
        ),
      );
    });
    const error = await rpc.abort().then(
      () => undefined,
      (err: unknown) => err,
    );
    expect(error).toBeInstanceOf(OmpRpcError);
    expect((error as OmpRpcError).code).toBe("request_failed");
    expect((error as OmpRpcError).message).toBe("no such tool");
    await rpc.close();
  });

  test("rejects with request_timeout when no response arrives", async () => {
    const { rpc } = await makeStartedRpc();
    await expect(rpc.request({ type: "get_state" }, 30)).rejects.toMatchObject({
      name: "OmpRpcError",
      code: "request_timeout",
    });
    await rpc.close();
  });

  test("timeoutMs <= 0 disables the timer (resolved only by response or close)", async () => {
    const { mem, rpc } = await makeStartedRpc();
    const pending = rpc.request({ type: "get_state" }, 0);
    await sleep(60); // longer than any sensible default timer
    mem.push(encode('{"id":1,"type":"response","success":true,"isStreaming":false}\n'));
    await expect(pending).resolves.toMatchObject({ isStreaming: false });
    await rpc.close();
  });

  test("conveniences send the documented method frames", async () => {
    const { mem, rpc } = await makeStartedRpc();
    const floats: Array<Promise<unknown>> = [
      rpc.prompt("do the thing"),
      rpc.steer("new info"),
      rpc.abort(),
      rpc.setSessionName("my session"),
    ];
    await sleep(5);
    expect(mem.writes()).toEqual([
      '{"id":1,"type":"prompt","message":"do the thing"}\n',
      '{"id":2,"type":"steer","message":"new info"}\n',
      '{"id":3,"type":"abort"}\n',
      '{"id":4,"type":"set_session_name","name":"my session"}\n',
    ]);
    for (const float of floats) {
      float.catch(() => {}); // these are answered by close(), not responses
    }
    await rpc.close();
  });
});

describe("OmpRpc events and closure", () => {
  test("forwards non-response frames verbatim; junk never throws", async () => {
    const { mem, rpc } = await makeStartedRpc();
    const events: Array<Record<string, unknown>> = [];
    rpc.onEvent((event) => events.push(event));
    mem.push(encode('{"type":"agent_start"}\n'));
    mem.push(encode('{"type":"weird_unknown_frame","payload":{"x":1}}\n'));
    mem.push(encode("this is not json at all\n"));
    mem.push(encode('{"type":"response","id":999,"success":true}\n')); // unknown id -> event
    mem.push(encode('{"type":"message_end","message":{"role":"assistant"}}\n'));
    await sleep(20);
    expect(events.map((event) => event.type)).toEqual([
      "agent_start",
      "weird_unknown_frame",
      "response",
      "message_end",
    ]);
    await rpc.close();
  });

  test("unsubscribe stops event delivery", async () => {
    const { mem, rpc } = await makeStartedRpc();
    const events: string[] = [];
    const off = rpc.onEvent((event) => events.push(event.type));
    mem.push(encode('{"type":"first"}\n'));
    await sleep(5); // dispatch happens on the async read pump
    expect(events).toEqual(["first"]);
    off();
    mem.push(encode('{"type":"second"}\n'));
    await sleep(5);
    expect(events).toEqual(["first"]);
    await rpc.close();
  });

  test("channel EOF resolves channelClosed, fires onClose, rejects pending requests", async () => {
    const { mem, rpc } = await makeStartedRpc();
    const closed: string[] = [];
    rpc.onClose(() => closed.push("closed"));
    const pending = rpc.request({ type: "get_state" }, 0); // no timer — only closure settles it
    mem.endReadable();
    await expect(pending).rejects.toMatchObject({
      name: "OmpRpcError",
      code: "channel_closed",
    });
    await rpc.channelClosed;
    expect(closed).toEqual(["closed"]);
    await rpc.close(); // idempotent
    expect(closed).toEqual(["closed"]);
  });

  test("onClose unsubscribe works", async () => {
    const mem = makeMemChannel();
    const rpc = new OmpRpc(mem.channel);
    const closed: string[] = [];
    const off = rpc.onClose(() => closed.push("closed"));
    off();
    const pendingStart = rpc.start();
    mem.endReadable();
    await expect(pendingStart).rejects.toMatchObject({ code: "channel_closed" });
    await rpc.channelClosed;
    expect(closed).toEqual([]);
  });

  test("requests after close() reject immediately with channel_closed", async () => {
    const { rpc } = await makeStartedRpc();
    await rpc.close();
    await expect(rpc.getState()).rejects.toMatchObject({
      name: "OmpRpcError",
      code: "channel_closed",
    });
  });

  test("close() rejects in-flight requests with channel_closed", async () => {
    const { rpc } = await makeStartedRpc();
    const pending = rpc.request({ type: "get_state" }, 0);
    await rpc.close();
    await expect(pending).rejects.toMatchObject({ code: "channel_closed" });
    await rpc.channelClosed;
  });
});
