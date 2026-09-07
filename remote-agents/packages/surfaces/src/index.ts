import type { Surface, SurfaceKind } from "@remote-agents/protocol";
import { HerdrSurface } from "./herdr";
import { ProcessSurface } from "./process";

export interface SurfaceOpts {
  ompBin: string;
  /** Where the herdr bridge script is written (e.g. <stateDir>/bridge.mjs). */
  bridgePath: string;
  stateDir: string;
  /** herdr surface: workspace name (default "remote-agents"). */
  workspace?: string;
  /** herdr surface: herdr CLI path (default "herdr"). */
  herdrBin?: string;
}

/**
 * Build a worker surface by kind. Auto-selection between kinds lives in the
 * server config (CONTRACT.md §4), not here.
 */
export function createSurface(kind: SurfaceKind, opts: SurfaceOpts): Surface {
  switch (kind) {
    case "process":
      return new ProcessSurface();
    case "herdr":
      return new HerdrSurface({
        herdrBin: opts.herdrBin,
        workspace: opts.workspace,
        bridgePath: opts.bridgePath,
      });
    default: {
      const exhaustive: never = kind;
      throw new Error(`unknown surface kind: ${String(exhaustive)}`);
    }
  }
}

export { HerdrSurface, DEFAULT_HERDR_BIN, DEFAULT_HERDR_WORKSPACE } from "./herdr";
export type { HerdrSurfaceOptions } from "./herdr";
export { ProcessSurface, processChannel, stopProcess, workerArgv } from "./process";
export { renderBridgeScript } from "./bridge";
export { SurfaceError } from "./error";
export { TASK_ID_ENV_VAR, WORKER_ENV_VAR } from "./process";
