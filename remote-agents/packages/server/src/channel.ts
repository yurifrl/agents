import type { Channel } from "@remote-agents/protocol";

/**
 * Adapt a Bun TCP socket (callback API — Bun 1.3 TCPSocket exposes no web-stream
 * `readable`/`writable`) to the protocol `Channel` seam.
 */
export function tcpChannel(hostname: string, port: number): Promise<Channel> {
  const { promise, resolve, reject } = Promise.withResolvers<Channel>();
  let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
  const readable = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
  });
  Bun.connect({
    hostname,
    port,
    socket: {
      data(_socket, chunk) {
        try {
          controller?.enqueue(chunk);
        } catch {
          // stream already closed; drop the chunk
        }
      },
      close() {
        try {
          controller?.close();
        } catch {
          // already closed
        }
      },
      error() {
        try {
          controller?.close();
        } catch {
          // already closed
        }
      },
    },
  }).then(
    (socket) => {
      const writable = new WritableStream<Uint8Array>({
        write(chunk) {
          socket.write(chunk);
        },
        close() {
          socket.end();
        },
        abort() {
          socket.end();
        },
      });
      resolve({
        readable,
        writable,
        close: async () => {
          socket.end();
        },
      });
    },
    (err) => reject(err instanceof Error ? err : new Error(String(err))),
  );
  return promise;
}
