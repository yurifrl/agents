import { expect, test } from "bun:test";
import type { SseFrame } from "@remote-agents/protocol";
import { createSseParser } from "../src/api.ts";

function frame(seq: number): SseFrame {
  return { taskId: "t1", seq, type: "log", data: { line: `e${seq}` }, ts: seq };
}

function wire(f: SseFrame): string {
  return `data: ${JSON.stringify(f)}\n\n`;
}

function collect(): { frames: SseFrame[]; push(chunk: string): void; end(): void } {
  const frames: SseFrame[] = [];
  const parser = createSseParser((f) => frames.push(f));
  return { frames, push: (chunk) => parser.push(chunk), end: () => parser.end() };
}

test("parses a complete frame", () => {
  const { frames, push } = collect();
  push(wire(frame(1)));
  expect(frames).toEqual([frame(1)]);
});

test("tolerates a chunk split mid-frame (mid-JSON)", () => {
  const { frames, push } = collect();
  const payload = wire(frame(7));
  push(payload.slice(0, 12));
  expect(frames).toEqual([]);
  push(payload.slice(12));
  expect(frames).toEqual([frame(7)]);
});

test("tolerates a split inside the CRLF pair", () => {
  const { frames, push } = collect();
  const payload = `data: ${JSON.stringify(frame(3))}\r\n\r\n`;
  push(payload.slice(0, payload.length - 2));
  expect(frames).toEqual([]);
  push(payload.slice(payload.length - 2));
  expect(frames).toEqual([frame(3)]);
});

test("parses multiple frames arriving in one chunk", () => {
  const { frames, push } = collect();
  push(`${wire(frame(1))}${wire(frame(2))}${wire(frame(3))}`);
  expect(frames.map((f) => f.seq)).toEqual([1, 2, 3]);
});

test("ignores comments and heartbeats", () => {
  const { frames, push } = collect();
  push(`: heartbeat\n\n${wire(frame(1))}: ping\n\n`);
  expect(frames).toEqual([frame(1)]);
});

test("flushes a trailing frame without its final blank line on end()", () => {
  const { frames, push, end } = collect();
  push(`data: ${JSON.stringify(frame(9))}\n`);
  expect(frames).toEqual([]);
  end();
  expect(frames).toEqual([frame(9)]);
});

test("ignores non-JSON data payloads", () => {
  const { frames, push } = collect();
  push(`data: not-json\n\n${wire(frame(2))}\n\n`);
  expect(frames).toEqual([frame(2)]);
});

test("handles CRLF-terminated frames end to end", () => {
  const { frames, push } = collect();
  push(`data: ${JSON.stringify(frame(1))}\r\n\r\ndata: ${JSON.stringify(frame(2))}\r\n\r\n`);
  expect(frames.map((f) => f.seq)).toEqual([1, 2]);
});
