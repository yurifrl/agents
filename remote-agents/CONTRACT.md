# CONTRACT — remote-agents monorepo (frozen during parallel build)

One server manages omp worker processes; an omp extension gives parents a native
subagent UI (widget, tools, commands) with resume-after-restart. Workers are omp
RPC processes; workers may run **directly** (child of server) or **inside a
herdr pane** bridged over TCP (survives server restart).

## Layout & ownership

```
remote-agents/
  package.json            # root workspaces — ONLY integration owner edits
  CONTRACT.md             # this file — frozen; deviations must be reported, not improvised
  packages/
    protocol/             # types + constants ONLY (no runtime logic, no deps)
    rpc-driver/           # framing + OmpRpc client over any Channel          [builder A]
    surfaces/             # process + herdr worker surfaces                  [builder A]
    server/               # HTTP API, store, supervisor, re-adoption         [builder B]
    extension/            # omp extension + api client (widget/tools/cmds)   [builder C]
  scripts/install-extension.sh
```

Runtime: **Bun everywhere** (`bun test`, `Bun.serve`, `Bun.spawn`; `node:*`
builtins allowed). **Zero external dependencies** except root-level
`typebox` (extension tool schemas; omp maps it to a virtual module at runtime).
No build step. TS strict, small modules, functions over classes, no `any`.

Cross-package imports: `@remote-agents/protocol` only (plus `typebox` in the
extension). Tests may spawn fixtures from other packages **by path only**
(see §6). Nobody edits files outside their packages; nobody edits root.

## 1. protocol (exists already — do not duplicate)

`packages/protocol/src/index.ts` exports: `TaskStatus`, `Task`, `TaskEvent`,
`TaskEventData`, `CreateTaskInput`, `AttachResponse`, `HealthResponse`,
`Channel`, `WorkerSpec`, `RunningWorker`, `Surface`, env-key constants
(`REMOTE_AGENTS_*`), `DEFAULT_PORT`, `PROTOCOL_VERSION`, `API` path builders.

## 2. rpc-driver [A]

`packages/rpc-driver/src/`:

- `framing.ts` — `createLineChannel(channel: Channel, onLine: (line: string, raw: { close(): void }) => void): void`. Splits `channel.readable` on `\n` (strips one trailing `\r`); tolerant of multi-byte chunk boundaries. Returns nothing; caller wires writers via `channel.writable.getWriter()`.
- `client.ts` — `class OmpRpc`:
  - `constructor(channel: Channel, opts?: { handshakeTimeoutMs?: number })`
  - `start(): Promise<ReadyInfo>` — first line must parse as JSON with `type === "ready"`; capture `protocolVersion`, `sessionFile?`. Timeout (default 10s) → throws `OmpRpcError("handshake_timeout")`. All non-response lines after ready are dispatched as events.
  - `onEvent(cb: (event: RpcEvent) => void): () => void` — every JSON line that is not a response to a pending id.
  - `request(method: object & { type: string }, timeoutMs?): Promise<any>` — writes `{ id: nextSeq, ...method }\n`, resolves with the response frame having the same `id`; rejects on `success: false` (message from `error`) or timeout (default 30s).
  - Conveniences: `prompt(message)`, `steer(message)`, `abort()`, `getState()`, `getLastAssistantText()`, `setSessionName(name)`.
  - `close(): Promise<void>` — closes the channel. `channelClosed` promise + `onClose(cb)` for exit detection.
  - Event/type names come from omp: `agent_start`, `turn_start|turn_end`, `message_start|message_update|message_end`, `tool_execution_start|update|end`, `agent_end`, `agent_settled`, `bash_execution_update`, plus unknowns (forwarded verbatim; never throw).
  - Observed reality (installed omp, pi core 0.84.4): `agent_settled` documented but **never emitted**; `agent_end` is the last lifecycle event of every turn. Consumers must not rely on `agent_settled` alone (see §4 settle).
  - Errors: `class OmpRpcError extends Error { code: string }` with codes `handshake_timeout | request_timeout | request_failed | channel_closed`.
- `test/fake-omp.ts` — deterministic fake worker: `bun fake-omp.ts [--settled-delay MS] [--tool NAME] [--exit-after SETTLED N] [--resume-crash]`. Emits: `ready` frame; on `prompt` request → response + `agent_start`, one `tool_execution_start/end` (if `--tool`), `message_end` with assistant text `"FAKE_RESULT=<first 32 chars of prompt>"`, `agent_end`, `agent_settled`; answers `get_state` (isStreaming false), `get_last_assistant_text`, `steer`, `abort` (emits `agent_settled` immediately). Used by A, B, and integration tests **by path** `packages/rpc-driver/test/fake-omp.ts`.
  - The Go server's fixtures (`internal/server/fixture_test.go`, `internal/rpc/rpc_test.go`) cover the same shapes: settled/quiet/crash/steer/hang/busy modes.
- Tests (bun test): framing chunk-boundary split, handshake timeout, request correlation, steer/abort, fake-omp end-to-end prompt→result.

## 3. surfaces [A]

**Moved to Go (2026-09-17 unification):** `internal/surface/` — `process.go`,
`herdr.go`, `k8s.go`, and `bridge/` (the ported `renderBridgeScript()`, byte
identical). The TS `packages/surfaces` was deleted. Semantics unchanged below,
plus a new `k8s` surface: worker Pod running `agents worker` (omp RPC over TCP
on :8080) + Service; the server dials the pod IP; re-adoption dials
`meta.host:port`.

Former `packages/surfaces/src/` (contract record):

- `index.ts` — `createSurface(kind: SurfaceKind, opts: SurfaceOpts): Surface` where kind ∈ `"process" | "herdr"`; `opts: { ompBin, bridgePath, stateDir, workspace?, herdrBin? }`. Auto-selection lives in the server (config), NOT here.
- `process.ts` — `ProcessSurface`: spawn `omp --mode rpc --session-dir <sessionDir> --no-pty --auto-approve [--max-time <s>] [--model <m>]` via `Bun.spawn` with piped stdio → `Channel` from `proc.stdout`/`proc.stdin`. `stop()`: SIGTERM → 3s → SIGKILL. Expose `pid`. Environment: pass `REMOTE_AGENTS_WORKER=1`, `REMOTE_AGENTS_TASK_ID`.
- `herdr.ts` — `HerdrSurface`:
  1. ensure workspace: `herdr workspace list` → find by name (`opts.workspace`, default `"remote-agents"`); else `herdr workspace create <name>` (parse `.result.workspace.id`, keep the root pane idle — do not use it).
  2. `herdr tab create --workspace <ws> --label ra-<taskId> --cwd <cwd> --no-focus` → `.result.tab`/`.result.root_pane` — use the returned root pane id.
  3. write `<sessionDir>/bridge.mjs` (generated by `bridge.ts`: `renderBridgeScript()` — pure string, unit-testable) and `<sessionDir>/port`.
  4. `herdr pane run <paneId> "bun <bridgePath> --port-file <portFile> -- <ompBin> --mode rpc --session-dir <sessionDir> --no-pty --auto-approve [--max-time] [--model]"` (shell-quote args; single quotes + escaped inner quotes).
  5. poll the port file (500ms, 30s budget), then `Bun.connect({ port })` → `Channel` (socket readable/writable).
  6. `stop()`: close socket → `herdr pane close <paneId>` (best effort). Meta: `{ paneId, workspaceId, port }`.
  - If `herdr` CLI missing or any step fails → throw `SurfaceError("herdr_unavailable", cause)`; server falls back per its config policy.
- `bridge.ts` — emits the self-contained bridge script (runs under `bun`): listens on an OS-assigned port, spawns the given command with piped stdio, pumps socket↔stdio bidirectionally, writes the bound port to `--port-file` atomically, keeps running after client disconnects (worker keeps working), exits when the child exits. Handles multiple sequential connections (new client = same live worker).
- `test/` — process surface with fake-omp (§2 path); bridge round-trip with fake-omp (spawn bridge directly, TCP connect, prompt→result, disconnect, reconnect, still alive); herdr surface marked `test.skip` unless `herdr status` server is running (auto-skip otherwise).

## 4. server [B]

**Moved to Go (2026-09-17 unification):** `internal/server/` (store,
supervisor, HTTP+SSE handler) + `internal/rpc/` (driver) + `cmd/agents
server|worker`. The TS `packages/server` was deleted; `REMOTE_AGENTS_SURFACE`
now also accepts `k8s`; the extension spawns the Go binary
(`REMOTE_AGENTS_SERVER_BIN` → `agents` on PATH). Semantics unchanged below.

Former `packages/server/src/` (contract record):

- `config.ts` — env: `REMOTE_AGENTS_PORT` (default `protocol.DEFAULT_PORT` 8679), `REMOTE_AGENTS_STATE_DIR` (`~/.remote-agents`), `REMOTE_AGENTS_SURFACE` (`auto|process|herdr`; `auto` = herdr if `herdr` on PATH else process), `REMOTE_AGENTS_OMP_BIN` (`omp`), `REMOTE_AGENTS_MODEL`, `REMOTE_AGENTS_MAX_TIME_S` (1800), `REMOTE_AGENTS_URL` (server self-URL for logs).
- `store.ts` — `TaskStore(stateDir)`:
  - `state.json` (all tasks) — atomic write: write `state.json.tmp` + `renameSync`; debounced 25ms; flush + `saveNow()` on shutdown.
  - `events/<taskId>.jsonl` — append per event (`appendFileSync`, full JSON line, `seq` = monotonic per task starting 1).
  - API: `create(input)`, `get(id)`, `list(filter)`, `appendEvent(taskId, type, data)`, `eventsSince(taskId, sinceSeq)`, `update(id, patch)`, `ack(id)`, `all()`. Events also mirrored in memory per task (cap 5000).
- `supervisor.ts` — `Supervisor(store, surface, opts)`:
  - `create(input)` → task `queued` → surface.start → `starting` → `OmpRpc.start()` ok → `running` → `rpc.prompt(task)`.
  - Event wiring: `tool_execution_start` → `activity {toolName, detail: short(args)}`; `message_end(role=assistant)` text buffer; settle → `finalizing` → `getLastAssistantText()` → append `result` → `completed`; channel close before settled → if stop was requested: `canceled`, else `failed` (`error {message: "worker exited (code N)"}`); handshake failure → `failed`.
  - Settle: `agent_settled` settles immediately. **Fallback** (installed omp never emits it): `agent_end` arms a quiet window (`opts.settleQuietMs`, default 3000); `agent_start|turn_start|message_start` cancel it; when it fires, `get_state` is checked — still streaming → re-arm, idle → settle. `settleQuietMs: 0` disables the fallback.
  - Session file: ready frame on the installed omp carries only `{protocolVersion}` (no `sessionFile`), so at settle time the supervisor globs `stateDir/workers/<id>/*.jsonl` (newest) when `sessionFile` is still unset. The worker creates the file with the first message — never glob at prompt time.
  - `steer(id, msg)` (queued+running only), `cancel(id)` (stop requested flag → `rpc.abort()` → surface.stop), `ack(id)`.
  - Deadline: also enforce `opts.maxTimeMs` wall clock as backstop.
  - **Re-adoption (boot)**: for every stored task with status ∈ {queued, starting, running}:
    - `herdr` meta: try TCP connect `meta.port` (3s); on success re-attach `OmpRpc`, poll `getState()` every 1s; when `isStreaming === false` → `finalizing` path (result from `getLastAssistantText()`). Append `log {line: "readopted"}`.
    - `process` meta: `process.kill(pid, 0)` alive → orphaned (no stdio) → `failed` with error `orphaned worker (pid N) — resume via session file`; dead → `failed`.
  - Idempotent: adoption and event wiring must not double-append terminal events (check task.status before transitions).
- `http.ts` — `Bun.serve` router (exact paths from `protocol.API`):
  - `GET /health` → `HealthResponse`.
  - `POST /tasks` (CreateTaskInput; validate name/task/owner non-empty, cwd absolute) → 201 `Task`.
  - `GET /tasks?owner=&unacknowledged=1` → `Task[]` (summary: omit `task` body text? No — include; extension renders previews).
  - `GET /tasks/:id` → `Task` | 404.
  - `POST /tasks/:id/steer` `{message}` → 202 | 409 when not steerable.
  - `POST /tasks/:id/cancel`, `POST /tasks/:id/ack` → `Task` | 404 | 409.
  - `GET /tasks/:id/stream?since=<seq>` → SSE; replay `eventsSince > since` immediately, then live; heartbeat comment every 15s; event frame: `data: {"taskId":…,"seq":N,"type":…,"data":…,"ts":…}\n\n`. Client disconnect must not kill anything.
  - `POST /attach` `{owner}` → `AttachResponse` = non-acknowledged tasks of owner (any status) + `server: {uptimeMs, surface}`.
- `main.ts` — parse config → store → supervisor (createSurface per config; herdr→process fallback logs `log` event on server pseudo-task? no: console) → `Bun.serve` → boot log line `remote-agents server listening on <port> (surface=<kind>, adopted=<n>)` → SIGINT/SIGTERM: stop supervisor children gracefully (abort+stop each running task), `saveNow()`, exit.
- Tests: store (atomicity, cursor, ack); supervisor with fake-omp via process surface (full lifecycle incl. steer + cancel); re-adoption for process surface (orphan/fail) and a TCP fake (bridge path) for herdr adoption; HTTP: all endpoints + SSE replay with `since` + 404/409 paths. Ephemeral ports (`port: 0`).

## 5. extension [C]

`packages/extension/src/` (runs inside omp via jiti; imports `@remote-agents/protocol` + `typebox` only):

- `api.ts` — `RemoteAgentsClient`:
  - `baseUrl` = `process.env.REMOTE_AGENTS_URL ?? "http://127.0.0.1:" + DEFAULT_PORT`.
  - `ensureServer(opts: { serverEntry: string; stateDir: string })`: GET /health (1s timeout); on failure spawn `bun <serverEntry>` DETACHED (`Bun.spawn` with `stdin: "ignore"`, stdout/stderr → `<stateDir>/logs/server.log` opened append, `env` inherited), then poll /health ≤ 5s. Throws `ServerUnreachable` after budget. Never spawns when health OK.
  - Methods: `health`, `create`, `list`, `get`, `steer`, `cancel`, `ack`, `attach(owner)`; `stream(id, sinceSeq, onEvent, signal)` — fetch + manual SSE parse from response body reader (handles `data:` frames, ignores comments/heartbeats, reconnect with backoff 500ms→5s on error, resumes with last seq).
- `runtime.ts` — session-scoped state on `globalThis[Symbol.for("remote-agents/runtime")]` (survives /reload): `{ client, widgetTimer?, subscriptions: Map<taskId, AbortController>, lastSeq: Map<taskId, number> }`. `ensureRuntime(pi, ctx)` creates once. Teardown in `session_shutdown`: clear timer, abort subscriptions (server keeps workers running — document why: workers belong to the server, not the parent session).
- `widget.ts` — `renderWidget(tasks: Task[]): string[]` pure:
  - Hidden when no tasks (return `[]`).
  - Header: `Remote agents · N active · M open` (open = non-acknowledged non-active).
  - Rows (max 8, then `+N more`): `MM:SS  <name> (<surface>)  <state>` + ` · <toolName> <n>s` when running with last activity; elapsed = now-startedAt frozen at terminal. Column-pad name to max 24.
  - TUI-only colors are out of scope (string[] API) — text only.
- `index.ts` — factory:
  - `export default function (pi: ExtensionAPI): void` — no IO in factory body besides `pi.on(...)` registrations (defer to `session_start`).
  - Tools (TypeBox schemas; execute throws on failure, returns `{content:[{type:"text",text}], details}`):
    - `RemoteAgent {name, task, cwd?, model?}` → creates (owner = `process.env.REMOTE_AGENTS_OWNER ?? process.env.USER ?? "local"`, parentSession = `ctx.sessionManager.getSessionFile()`), subscribes SSE, returns `taskId` + note that result arrives automatically. `promptGuidelines: ["Results are delivered automatically as messages; do not poll RemoteAgentGet to wait for completion."]`
    - `RemoteAgentList {}`, `RemoteAgentGet {id}`, `RemoteAgentSteer {id, message}`, `RemoteAgentKill {id}`.
  - Result delivery: on `result` event for task with `deliver: true` → `pi.sendMessage({customType: "remote_agent_result", content: <formatted>, display: <one-liner>, details: {taskId, name, status, elapsedMs, sessionFile}}, {triggerTurn: true, deliverAs: "steer"})`, then `ack`. On `state_changed → failed/canceled` → same channel with failure text + resume hint (`sessionFile`). Never double-deliver (track delivered set in runtime).
  - `session_start` handler: `ensureServer` (failure → `ui.notify(..., "error")`, disable silently for session); `attach(owner)` → for each task: subscribe if active; if terminal & unacknowledged → deliver (steer) + ack. Start widget timer (2s poll of `list(owner)`; stops itself after 2 consecutive empty polls when no subscriptions).
  - Commands: `/remote-agents` (list), `/remote-agents steer <id> <msg>`, `/remote-agents kill <id>`, `/remote-agents logs <id>` (last 10 events via `get`+`eventsSince 0`… implement `events` via stream snapshot endpoint? NO — use `GET /tasks/:id` `recentEvents` field — server includes last 10 events in Task detail response; extension prints compact lines via `ui.notify`).
    → **Contract addition to server [B]**: `GET /tasks/:id` response includes `recentEvents: TaskEvent[]` (last 10). Extension relies on it.
  - `registerMessageRenderer("remote_agent_result", …)` — simple boxed renderer: name · status · elapsed · result text (truncate 2000 chars).
  - `session_shutdown {reason}` — if reason === "quit" keep server + workers (documented); teardown runtime handles only.
- Tests: widget formatting; reconcile (fake server fixture: completed-unacked task → expect sendMessage steer + ack called); SSE parser (chunk-split frames); client against `Bun.serve` fixture; command arg parsing. Mock `pi` object capturing calls. **Do not spawn real omp.**

## 6. Shared fixtures & test rules

- fake-omp path: `packages/rpc-driver/test/fake-omp.ts` (spawn via `bun <path>`).
- Server fixtures bind port 0 and expose actual port. No fixed ports in tests.
- Every package: `bun test` green before hand-off; include test run output in your report.
- No network beyond 127.0.0.1. No real model calls in any package test.

## 7. Integration (owner only)

Root package.json/scripts, contract-mismatch fixes, install script (symlink
extension into `~/.omp/agent/extensions/remote-agents.ts` + document `-e` usage),
README, real-omp E2E, parent-restart E2E, herdr E2E, status.md update.

## Known deliberate v1 limits (do not "fix")

- Process-surface workers die with the server (piped stdio) — re-adoption marks
  them failed with resume hint. Herdr surface survives via TCP bridge.
- Duplicate result risk if parent crashes between steer and ack — accepted.
- Single server instance; no auth (localhost only).
