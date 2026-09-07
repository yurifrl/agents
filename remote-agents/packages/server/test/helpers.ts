import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import type { SseFrame, SurfaceKind } from "@remote-agents/protocol";
import { createApp, loadConfig, type App } from "../src/index";
import { FakeSurface } from "./fake-surface";

export const sleep = (ms: number): Promise<void> => {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
};

export async function waitUntil<T>(
  fn: () => T | undefined | null | false,
  timeoutMs = 10_000,
  stepMs = 25,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const value = fn();
    if (value) return value;
    if (Date.now() > deadline) throw new Error(`waitUntil: timed out after ${timeoutMs}ms`);
    await sleep(stepMs);
  }
}

export function makeStateDir(): string {
  return mkdtempSync(path.join(tmpdir(), "ra-server-"));
}

export function cleanupDir(dir: string): void {
  rmSync(dir, { recursive: true, force: true });
}

/**
 * Contract §6: prefer the shared fake worker at packages/rpc-driver/test/fake-omp.ts
 * (spawned by path). Until that lands, fall back to the local throwaway fake
 * speaking the same protocol; integration unifies on the shared one.
 */
export async function stdioFakeOmpPath(): Promise<string> {
  const shared = new URL("../../rpc-driver/test/fake-omp.ts", import.meta.url).pathname;
  if (await Bun.file(shared).exists()) return shared;
  return new URL("./local-fake-omp.ts", import.meta.url).pathname;
}

/** The shared fake-omp is stdio-only; TCP adoption tests use the local fake's --tcp mode. */
export function tcpFakeOmpPath(): string {
  return new URL("./local-fake-omp.ts", import.meta.url).pathname;
}

export interface TestServer {
  app: App;
  baseUrl: string;
  surface: FakeSurface;
  cleanup(): Promise<void>;
}

export async function startTestServer(opts: {
  stateDir: string;
  fakePath: string;
  extraArgs?: string[];
  kind?: SurfaceKind;
}): Promise<TestServer> {
  const config = loadConfig({
    REMOTE_AGENTS_PORT: "0",
    REMOTE_AGENTS_STATE_DIR: opts.stateDir,
    REMOTE_AGENTS_MAX_TIME_S: "120",
  });
  const surface = new FakeSurface({
    fakePath: opts.fakePath,
    extraArgs: opts.extraArgs,
    kind: opts.kind,
  });
  const app = await createApp(config, {
    surface,
    heartbeatMs: 30,
    supervisorOpts: { maxTimeMs: 60_000 },
  });
  return {
    app,
    surface,
    baseUrl: `http://127.0.0.1:${app.server.port}`,
    cleanup: async () => {
      await surface.killAll();
      app.server.stop(true);
    },
  };
}

export interface SseCapture {
  frames: SseFrame[];
  closed: boolean;
}

/** Read an SSE endpoint to natural close, parsing `data:` frames; comments ignored. */
export async function captureSse(url: string, timeoutMs = 15_000): Promise<SseCapture> {
  const { promise, resolve, reject } = Promise.withResolvers<SseCapture>();
  const timer = setTimeout(() => reject(new Error("captureSse: timed out")), timeoutMs);
  timer.unref?.();
  void (async () => {
    try {
      const res = await fetch(url, { headers: { accept: "text/event-stream" } });
      if (!res.ok || !res.body) throw new Error(`captureSse: HTTP ${res.status}`);
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      const frames: SseFrame[] = [];
      let closed = false;
      for (;;) {
        const { done, value } = await reader.read();
        if (done) {
          closed = true;
          break;
        }
        buffer += decoder.decode(value, { stream: true });
        for (;;) {
          const idx = buffer.indexOf("\n\n");
          if (idx < 0) break;
          const block = buffer.slice(0, idx);
          buffer = buffer.slice(idx + 2);
          for (const line of block.split("\n")) {
            if (line.startsWith("data: ")) {
              frames.push(JSON.parse(line.slice("data: ".length)) as SseFrame);
            }
          }
        }
      }
      resolve({ frames, closed });
    } catch (err) {
      reject(err instanceof Error ? err : new Error(String(err)));
    } finally {
      clearTimeout(timer);
    }
  })();
  return promise;
}
