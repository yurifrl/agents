import { describe, expect, test } from "bun:test";
import { createLineChannel } from "../src/framing";
import { encode, makeMemChannel, sleep } from "./helpers";

/** Mem channel + line collector; pump dispatches async, so await settle(). */
function collectLines() {
  const mem = makeMemChannel();
  const lines: string[] = [];
  let raw: { close(): void } | undefined;
  createLineChannel(mem.channel, (line, handle) => {
    lines.push(line);
    raw ??= handle;
  });
  const settle = () => sleep(2);
  return { mem, lines, rawHandle: () => raw, settle };
}

describe("createLineChannel", () => {
  test("splits a line written across two chunks", async () => {
    const { mem, lines, settle } = collectLines();
    mem.push(encode('{"type":"ready","protocol'));
    mem.push(encode('Version":1}\n'));
    await settle();
    expect(lines).toEqual(['{"type":"ready","protocolVersion":1}']);
  });

  test("delivers every complete line across many chunk boundaries", async () => {
    const { mem, lines, settle } = collectLines();
    const payload = "first\nsecond\nthird\n";
    // Every possible 2-chunk split of the payload must yield the same lines.
    for (let split = 0; split <= payload.length; split++) {
      lines.length = 0;
      mem.push(encode(payload.slice(0, split)));
      mem.push(encode(payload.slice(split)));
      await settle();
      expect(lines).toEqual(["first", "second", "third"]);
    }
  });

  test("emits several lines from a single chunk", async () => {
    const { mem, lines, settle } = collectLines();
    mem.push(encode("a\nbb\nccc\n"));
    await settle();
    expect(lines).toEqual(["a", "bb", "ccc"]);
  });

  test("strips one trailing \\r (CRLF sources)", async () => {
    const { mem, lines, settle } = collectLines();
    mem.push(encode("line1\r\nline2\r\n"));
    await settle();
    expect(lines).toEqual(["line1", "line2"]);
  });

  test("keeps empty lines (dumb splitter; consumers filter)", async () => {
    const { mem, lines, settle } = collectLines();
    mem.push(encode("a\n\nb\n"));
    await settle();
    expect(lines).toEqual(["a", "", "b"]);
  });

  test("delivers a final unterminated line when the stream ends", async () => {
    const { mem, lines } = collectLines();
    mem.push(encode('{"type":"agent_settled"}'));
    mem.endReadable();
    await sleep(5);
    expect(lines).toEqual(['{"type":"agent_settled"}']);
  });

  test("re-assembles multi-byte UTF-8 split across chunks", async () => {
    const { mem, lines, settle } = collectLines();
    const line = '{"text":"héllo 🤖 wörld"}\n';
    const bytes = encode(line);
    // Split at every byte position, including inside "é" and the 4-byte emoji.
    for (let split = 1; split < bytes.length; split++) {
      lines.length = 0;
      mem.push(bytes.slice(0, split));
      mem.push(bytes.slice(split));
      await settle();
      expect(lines).toEqual([line.replace(/\n$/, "")]);
    }
  });

  test("raw.close() cancels the pump and closes the channel", async () => {
    const { mem, lines, rawHandle, settle } = collectLines();
    mem.push(encode("trigger\n"));
    await settle(); // let onLine fire so the raw handle is captured
    expect(rawHandle()).toBeDefined();
    rawHandle()?.close();
    await mem.channelClosed;
    lines.length = 0;
    mem.push(encode("after-close\n"));
    await settle();
    expect(lines).toEqual([]);
  });
});
