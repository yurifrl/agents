# Status

What is actually built vs. pending. This is the truth about *current* state;
keep it honest, including known corner-cuts that still need fixing.

_Last reconciled: 2026-06-15._

## Built

- **`AgentTask` CRD** (`api/v1alpha1`) — spec: type, prompt, repo, owner,
  runtime, lifecycle, output; status: phase, podName, artifacts, restarts.
- **Controller (ephemeral)** — `AgentTask` → Pod (+Service), owner refs,
  `activeDeadlineSeconds`, configurable `imagePullSecrets`, pod-phase → status.
- **Agent entrypoint** — `agent-entrypoint`; one-shot or `--serve` (in-pod A2A
  server: agent card, `/invoke` SSE, `/health`). SIGTERM-aware.
- **A2A surface** (`internal/a2a`) + **runner** (`internal/runner`) — shared
  invocation orchestration emitting streamed events; `output: response` and
  `output: pr|commit` (go-git clone/commit/push + GitHub API). Verified: a pod
  opened a real PR against `yurifrl/agents`.
- **Gateway** (`internal/gateway`, `agents gateway`) — owner-scoped task store
  (in-memory MVP), creates AgentTask CRDs, waits for pod readiness, dispatches
  `/invoke`, fans out SSE with replay (reconnection), cancel/ack/list +
  reconnection API, plus a **TTL/stale reaper** (cancel runaway running tasks,
  prune old acknowledged tasks). Pod-side A2A verified locally; full gateway
  loop needs the cluster (pod Service DNS).
- **Workstrator** (`internal/workstrator`, `agents workstrator`) — claims ready
  beads (atomic `--claim` dedup), dispatches to the gateway, polls to completion,
  comments + closes on success / unclaims on failure; repo mapping.
- **MCP facade** (`internal/mcp`, `agents mcp`) — stdio MCP server exposing
  `agents_*` (wrap gateway) + `beads_*` (wrap bd) for clients like hermes;
  verified `tools/list` over stdio.
- **pi extension** (`pi/pi-remote-agents.ts`) — `RemoteAgent`, `RemoteAgentList`,
  `RemoteAgentGet`, `RemoteAgentKill` + startup reconciliation. Transpiles;
  end-to-end needs a running gateway.
- **Controller (ephemeral)** — `AgentTask` → Pod (+Service), owner refs,
  `activeDeadlineSeconds`, configurable `imagePullSecrets`, pod-phase → status,
  **restart budget** + **max-concurrency** guard (3.3).
- **Deployment (declarative)** — `k8s/charts/agents` (home-systems): controller +
  gateway (+Service) + optional workstrator Deployments, RBAC/ServiceAccounts,
  and the vendored CRD in `crds/`; wired into `k8s/applications/agents.yaml`
  alongside the `agents`/ghcr ExternalSecrets.
- **Image** — `ghcr.io/yurifrl/agents`, distroless, multi-arch (amd64+arm64).

## Pending

- **Durable task store (D5)** — the gateway store is in-memory; reconciliation
  state is lost on gateway restart until a SQL (Dolt) `TaskStore` lands.
- **Persistent lifecycle (3.5/3.6)** — stopped on genuine design doubt: how a
  *shared* persistent agent Deployment is owned/GC'd across AgentTasks, and how
  the gateway routes to it by name. Controller still skips persistent tasks.
- **ADK tool-use loop (2.2/2.4)** — stopped on doubt (google ADK vs native
  Anthropic tool-use, large surface). The worker does a single structured edit,
  not a multi-step tool loop. Resolved by research (D2 in
  `research/2026-09-07-remote-subagents-design.md`): `runtime: adk|pi` becomes
  `runtime: harness` — the worker is a full coding CLI driven over omp RPC;
  ADK is an authoring framework, not a runtime.
- **Foreground-disconnect cancel (D6, 4.4/5.5)** — SSE hold/release semantics
  not wired; background vs foreground is a pi-side flag only.
- **pi widget integration (5.3)** — tools work; no custom remote-agent widget.
- **`RemoteAgentSteer`** — no steer endpoint on the gateway.
- **1Password `agents` item** — exists in the Kubernetes vault with
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GITHUB_TOKEN`, `GIT_AUTHOR_NAME`,
  `GIT_AUTHOR_EMAIL`. The `agents` ExternalSecret maps these by exact label;
  no manual prerequisite remains. (The agent runs on Anthropic/Claude; the
  OpenAI key is present but unused unless the client is switched.)
- **CRD source vs vendored** — the CRD is vendored into `k8s/charts/agents/crds/`
  (matching the hermes chart pattern) because the agents repo is private; keep it
  in sync with `config/crd` on CRD changes.

## Built: remote subagents, local-first (2026-09-07)

- `remote-agents/` — server-managed omp workers driven from a parent omp
  session. Monorepo (`packages/protocol|rpc-driver|surfaces|server|extension`),
  Bun, TypeScript. omp RPC JSONL over stdio (process surface) or TCP bridge
  (herdr surface). HTTP+SSE API on 127.0.0.1:8679; durable JSON store + per-task
  event logs; re-adoption on boot; steer/cancel/ack; widget + `RemoteAgent*`
  tools + `/agents` commands; result steer-back; session-file resume hints.
  **Verified live**: `bun test` 166/166; in-process server e2e; HTTP e2e
  (`completed`, result + sessionFile recorded); real `omp -p` session spawned a
  worker and received `PING-99` back through the extension. Contract record:
  `remote-agents/CONTRACT.md`. Notable finding: installed omp (pi 0.84.4) never
  emits `agent_settled` — the supervisor settles on `agent_end` + 3s quiet
  window (see CONTRACT §4).

## Known corner-cuts to repay (debt log)

- **Secrets — repaid in source.** The `agents` ExternalSecret and the ghcr pull
  secret are now declared in `home-systems` (`k8s/applications/agents.yaml`) via
  the support chart + 1Password. The live cluster still holds the hand-applied
  versions; they are superseded the moment ArgoCD syncs the app.
- **Namespace + CRD — still imperative.** Created by hand during the demo.
  Namespace is covered by `CreateNamespace=true`; the CRD install remains debt
  (see Pending) until the private-repo source access is resolved. Until both are
  declarative, the cluster is not fully reproducible from source.
