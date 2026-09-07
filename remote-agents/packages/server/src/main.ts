import { createSurface } from "@remote-agents/surfaces";
import { loadConfig, resolveSurfaceKind, surfaceOptions } from "./config";
import { createApp } from "./index";

const config = loadConfig();
const kind = resolveSurfaceKind(config);
const surface = createSurface(kind, surfaceOptions(config));
const app = await createApp(config, { surface });
const adopted = await app.supervisor.adoptExisting();

console.log(
  `remote-agents server listening on ${app.server.port} (surface=${kind}, adopted=${adopted})`,
);

let shuttingDown = false;
const shutdown = (signal: string): void => {
  if (shuttingDown) return;
  shuttingDown = true;
  void (async () => {
    await app.supervisor.shutdown();
    app.store.saveNow();
    app.server.stop(true);
    console.log(`remote-agents: ${signal} received — shut down`);
    process.exit(0);
  })();
};

process.on("SIGINT", () => shutdown("SIGINT"));
process.on("SIGTERM", () => shutdown("SIGTERM"));
