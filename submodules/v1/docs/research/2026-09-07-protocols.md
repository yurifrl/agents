# Research: agent protocols & runtime options (survey)

_2026-09-07. Web research by subagent; primary sources verified (a2a-protocol.org, github.com/a2aproject, agentclientprotocol.com, modelcontextprotocol.io). Dates noted._

## 1. A2A (Agent2Agent — Linux Foundation → Agentic AI Foundation)

- **v1.0 shipped 2026-03-12**; v0.3 widely deployed; AgentCard backward-compatible (can advertise both). SDKs lag spec (`a2a-sdk` PyPI stable 0.3.25, 1.0.0a0 alpha). 150+ orgs; landed in Agentic AI Foundation Aug 2026.
- Shape: Agent Card at `/.well-known/agent-card.json` (v1.0: `supportedInterfaces[]` with `protocolBinding` JSON-RPC|gRPC|HTTP+JSON + JWS-signed cards); Tasks = server-generated-ID state machine (`SUBMITTED/WORKING/INPUT_REQUIRED/AUTH_REQUIRED/COMPLETED/FAILED/CANCELED/REJECTED`); ops `message/send`, `message/stream` (SSE), `get_task`, `list_tasks`, `cancel_task`, **`SubscribeToTask`** (reattach to an interrupted stream — the reconnect primitive), push-notification config CRUD; artifacts multi-part/streamable; auth via OpenAPI-style `securitySchemes` (API key, OAuth2 device-code+PKCE, mTLS).
- **No `tasks/resume`** — resume = SendMessage on an INPUT_REQUIRED task; stream reattach = SubscribeToTask.
- Fit: tasks map 1:1 to subagent runs; SSE, cancel, artifacts, SubscribeToTask, GetTask-for-crash-recovery are exactly the wanted primitives. **Overkill for v1**: three binding equivalence (gRPC), multi-tenancy, push-config CRUD, signed cards, skills discovery for open multi-agent negotiation, DataParts, OAuth machinery.
- Adoption: **no coding harness/CLI speaks A2A natively**; official `a2a-cli` is a debug client. Framework support is business-agent oriented.

## 2. ACP — Agent Client Protocol (Zed, agentclientprotocol.com)

- "LSP for coding agents": JSON-RPC 2.0 over **stdio** ndjson, editor↔agent. Client→agent: `initialize`, `session/new`, **`session/load`** (reattach persisted session), `session/prompt`, `session/cancel`, `session/set_mode`, `session/set_model`. Agent→client: `session/update` notifications (message chunks, `tool_call`, `tool_call_update`, plan), **`request_permission`** (agent-initiated), client-side tool callbacks (`fs/read_text_file`, terminal). Extensibility via `_meta`; MCP server config passed at session/new.
- **Transports beyond stdio: none standardized yet** — a Streamable HTTP & WebSocket Transport RFD (draft 2026-08-05) proposes a single `/acp` endpoint: POST for client→server (202 + correlated responses), long-lived SSE GET for server→client. Not final.
- omp ships `omp acp` (stdio server; also `session/list`, `session/resume`, `session/fork`). ACP has richer *session reattach* than omp's raw RPC but a thinner control surface (no direct bash, no paging, no subagent bus).
- Adopters: Zed, claude-code-acp adapters, gemini-cli — editor-embedding land.

## 3. Google ADK (Agent Development Kit)

- Python/Java **framework** for building agents (multi-agent hierarchies, tools, sessions/state, evaluation, deploy to Agent Engine/Cloud Run). A2A interop is a feature (expose an ADK agent as an A2A server).
- For this project: it answers "how do I write an agent", not "how do I drive/organize harness workers". The repo's existing doubt (ADK vs native tool loop) resolves to: **the worker is already a full coding harness (omp) — a framework adds nothing but a dependency**. The `runtime: adk|pi` field in the CRD reflects this unresolved fork; the honest v1 is "runtime: harness" with the worker being any CLI.

## 4. MCP (Model Context Protocol)

- Tool/resource server protocol — the wrong layer for agent control (no tasks, no streaming agent events, no lifecycle). Relevant only as: (a) workers may consume MCP servers as tools; (b) an agent-server could expose its control API *as* MCP tools for harnesses lacking extensions. 2025-2026 additions (streamable HTTP, OAuth, remote servers) matter for the second case.
- Note: both pi and omp have MCP adapters (`pi-mcp-adapter` installed globally; `internal/mcp` in this repo already exposes `agents_*` + `beads_*` over stdio) — the MCP facade is a viable fallback client surface.

## 5. Others (one-liners)

- **OpenAI Agents SDK handoffs**: in-framework agent-to-agent handoff; not a wire protocol.
- **LangGraph Platform API**: graph-hosting REST with checkpoints/thread state; heavyweight, Python-centric.
- **AG-UI**: event protocol for agent→UI streaming (chat apps); overlaps with our SSE event stream, UI-oriented.
- **Agent Protocol (AI Engineer Foundation)**: REST task spec, effectively stalled; A2A superseded it.

## 6. Runtime options for "where the subagent runs"

| Runtime | Spawn | Stream | Supervise | Reattach |
|---|---|---|---|---|
| Plain process | `exec.Command` / daemon (omp `ps` pattern) | stdio pipes → SSE | PID + exit wait, backoff restart (hub watcher pattern) | `--resume <sessionFile>` in a new process (verified) |
| Docker container | `docker run -i image omp --mode rpc` | stdio via `docker attach`/`exec -i`, or published port | `docker inspect` health, restart policies | keep sessions volume; `docker start` + `--resume` (official containerization doc pattern) |
| systemd user service | unit per worker | journald or port | systemd restart policies | service restarts with same session volume |
| microVM (gondolin) | host harness routes tools into VM | host-side | VM lifecycle | write-through workspace |
| k8s pod | CRD → Pod (existing controller) | Service DNS → /invoke SSE | controller + reaper (existing) | CRD durable; re-dispatch |

Auth for remote: A2A securitySchemes / bearer tokens; omp `auth-broker` + `auth-gateway` lets workers fetch provider tokens from a vault instead of shipping raw keys (the clean pattern for multi-worker hosts).

## 7. Comparison & recommendation inputs

| Protocol | Transport | Primitives | Reattach | Streaming | Auth | Worker cost | Harness cost | Maturity |
|---|---|---|---|---|---|---|---|---|
| Custom JSON/HTTP+SSE (repo internal/a2a) | HTTP | create/invoke/cancel/ack/list + SSE replay | whole-history replay (extendable) | SSE | bearer (your choice) | must speak /invoke | **zero** (extension exists) | ours |
| A2A subset | HTTP+JSON (JSON-RPC optional) | tasks, message/stream, cancel, artifacts, SubscribeToTask | SubscribeToTask + GetTask | SSE | securitySchemes | speak A2A (SDK or minimal) | new extension or bridge | spec v1.0, SDKs lag |
| ACP | stdio (HTTP RFD draft) | session/new/load/prompt/cancel, permissions, tool events | session/load | notifications | none std | be an ACP agent (omp is) | client lib + transport | real adopters, stdio-bound |
| MCP | stdio/HTTP | tools/resources only | n/a | n/a | OAuth (remote) | trivial | mcp facade exists | huge, wrong layer |

**Recommendation (boring-first)**: keep the gateway's custom HTTP+SSE contract as the core protocol (it already exists, the pi extension already speaks it, and it maps 1:1 to the domain), but: (1) borrow A2A's task-state vocabulary and `SubscribeToTask`/`GetTask` semantics for reattach — aligning names now makes an A2A-compliant facade cheap later; (2) keep workers as **any CLI behind a thin adapter** (the herdr HarnessDriver lesson) — omp/pi workers speak omp RPC on stdio, non-omp CLIs get wrapped; (3) don't adopt A2A/ADK/ACP wholesale in v1 — ACP's HTTP transport isn't final, A2A SDKs lag the spec, and both buy nothing the gateway + RPC-mode workers don't already provide. Revisit A2A when a harness-side client appears.

## Sources

- https://a2a-protocol.org/latest/blog/2026/03/12/a2a-protocol-ships-v10-production-ready-standard-for-agent-to-agent-communication/
- https://github.com/a2aproject/A2A/blob/main/docs/whats-new-v1.md
- https://www.linuxfoundation.org/press/a2a-protocol-surpasses-150-organizations-lands-in-major-cloud-platforms-and-sees-enterprise-production-use-in-first-year
- https://agentclientprotocol.com/rfds/streamable-http-websocket-transport
- https://modelcontextprotocol.io (spec)
- omp/pi docs (installed package), containerization.md, security.md
