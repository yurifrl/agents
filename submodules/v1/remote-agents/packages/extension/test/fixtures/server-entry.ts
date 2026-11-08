#!/usr/bin/env bun
// Test-only server binary stand-in for ensureServer bootstrap: binds the port
// the extension client expects, serves /health, and writes its pid so the test
// can clean up the detached process. Spawned as `<this-file> server`.

import { ENV } from "@remote-agents/protocol";
import { writeFileSync } from "node:fs";

const pidFile = process.env.REMOTE_AGENTS_TEST_PID_FILE ?? "";
if (pidFile !== "") writeFileSync(pidFile, String(process.pid));

const url = process.env[ENV.url] ?? "";
const portFromUrl = url !== "" ? new URL(url).port : undefined;
const port = Number(process.env[ENV.port] ?? portFromUrl ?? "8679");

Bun.serve({
  port,
  fetch(req: Request): Response {
    if (new URL(req.url).pathname === "/health") {
      return new Response(
        JSON.stringify({
          ok: true,
          version: "entry-fixture",
          surface: "process",
          uptimeMs: 1,
          tasks: { active: 0, total: 0 },
        }),
        { headers: { "content-type": "application/json" } },
      );
    }
    return new Response(JSON.stringify({ error: "not found" }), {
      status: 404,
      headers: { "content-type": "application/json" },
    });
  },
});

console.log(`fixture server binary listening on ${port}`);
