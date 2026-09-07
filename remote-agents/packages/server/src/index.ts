import path from "node:path";
import { type Surface } from "@remote-agents/protocol";
import { createSurface } from "@remote-agents/surfaces";
import {
  resolveSurfaceKind,
  SERVER_VERSION,
  surfaceOptions,
  type Config,
  type SurfaceFactoryOptions,
} from "./config";
import { createHttpServer } from "./http";
import { Supervisor, type SupervisorOpts } from "./supervisor";
import { TaskStore } from "./store";
import { badRequest, conflict, HttpError, notFound } from "./errors";

export {
  loadConfig,
  resolveSurfaceKind,
  surfaceOptions,
  SERVER_VERSION,
  type Config,
  type SurfaceFactoryOptions,
} from "./config";
export { TaskStore, type TaskFilter } from "./store";
export { Supervisor, type SupervisorOpts } from "./supervisor";
export { createHttpServer, type HttpOptions } from "./http";
export { HttpError, badRequest, conflict, notFound } from "./errors";

export interface App {
  config: Config;
  store: TaskStore;
  supervisor: Supervisor;
  server: Bun.Server<undefined>;
}

export interface CreateAppOptions {
  /** Injected surface (tests); when omitted, built from config via @remote-agents/surfaces. */
  surface?: Surface;
  store?: TaskStore;
  supervisor?: Supervisor;
  supervisorOpts?: SupervisorOpts;
  heartbeatMs?: number;
}

/**
 * Compose store + supervisor + HTTP server. Tests inject a `surface` (and may
 * inject store/supervisor); production resolves the configured surface kind
 * via @remote-agents/surfaces.
 */
export async function createApp(config: Config, options: CreateAppOptions = {}): Promise<App> {
  const store = options.store ?? new TaskStore(config.stateDir);
  const surface =
    options.surface ?? createSurface(resolveSurfaceKind(config), surfaceOptions(config));
  const supervisorOpts: SupervisorOpts = {
    maxTimeMs: config.maxTimeS * 1000,
    ompBin: config.ompBin,
    model: config.model,
    ...options.supervisorOpts,
  };
  const supervisor = options.supervisor ?? new Supervisor(store, surface, supervisorOpts);
  const server = createHttpServer({
    config,
    store,
    supervisor,
    heartbeatMs: options.heartbeatMs,
  });
  return { config, store, supervisor, server };
}
