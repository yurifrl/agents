# Research: agents repo teardown (current state)

_2026-09-07. Source: this repo. What exists today, and the exact seams a local/remote runtime provider must fill._

## 1. Gateway flow today

Routes (`internal/gateway/http.go`): `POST /tasks/create`, `GET /tasks?owner=&acknowledged=`, `GET /tasks/{id}`, `GET /tasks/{id}/stream` (SSE), `POST /tasks/{id}/cancel`, `POST /tasks/{id}/ack`, `GET /health`.

`CreateTask` (gateway.go:68-113): validate owner+type+prompt → id = 8-hex uuid prefix → `store.Create` (StatePending) → build AgentTask CRD → `g.k8s.Create` → `go g.dispatch(task)` → return 201 immediately.

`dispatch` (gateway.go:128-171): `host = g.dialHost(task.ID)` (default `at-<id>.<ns>.svc:8080`) → `waitReady` polls `GET http://host/health` every 2 s up to 2 min → State=Running + publish → `POST http://host/invoke` (Accept: text/event-stream) → frames fanned to the task's broker → terminal event → `finish()`.

**Broker** (broker.go): per-task `{history []Event (unbounded), subs, finished}`; `subscribe()` pre-loads the FULL history into a buffered channel; finished → replay-then-close; else live sub. Reconnect = whole-history replay (no per-client cursor). Brokers never freed (leak).

**Reaper** (reaper.go, defaults 5 m interval / 30 m max-running / 24 h retention): pending|running past MaxRunning → Cancel; terminal+acknowledged past Retention → store.Delete.

States: `pending | running | succeeded | failed | canceled` (gateway-authoritative, independent of k8s phases).

## 2. In-pod agent surface

One binary, five cobra commands (`cmd/agents/`): `controller`, `agent-entrypoint`, `gateway`, `workstrator`, `mcp`.

Entrypoint: one-shot (env `AGENT_PROFILE/AGENT_PROMPT/AGENT_REPO/AGENT_OUTPUT`) or `--serve` → `a2a.Server` (AgentCard at `/.well-known/agent-card.json`, `GET /health`, `POST /invoke` SSE: EventStatus{running} → run → EventResult|EventError).

Runner (`internal/runner/`): model = `req.Model || def.Model`; `output: response` → single completion (`client.Run`) or tool loop (`RunLoop` with OnToolEvent → EventToolCall/EventToolResult, redacted); `output: commit|pr` → `client.Plan` (structured Change via strict-JSON prompt) → `output.PR` (go-git clone depth 1, branch `agent/<slug>`, commit, push, REST PR — pure Go, distroless D5/D7).

Agent client (`internal/agent/`): Anthropic Messages API, maxTurns 10, tool loop when `SetTools` attached (currently dead in production paths — entrypoint never attaches tools).

## 3. AgentTask model

Spec (`api/v1alpha1/agenttask_types.go:72-145`): `Type` (profile) XOR `Definition` (inline Agent.md), `Prompt`, `Skills[]` (inline only; `{name}` or `{selector}`), `Image`, `ServiceAccountName`, `SecretName`, `Env[]`, `Repo`, `Output` (response|commit|pr), `Owner` (required), `Runtime` (adk|pi), `Lifecycle` (ephemeral|persistent), `ActiveDeadlineSeconds`.
Status: `Phase (Pending|Running|Succeeded|Failed|Canceled), PodName, Artifacts, Restarts, Message, StartedAt, CompletedAt, ObservedGeneration, Conditions`.

Controller: ephemeral → Pod (`at-<task>`, RestartPolicy Never, ActiveDeadlineSeconds, envFrom Secret, readiness /health) + Service; recreate-on-fail until RestartBudget (3); max-concurrency guard; persistent lifecycle SKIPPED (design doubt).

⚠ **Generated CRD is stale vs types** — `config/crd/` lacks definition/skills/image/serviceAccountName/secretName/env and still requires type+prompt. Needs `task manifests` before inline-mode work.

⚠ **Gateway↔CRD gap**: `CreateTask` populates only Type/Prompt/Repo/Owner/Output — the new spec fields are never set by the gateway.

## 4. pi extension ↔ gateway contract

`pi/pi-remote-agents.ts` — env `AGENTS_GATEWAY_URL` (default :9090), `AGENTS_OWNER` ($USER):
- `RemoteAgent {type, prompt, repo?, output?, background?}` → POST /tasks/create; foreground streams SSE to `onUpdate`; background returns the id.
- `RemoteAgentList/Get/Kill` → list/get/cancel.
- `session_start` reconciliation: unacked succeeded|failed → `ctx.ui.notify("N remote agent result(s) waiting")`.
- Gaps: no steer endpoint; foreground abort kills only the fetch (no cancel-on-disconnect); notify-only reconciliation (no auto-ack, no result injection into the conversation).

## 5. Statefulness gaps

`MemoryStore` = map + RWMutex. Gateway restart loses: all Task records, broker history, every in-flight dispatch goroutine. Post-restart: GET /tasks/{id} 404s; stream hangs on a fresh empty broker while the pod keeps running, unreachable; owner list empty so reconciliation sees nothing. CRDs survive in-cluster but nothing re-adopts them.

docs/status.md Pending: durable (Dolt) TaskStore; persistent lifecycle (shared Deployment ownership + name routing); ADK-vs-native tool loop; foreground-disconnect cancel (SSE hold/release); pi widget; RemoteAgentSteer.

## 6. Extension points for a non-k8s runtime

Already provider-friendly:
- `TaskStore` interface (Create/Get/List/Update/Delete) — swap for Dolt/SQL.
- `dialHost func(taskName) string` — the ONLY cluster-DNS assumption in dispatch; return docker/localhost/remote host:port.
- `Gateway.http` injectable; `readyTO` tunable. waitReady + invoke are plain HTTP — no k8s.

Must be built for a local/docker/remote provider:
1. **WorkloadLauncher abstraction** behind the two hard-coded `g.k8s` call sites — `CreateTask`'s CRD create (gateway.go:90-109) and `Cancel`'s CRD delete (gateway.go:229-246, reused by the reaper). Interface: `Launch(task) error / Cancel(id) error` with k8s | local-process | docker | remote-server impls. (`g.k8s` nil is not tolerated today.)
2. **dialHost impl**: task id → addressable endpoint exposing `/health` + `/invoke` (SSE).
3. **Provider-side deadline/kill**: ActiveDeadlineSeconds + reaper MaxRunning must map to real process/container kills.
4. **Env plumbing**: ANTHROPIC_API_KEY, GITHUB_TOKEN, GIT_AUTHOR_* as plain env (no mounted Secret).

Untouched if provider-only: a2a types/server, runner, agent, output, gateway HTTP + broker/replay, pi extension, workstrator, mcp, gwclient.

**Key insight**: the gateway already IS 70 % of the "agent server" — owner-scoped store, SSE fan-out with replay, cancel/ack/list, reaper, and an HTTP-only client contract. What's missing: durable store, re-adoption after restart, a workload-launcher seam, and a streaming-richer invoke surface (steer, tool events to the client, attach).

## Open questions

1. One gateway instance (SQL store enough) or HA (needs event log, not just task rows — broker history + dispatch must survive)?
2. Should dispatch re-adopt orphaned CRDs/tasks post-restart?
3. Persistent lifecycle: one Deployment per AgentTask (design doc) vs shared warm pool keyed by profile — name-based routing needed either way.
4. Steer: needs mid-run input injection into the runner loop — design from scratch (a2a has no such path today).
