import type { Channel } from "@remote-agents/protocol";

/** Absolute path of the shared fake worker fixture (spawn by path only). */
export const FAKE_OMP_PATH = `${import.meta.dir}/fake-omp.ts`;

export interface MemChannel {
  channel: Channel;
  /** Bytes a "worker" writes into the channel's readable side. */
  push(chunk: Uint8Array): void;
  endReadable(): void;
  failReadable(cause: unknown): void;
  /** Text of every chunk the OmpRpc client wrote, one entry per write. */
  writes(): string[];
  /** Resolves when channel.close() is called (any direction). */
  channelClosed: Promise<void>;
}

export interface MemChannelOptions {
  /** Called with each decoded chunk the client writes (echo-server hook). */
  onWrite?: (text: string) => void;
}

/** In-memory Channel with manual readable control and recorded writes. */
export function makeMemChannel(opts: MemChannelOptions = {}): MemChannel {
  const decoder = new TextDecoder();
  const written: string[] = [];
  let pushChunk: (chunk: Uint8Array) => void = () => {};
  let endReadableFn: () => void = () => {};
  let failReadableFn: (cause: unknown) => void = () => {};
  const closed = Promise.withResolvers<void>();

  const channel: Channel = {
    readable: new ReadableStream<Uint8Array>({
      start(controller) {
        pushChunk = (chunk) => {
          try {
            controller.enqueue(chunk);
          } catch {
            // Stream already closed.
          }
        };
        endReadableFn = () => {
          try {
            controller.close();
          } catch {
            // Stream already closed.
          }
        };
        failReadableFn = (cause) => {
          try {
            controller.error(cause);
          } catch {
            // Stream already closed.
          }
        };
      },
    }),
    writable: new WritableStream<Uint8Array>({
      write(chunk) {
        const text = decoder.decode(chunk);
        written.push(text);
        opts.onWrite?.(text);
      },
    }),
    close: () => {
      closed.resolve();
      return Promise.resolve();
    },
  };

  return {
    channel,
    push: (chunk) => pushChunk(chunk),
    endReadable: () => endReadableFn(),
    failReadable: (cause) => failReadableFn(cause),
    writes: () => written,
    channelClosed: closed.promise,
  };
}

interface ProcessLikeStdin {
  write(data: Uint8Array): unknown;
  flush(): unknown;
  end(): unknown;
}

/** WritableStream half of a Channel over a Bun subprocess FileSink stdin. */
export function sinkWritable(stdin: ProcessLikeStdin): WritableStream<Uint8Array> {
  return new WritableStream<Uint8Array>({
    write(chunk) {
      stdin.write(chunk);
      stdin.flush();
    },
    close() {
      stdin.end();
    },
    abort() {
      stdin.end();
    },
  });
}

export interface FakeOmpProcess {
  pid: number;
  channel: Channel;
  exited: Promise<number>;
  stderr(): Promise<string>;
  /** SIGKILL the process (tests that need a hard stop). */
  kill(): void;
}

/** Spawn the fake omp worker with piped stdio and expose it as a Channel. */
export function spawnFakeOmp(args: string[] = [], env?: Record<string, string>): FakeOmpProcess {
  const proc = Bun.spawn([process.execPath, FAKE_OMP_PATH, ...args], {
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
    ...(env !== undefined ? { env: { ...process.env, ...env } } : {}),
  });
  return {
    pid: proc.pid,
    channel: {
      readable: proc.stdout as ReadableStream<Uint8Array>,
      writable: sinkWritable(proc.stdin),
      close: async () => {
        try {
          proc.stdin.end();
        } catch {
          // Already gone.
        }
      },
    },
    exited: proc.exited,
    stderr: () => new Response(proc.stderr).text(),
    kill: () => proc.kill("SIGKILL"),
  };
}

export function encode(text: string): Uint8Array {
  return new TextEncoder().encode(text);
}

export function sleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

export interface RpcEventLike {
  type: string;
}

/** Poll until an event of `type` was observed (fake workers settle async). */
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
