import type { Channel } from "@remote-agents/protocol";

/** Handle handed to the line callback for closing the underlying channel. */
export interface RawLineHandle {
  close(): void;
}

export type LineCallback = (line: string, raw: RawLineHandle) => void;

/**
 * Consume `channel.readable` and invoke `onLine` once per line.
 *
 * Lines are split on LF ("\n"); a single trailing "\r" is stripped so CRLF
 * sources degrade gracefully. Tolerant of chunks that split a line — or a
 * multi-byte UTF-8 sequence — at an arbitrary boundary; a final line without
 * a trailing newline is still delivered when the stream ends.
 *
 * Returns nothing. The caller owns the write side via
 * `channel.writable.getWriter()`.
 */
export function createLineChannel(channel: Channel, onLine: LineCallback): void {
  const decoder = new TextDecoder();
  let buffer = "";
  let closed = false;

  const reader = channel.readable.getReader();

  const raw: RawLineHandle = {
    close(): void {
      if (closed) return;
      closed = true;
      void reader.cancel().catch(() => {});
      void channel.close().catch(() => {});
    },
  };

  function emitLine(line: string): void {
    onLine(line.endsWith("\r") ? line.slice(0, -1) : line, raw);
  }

  function emitCompleteLines(): void {
    for (;;) {
      const idx = buffer.indexOf("\n");
      if (idx === -1) return;
      const line = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 1);
      emitLine(line);
      if (closed) return;
    }
  }

  void (async () => {
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        emitCompleteLines();
      }
      // Flush any pending multi-byte sequence, then deliver a final
      // unterminated line if the producer omitted the trailing newline.
      buffer += decoder.decode();
      if (buffer.length > 0 && !closed) {
        emitLine(buffer);
        buffer = "";
      }
    } catch {
      // Read side errored or was cancelled. Closure is detected by whoever
      // watches the channel (see client.ts); nothing to do here.
    }
  })();
}
