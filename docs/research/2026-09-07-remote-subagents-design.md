# Design: Remote Subagents — omp talks to an agent server

_2026-09-07. Synthesis of docs/research/2026-09-07-*.md. Goal: open omp (or pi), spawn subagents that run **elsewhere** — another process, a docker container, a server — with stateful pickup after restart, and native-feeling UI. Drop or keep pi deliberately; keep Kubernetes as one runtime, not the only one._

## 0. The one-paragraph answer

The `agents` repo already contains ~70 % of the "agent server": a stateful gateway with an owner-scoped task store, SSE streaming with replay, cancel/ack/list, a reaper, and a pi extension that speaks HTTP to it. The missing pieces are: (1) a **workload-launcher seam** so the gateway can start workers as local processes / docker containers / remote-server requests instead of only k8s pods; (2) a **durable store + re-adoption** so tasks and running workers survive gateway restarts; (3) a **streaming-richer worker contract** (omp RPC over stdio instead of one-shot /invoke) so steering, tool events, and reattach work; (4) an **omp extension with widget + commands** so remote agents feel like native subagents. No new protocol is needed in v1: keep the repo's HTTP+SSE (an A2A subset in spirit), align its vocabulary with A2A task states, and wrap any harness CLI behind a driver interface. ADK is out (a framework for writing agents, not driving them); ACP stays out until its HTTP transport lands; MCP remains a facade option, not the core.

## 1. Where subagents can run — one model, four surfaces

Every option is the same architecture with a different `Surface` (launch/attach/observe/kill):

| Surface | Launch | Stream | Reattach | Isolation | v1? |
|---|---|---|---|---|---|
| **Local process** (headless) | spawn `omp --mode rpc --session-dir …` | stdio JSONL → gateway SSE | new process `--resume <sessionFile>` (verified) | none (flag-level) | ✅ simplest, first |
| **Docker container** | `docker run -i … omp --mode rpc` (sessions volume) | stdio via attach, or published port | keep volume; re-exec `--resume` | container boundary; official upstream pattern | ✅ second |
| **Remote server** | POST to a small runner daemon on the host | the daemon's SSE | daemon re-execs `--resume`; attach-on-connect | host boundary + `auth-broker` for creds | ✅ the real target |
| **Kubernetes pod** | existing AgentTask controller | Service DNS → /invoke SSE | CRD durable; re-dispatch | strongest | already built |

The pi-herdr lesson: the *surface* is six operations (`create/run/read/inspect/sendKeys/close`); everything else — drivers, completion sidecars, session seeding, lifecycle state machine, registry, steering — is surface-agnostic. The herdr pane is just the first surface; here we standardize the others.

## 2. Architecture (v1: local server + omp extension)

```
omp session (extension: remote-agents)
  │  RemoteAgent / RemoteAgentList / RemoteAgentGet / RemoteAgentKill / RemoteAgentSteer
  │  widget (setWidget) + /agents commands + session_start reconciliation (steer-back)
  ▼  HTTP + SSE (the existing gateway contract, extended)
┌──────────────────────────── agent server (gateway) ────────────────────────────┐
│  HTTP API: create/list/get/cancel/ack/stream/attach/steer                       │
│  TaskStore (durable: Dolt/SQL)          broker: per-task SSE w/ history+cursor  │
│  Supervisor: spawn/kill/restart/adopt   reaper (TTL, stale, deadline)           │
│  WorkloadLauncher (provider seam):                                              │
│    local-process │ docker │ remote-runner │ k8s (existing)                      │
└──────────────┬──────────────────────────────────────────────┬──────────────────┘
               ▼ stdio omp RPC                                 ▼ HTTP
        worker: omp --mode rpc --session-dir …          remote runner daemon
        (or any CLI behind a HarnessDriver)             (starts omp workers on a host)
```

Key choices, each with the evidence behind it:

- **D1 — keep the gateway's HTTP+SSE; align vocabulary with A2A.** The client contract (extension) already exists and works; the whole-history broker replay becomes cursor-based (`?since=<eventId>` — A2A's SubscribeToTask semantics). Renaming task states to A2A's (`submitted/working/completed/failed/canceled`) is free now and keeps an A2A-compliant facade a thin adapter later. Adopting full A2A v1.0 (three bindings, signed cards, push-config CRUD) buys nothing today — no harness speaks it natively and SDKs lag the spec.
- **D2 — workers are omp RPC processes, not one-shot /invoke.** The current in-pod agent does a single completion (or a plan+PR); that's why steer/tool-streaming/attach don't exist. A worker that is `omp --mode rpc` behind stdio gives: prompt/steer/abort, tool events, `agent_settled` completion signal, session file on disk, `--resume` reattach — all verified. The `runtime: adk|pi` CRD fork resolves to **`runtime: harness`**: the worker is any coding CLI; a HarnessDriver per CLI (herdr's abstraction, verbatim) builds the command. ADK is a framework for writing agents — it doesn't drive harnesses; drop it.
- **D3 — durable state, re-adoption on restart.** Store: Dolt (repo already uses it for beads; fits the litmus test) — task rows + event log (events, not just tasks, so broker replay survives restart). Re-adoption: on boot the gateway lists its own unacknowledged tasks (and the provider's live workers) and re-attaches — the pi-hub attach-on-connect pattern (sessions announce `attached:true`; upsert idempotent; grace-reap what doesn't re-announce in 60 s). Crash resumability is guaranteed by the session-file format (append-only JSONL; flushes after first assistant message).
- **D4 — one supervisor model everywhere.** Bounded-backoff restart (max N, doubling, stability reset), SIGTERM→grace→SIGKILL kill, deadline→kill, adoption of durable state. Copy from pi-hub's watcher + omp_agent adapter; it's already written and tested in TS — port the shape, not the code.
- **D5 — the omp extension owns UX parity with native subagents.** Widget via `ctx.ui.setWidget` (rows like the native Subagents list: name, agent type, task preview, state, elapsed; blue while active, amber when all open); results steer back with `pi.sendMessage({customType:"remote_agent_result"}, {triggerTurn:true, deliverAs:"steer"})`; reconciliation on `session_start` steers unacked results into the conversation instead of a notify. Commands: `/agents` (list), `/agents attach <id>` (re-stream a running/finished task), `/agents steer <id> <msg>`, `/agents kill <id>`, `/agents logs <id>`. Tools stay model-facing; commands are user-facing.
- **D6 — secrets via `omp auth-broker`/`auth-gateway`** for multi-worker hosts (workers fetch provider tokens from the vault) instead of shipping `ANTHROPIC_API_KEY` everywhere; k8s keeps ESO.

## 3. The agent server API (delta from today)

Existing (keep): `POST /tasks/create`, `GET /tasks?owner=`, `GET /tasks/{id}`, `POST /tasks/{id}/cancel`, `POST /tasks/{id}/ack`, `GET /tasks/{id}/stream` (SSE), `GET /health`.

Extend:
- `GET /tasks/{id}/stream?since=<eventId>` — cursor replay (was: whole history).
- `POST /tasks/{id}/steer {message}` — inject mid-run into the worker's RPC `steer`. (Today: doesn't exist; runner has no input path — fixed by D2.)
- `POST /tasks` with `definition` inline Agent.md + `skills` — gateway sets the full spec (today it populates only type/prompt/repo/owner/output).
- `GET /tasks/{id}/events?since=` — NDJSON alternative to SSE for CLI clients.
- `POST /attach {owner}` — returns live+recent tasks for pickup after parent restart (replaces notify-only reconciliation).

## 4. Worker contract (the new core)

A worker is **a process the supervisor started, speaking omp RPC JSONL on stdio, bound to a session file the supervisor chose**:

1. Supervisor generates the deterministic session path (herdr's race-free pattern) and launches the driver command (env: `PI_SUBAGENT_NAME/ID/SESSION`, task as first `prompt`).
2. Supervisor watches: RPC events (`agent_settled` = completion; `tool_execution_*` → SSE `tool_call`/`tool_result` frames; `message_end` deltas → SSE artifacts) + process exit + deadline.
3. Result: last assistant text from the stream (or `get_last_assistant_text`); persisted in the task row; sidecar/exit-code reconciled.
4. Death before completion: task stays `running` with `reattachable: true` (session file known); re-dispatch = relaunch driver with `--resume <sessionFile>` — mid-task pickup, not restart-from-scratch.
5. Any CLI can be a worker: pi driver (`pi --mode rpc`), one-shot drivers for CLIs without RPC (herdr's claude/codex/opencode/grok/generic drivers) — completion via sentinel/sidecar, no steer.

The `.exit` sidecar + sentinel protocol from herdr stays for one-shot drivers; RPC drivers don't need it (the stream is the evidence).

## 5. Statefulness matrix (the requirement: close pi, restart, pick up)

| What restarts | What survives | How |
|---|---|---|
| parent omp session | remote tasks (server-side), session file, results | `session_start` → `POST /attach` → steer unacked results into the fresh session |
| agent server | task rows + event log (Dolt), worker processes (children unaffected), broker replay (event log) | re-adopt: list live workers, re-open brokers, cursor replay for clients |
| worker process | session JSONL | supervisor relaunches with `--resume` (verified binding) |
| worker host (container/server) | session files (volume/daemon disk) | same as above from a new host |
| everything | archive | task rows are the record; sessions on durable volume |

## 6. What we drop (decisions against)

- **Drop pi as a separate runtime.** omp already is pi (fork) + plugins; the omp extension compiles against the same `@earendil-works/pi-coding-agent` SDK. One harness, one extension host. pi remains available as a *worker* CLI via the driver if wanted.
- **Drop ADK.** Framework for authoring agents; we drive full harnesses. The tool loop comes free from the harness (RunLoop already exists in-repo for the k8s one-shot path).
- **No A2A SDK, no ACP client in v1.** Borrow semantics (task states, SubscribeToTask, session/load) — not dependencies. Revisit A2A when a harness speaks it natively.
- **K8s demoted from "the system" to "a provider".** Controller/CRD stay for the cluster story (home-lab workers), but v1 dogfoods local-process + docker + remote-runner.

## 7. Milestones (each shippable)

1. **M1 — provider seam + local-process provider.** Extract `WorkloadLauncher` from the two `g.k8s` call sites (create/cancel); implement `local` (spawn omp RPC worker per §4); gateway runs on localhost; existing pi extension drives it end-to-end. *Proof: spawn a remote agent from omp, see tool events stream, get result steered back.*
2. **M2 — statefulness.** Dolt TaskStore + event log; cursor replay; re-adoption + `POST /attach`; extension reconciliation steers results. *Proof: kill the gateway mid-task, restart, client reconnects with cursor, worker finishes, result delivered.*
3. **M3 — omp UI parity.** Widget + `/agents` commands + steer + kill. *Proof: screenshot parity with the native Subagents widget.*
4. **M4 — docker provider.** Sessions volume + `docker run -i` driver. *Proof: container worker survives gateway restart via --resume.*
5. **M5 — remote runner.** Small daemon (Go, same binary: `agents runner --listen :7000`) exposing start/kill/logs over the same HTTP contract with bearer auth + auth-broker integration; gateway `remote` provider POSTs to it. *Proof: subagent runs on another machine, steerable from local omp.*
6. **M6 — k8s provider parity.** WorkloadLauncher k8s impl wrapping the existing controller path; CRD regenerated (`task manifests`) and gateway sets the full spec.

## 8. Open questions (carried)

1. `--resume` with-content continuation: one real-model smoke test before M2 builds on it.
2. Single-instance gateway vs HA: v1 is single-instance + durable event log; HA needs consensus — defer.
3. Who owns cleanup for orphaned local workers after a gateway crash (reaper adopts by scanning the provider's worker registry — provider must persist worker pids/containers).
4. omp native subagent bus (`get_subagents`): can extension-spawned remote agents register there for true widget parity, or is `setWidget` the ceiling? (Probe suggests the command exists in every rpc child; population semantics undocumented.)
