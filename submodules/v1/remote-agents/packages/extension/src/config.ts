// remote-agents.json — extension config, read at session_start.
//
// Location: `<agent-dir>/extensions/remote-agents.json`
// (`~/.omp/agent/extensions/remote-agents.json` by default; honored per
// profile because the path is derived from the session's cwd-adjacent agent
// dir via HOME — the same dir omp auto-discovers this extension from).
//
// Shape:
//   { "enabled": false }                       — factory becomes a no-op
//   { "enabled": true, "intervalMs": 2000 }    — widget poll interval override
//
// Missing file / parse failure → defaults (enabled, 2s widget).

import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

export interface RemoteAgentsConfig {
  /** Master switch; false makes the factory register nothing. Default true. */
  enabled: boolean;
  /** Widget poll interval in ms. Default 2000. */
  intervalMs: number;
}

export const DEFAULT_CONFIG: RemoteAgentsConfig = { enabled: true, intervalMs: 2_000 };

/** `<home>/.omp/agent/extensions/remote-agents.json` (no node:os per contract §5). */
export function configPath(): string {
  const home = process.env.HOME ?? process.env.USERPROFILE ?? ".";
  return join(home, ".omp", "agent", "extensions", "remote-agents.json");
}

export function readConfig(path: string = configPath()): RemoteAgentsConfig {
  if (!existsSync(path)) return DEFAULT_CONFIG;
  try {
    const raw: unknown = JSON.parse(readFileSync(path, "utf8"));
    if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return DEFAULT_CONFIG;
    const obj = raw as Record<string, unknown>;
    return {
      enabled: typeof obj.enabled === "boolean" ? obj.enabled : DEFAULT_CONFIG.enabled,
      intervalMs:
        typeof obj.intervalMs === "number" && Number.isFinite(obj.intervalMs) && obj.intervalMs > 0
          ? obj.intervalMs
          : DEFAULT_CONFIG.intervalMs,
    };
  } catch {
    // Malformed config must not brick the extension; fall back to defaults.
    return DEFAULT_CONFIG;
  }
}

/** Persist the full config (preserves intervalMs); creates parent dirs. */
export function writeConfig(config: RemoteAgentsConfig, path: string = configPath()): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, `${JSON.stringify(config, null, 2)}\n`);
}
