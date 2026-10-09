# Plan — Unify `agents` repo: one Go server, three surfaces

One Go binary replaces the scattered split (Go controller universe + bun `remote-agents` universe). It runs as a standalone local server spawning omp in herdr/process, and as a Kubernetes controller spawning omp containers with repos pre-cloned via CRDs. One wire protocol; the existing omp extension becomes the only client.

## Context

The repo has two parallel systems doing the same job:

| | Go side | TS side (`remote-agents/`) |
|---|---|---|
| Task model | `AgentTask` CRD (`api/v1alpha1/`) | `Task` in `packages/protocol` |
| Server | `internal/gateway` (store/broker/http, A2A dispatch) | `packages/server` (store/supervisor/http) |
| Worker runtime | custom Anthropic client (`internal/agent`, `internal/runner`, `internal/a2a`) — "adk" | omp RPC child processes |
| Client extension | `pi/pi-remote-agents.ts` (dead — talks to gateway) | `packages/extension` (live, installed in omp) |
| Spawning | Pods (`internal/controller/agenttask`) | herdr panes / process children (`packages/surfaces`) |

Duplicated: task store, SSE broker, HTTP API, extension. The live, loved path is the omp extension + bun server. The Go controller path owns k8s (CRDs, pod lifecycle, skills, output contracts).

**Decision D0 (the unification axiom): omp is the only worker runtime.** Every surface — herdr pane, child process, k8s pod — runs `omp --mode rpc` behind the same line-JSON RPC. The adk runtime (`internal/agent`, `internal/runner`, `internal/a2a`, `internal/output`, `internal/workstrator`, `internal/agentmd`) is deleted; the CRD `runtime` field keeps `pi` and drops `adk`. One RPC driver, one supervisor, one result pipeline. Commit/PR outputs become worker prompts (omp does git natively), not Go code.

**D1 (already in docs/architecture.md) still holds:** everything reproducible from git + 1Password.

## Target shape

```
cmd/agents/            cobra: server | controller | worker (pod entrypoint)
api/v1alpha1/          AgentTask (+ RepoSpec pre-clone), AgentSkill
internal/
  protocol/            Go mirror of remote-agents/packages/protocol (Task, events, API paths)
  server/              HTTP + SSE + store + supervisor  ← absorbs internal/gateway
  surface/             process.go | herdr.go | k8s.go   ← absorbs packages/surfaces
  rpc/                 line-JSON driver + quiet-window settle (port of packages/rpc-driver)
  bridge/              renderBridgeScript() — byte-identical port of packages/surfaces bridge.ts
  controller/agenttask k8s reconciler (stays)
  config/ logging/ kube/   (stay)
remote-agents/packages/
  extension/           UNCHANGED client (talks the same HTTP API)
  protocol/            unchanged (source of truth for wire shapes)
  rpc-driver/          test fixtures only (fake-omp)
  server/ surfaces/    DELETED after parity
```

`agents server` = standalone (process/herdr surfaces, `~/.remote-agents` store, :8679).
`agents controller` = in-cluster (k8s surface: AgentTask → Pod; the API server is the store; SSE from an in-memory broker + status conditions). Both expose the identical extension API. One binary, Dockerfile builds it, ArgoCD ships it (D1).

## Steps

### Phase 1 — protocol + core server in Go (parity with bun server)
1. `internal/protocol/`: Go structs mirroring `packages/protocol/src/index.ts` — `Task`, `TaskEvent`, `TaskEventData`, `CreateTaskInput`, `AttachResponse`, `HealthResponse`, `API` path builders, env constants, `PROTOCOL_VERSION`. JSON tags exactly matching TS (extension must not change).
2. `internal/rpc/`: port `OmpRpc` — line framing, `ready` handshake, request correlation, event dispatch, `OmpRpcError` codes. Port the settle logic: `agent_settled` immediate + `agent_end` quiet-window fallback (3s, `get_state` re-arm).
3. `internal/server/`: store (`state.json` atomic write + `events/<id>.jsonl`, same formats), supervisor (create→starting→running→finalizing→completed/failed/canceled, steer/cancel/ack, re-adoption on boot: herdr TCP re-attach, process orphan-fail), HTTP router (`/health`, `/tasks` CRUD, steer/cancel/ack, `/attach`, SSE `/tasks/:id/stream?since=` with 15s heartbeat), `recentEvents` in task detail. Reuse `internal/gateway/broker.go` patterns for SSE fan-out; reuse `internal/config` (viper AGENTS_* env).
4. `internal/surface/process.go`: `exec.CommandContext` omp `--mode rpc --no-pty --auto-approve`, piped stdio channel, SIGTERM→3s→SIGKILL, `REMOTE_AGENTS_WORKER=1`.
5. `internal/bridge/` + `internal/surface/herdr.go`: port `renderBridgeScript()` byte-identical (bridge.mjs keeps working); herdr CLI calls via exec (`workspace list/create`, `tab create --no-focus`, `pane run`, port-file poll 500ms/30s, TCP connect); `stop()` = close socket + `pane close`.
   - Verify: `go test ./internal/...` with `packages/rpc-driver/test/fake-omp.ts` as fixture (spawn by path); endpoint tests incl. SSE replay; full lifecycle + steer + cancel; re-adoption tests (TCP fake + orphan). Parity checklist against CONTRACT.md §4.

### Phase 2 — cutover
6. `cmd/agents`: add `server` subcommand (port/state/surface/omp-bin/model/max-time from config/env, same names as today). Boot log: `remote-agents server listening on <port> (surface=<kind>, adopted=<n>)`.
7. Flip the extension's spawn target: `ensureServer` in `packages/extension/src/api.ts` runs `bun cmd/agents.go`? No — runs the **built binary** (`~/local/bin/agents server` fallback chain: env `REMOTE_AGENTS_SERVER_BIN` → `agents` on PATH → error telling user to run `task build-agent`). Delete `packages/server` + `packages/surfaces`; keep `protocol` + `rpc-driver` (fixtures) + `extension`.
8. Delete dead TS + dead Go: `remote-agents/packages/{server,surfaces}`, `pi/pi-remote-agents.ts`, `internal/{agent,a2a,runner,output,workstrator,agentmd,gateway,mcp,tool,gwclient}` (gateway code absorbed in step 3), `profiles.go`. Update `Taskfile.yml` (`run:remote-agents` → runs the binary or `go run ./cmd/agents server`), README, docs/architecture.md.
   - Verify: omp session end-to-end — `/remote-agents connect` → spawn → result steer-back → parent restart → re-adopt → `/remote-agents logs`. Old bun server gone; `lsof -i :8679` shows only the Go process.

### Phase 3 — k8s surface + CRD pre-clone
9. `api/v1alpha1`: extend `AgentTaskSpec.Repo` → `RepoSpec {url, ref, clonePath}`; `+kubebuilder:validation:XValidation` oneof with plain `repo` for back-compat. Runtime enum → `pi` only.
10. `internal/surface/k8s.go`: task → AgentTask CRD (or direct Pod when running as controller) — pod = omp image + `bridge` entrypoint (RPC over TCP on the service port) + **pre-clone initContainer** (git clone url@ref into emptyDir at clonePath, creds from the existing secret mount) + service. Supervisor speaks the same RPC over TCP; adopt/reconnect reuses the herdr re-attach path. Pod-phase → task status mapping reuses `internal/controller/agenttask`.
11. `cmd/agents worker` (replaces `agent-entrypoint`): in-pod — waits for RPC TCP connection, runs omp, keeps bridge semantics (survives supervisor restart). Delete A2A server; AgentSkill bundling (skills.go) stays — it mounts skills into the omp session dir.
    - Verify: kind/local cluster — apply sample AgentTask with `repo: yurifrl/agents` → pod starts pre-cloned, task runs, result lands via SSE; `kubectl delete pod` mid-run → supervisor re-adopts or fails with resume hint; deadline → kill; output contract = prompt-level (omp commits/pushes with mounted creds).

### Phase 4 — cleanup + docs
12. docs/architecture.md rewrite: one diagram, three surfaces, one runtime (omp), D0 recorded. Contract.md updated (server ownership moves to Go). `config/crd` regenerate (controller-gen), RBAC for pod/service/CRD ops.

## Verification (whole plan)

- `go test ./...` + `bun test packages/extension packages/protocol packages/rpc-driver` green.
- Live: omp session against `agents server` (herdr surface) — full loop; omp session against in-cluster controller via port-forward — full loop.
- `rg -l "bun packages/server"` → no hits; single :8679 listener at all times.

## Out of scope

- Auth/TLS on the server (localhost / in-cluster only, as today).
- Multi-server coordination, queueing beyond CRDs, Windows.
- Migrating stored `state.json` history (fresh start acceptable; format unchanged so old files still load).
