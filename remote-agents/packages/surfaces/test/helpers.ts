import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Channel } from "@remote-agents/protocol";
import { makeSocketReadable } from "../src/herdr";

/** The shared fake worker of this repo, referenced strictly by path. */
export const FAKE_OMP_PATH = join(import.meta.dir, "..", "..", "rpc-driver", "test", "fake-omp.ts");

/** TCP Channel over Bun.connect (socket callbacks → web streams). */
export function connectChannel(port: number): Promise<Channel> {
  const stream = makeSocketReadable();
  return Bun.connect({
    hostname: "127.0.0.1",
    port,
    socket: {
      data(_sock, chunk) {
        stream.push(chunk);
      },
      close() {
        stream.end();
      },
      error(_sock, cause) {
        stream.fail(cause);
      },
    },
  }).then(
    (socket): Channel => ({
      readable: stream.readable,
      writable: new WritableStream<Uint8Array>({
        write(chunk) {
          socket.write(chunk);
        },
        close() {
          try {
            socket.end();
          } catch {
            // Already closed.
          }
        },
        abort() {
          try {
            socket.end();
          } catch {
            // Already closed.
          }
        },
      }),
      close: async () => {
        try {
          socket.end();
        } catch {
          // Already closed.
        }
      },
    }),
  );
}

/** Poll the port file the bridge writes atomically. */
export async function waitForPort(portFile: string, budgetMs: number): Promise<number> {
  const deadline = Date.now() + budgetMs;
  for (;;) {
    try {
      const port = Number.parseInt(readFileSync(portFile, "utf8").trim(), 10);
      if (Number.isInteger(port) && port > 0) return port;
    } catch {
      // Not written yet.
    }
    if (Date.now() > deadline) throw new Error(`port file never appeared: ${portFile}`);
    await sleep(50);
  }
}

export function sleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

export interface RpcEventLike {
  type: string;
}

/** Poll until an event of `type` was observed (workers settle async). */
export async function waitForEvent<T extends RpcEventLike>(
  events: T[],
  type: string,
  timeoutMs = 5_000,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const found = events.find((event) => event.type === type);
    if (found) return found;
    if (Date.now() > deadline) {
      throw new Error(
        `event "${type}" never arrived within ${timeoutMs}ms; saw: [${events
          .map((event) => event.type)
          .join(", ")}]`,
      );
    }
    await sleep(10);
  }
}

/** Await a subprocess exit with a budget (guards against hangs in CI). */
export async function exitedWithin(exited: Promise<number>, budgetMs: number): Promise<number> {
  return Promise.race([
    exited,
    sleep(budgetMs).then(() => {
      throw new Error(`process did not exit within ${budgetMs}ms`);
    }),
  ]);
}
