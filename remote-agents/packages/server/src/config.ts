import { DEFAULT_PORT, ENV, type SurfaceKind } from "@remote-agents/protocol";
import { homedir } from "node:os";
import path from "node:path";

export const SERVER_VERSION = "0.1.0";

/** `REMOTE_AGENTS_SURFACE` value: a concrete kind or `auto` (detect herdr on PATH). */
export type SurfaceSetting = "auto" | SurfaceKind;

export interface Config {
  port: number;
  stateDir: string;
  surface: SurfaceSetting;
  ompBin: string;
  model?: string;
  maxTimeS: number;
  url: string;
}

function parsePort(raw: string | undefined): number {
  if (raw === undefined || raw === "") return DEFAULT_PORT;
  const n = Number(raw);
  if (!Number.isInteger(n) || n < 0 || n > 65535) {
    throw new Error(`invalid ${ENV.port}: ${raw}`);
  }
  return n;
}

function parseMaxTimeS(raw: string | undefined): number {
  if (raw === undefined || raw === "") return 1800;
  const n = Number(raw);
  if (!Number.isInteger(n) || n <= 0) {
    throw new Error(`invalid ${ENV.maxTimeS}: ${raw}`);
  }
  return n;
}

function expandHome(p: string): string {
  if (p === "~") return homedir();
  if (p.startsWith("~/")) return path.join(homedir(), p.slice(2));
  return p;
}

export function loadConfig(env: Record<string, string | undefined> = process.env): Config {
  const port = parsePort(env[ENV.port]);
  const surfaceRaw = env[ENV.surface] ?? "auto";
  if (surfaceRaw !== "auto" && surfaceRaw !== "process" && surfaceRaw !== "herdr") {
    throw new Error(`invalid ${ENV.surface}: ${surfaceRaw} (expected auto|process|herdr)`);
  }
  return {
    port,
    stateDir: expandHome(env[ENV.stateDir] ?? path.join(homedir(), ".remote-agents")),
    surface: surfaceRaw,
    ompBin: env[ENV.ompBin]?.trim() || "omp",
    model: env[ENV.model]?.trim() || undefined,
    maxTimeS: parseMaxTimeS(env[ENV.maxTimeS]),
    url: env[ENV.url]?.trim() || `http://127.0.0.1:${port}`,
  };
}

/** Resolve `auto` to a concrete surface kind via a `which`-style PATH check. */
export function resolveSurfaceKind(
  config: Config,
  which: (bin: string) => string | null = (bin) => Bun.which(bin),
): SurfaceKind {
  if (config.surface !== "auto") return config.surface;
  return which("herdr") ? "herdr" : "process";
}

/** Options forwarded to `createSurface(kind, opts)` from @remote-agents/surfaces. */
export interface SurfaceFactoryOptions {
  ompBin: string;
  bridgePath: string;
  stateDir: string;
  herdrBin: string;
}

export function surfaceOptions(config: Config): SurfaceFactoryOptions {
  return {
    ompBin: config.ompBin,
    bridgePath: path.join(config.stateDir, "bridge.mjs"),
    stateDir: config.stateDir,
    herdrBin: "herdr",
  };
}
