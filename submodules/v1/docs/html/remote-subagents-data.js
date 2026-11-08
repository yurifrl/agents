window.DATA = window.DATA || {};
window.DATA.surfaces = [
  {
    id: "local-process",
    name: "Local process",
    tagline: "headless omp on the same machine",
    launch: "spawn omp --mode rpc --session-dir …",
    stream: "stdio JSONL → gateway SSE",
    reattach: "new process, --resume <sessionFile> (verified)",
    isolation: "flag-level",
    v1: true,
    note: "simplest; the first provider to build"
  },
  {
    id: "docker",
    name: "Docker container",
    tagline: "official upstream containerization pattern",
    launch: "docker run -i … omp --mode rpc (sessions volume)",
    stream: "stdio via attach, or published port",
    reattach: "keep volume; re-exec with --resume",
    isolation: "container boundary",
    v1: true,
    note: "second provider; credentials stay out via volumes"
  },
  {
    id: "remote-server",
    name: "Remote server",
    tagline: "small runner daemon on another host",
    launch: "POST to runner daemon (agents runner)",
    stream: "daemon SSE → gateway → client",
    reattach: "daemon re-execs --resume; attach-on-connect",
    isolation: "host boundary + auth-broker creds",
    v1: true,
    note: "the real target: subagents on a server somewhere"
  },
  {
    id: "k8s",
    name: "Kubernetes pod",
    tagline: "the existing controller path",
    launch: "AgentTask CRD → Pod + Service",
    stream: "Service DNS → /invoke SSE",
    reattach: "CRD durable; re-dispatch",
    isolation: "strongest",
    v1: false,
    note: "already built; demoted from 'the system' to 'a provider'"
  }
];

window.DATA.protocols = [
  { dim: "Transport", custom: "HTTP + SSE (exists today)", a2a: "HTTP+JSON / JSON-RPC / gRPC", acp: "stdio only (HTTP RFD draft)", mcp: "stdio / streamable HTTP" },
  { dim: "Primitives", custom: "create/invoke/cancel/ack/list + SSE", a2a: "tasks, message/stream, artifacts", acp: "session/new|load|prompt|cancel", mcp: "tools & resources only" },
  { dim: "Reattach", custom: "broker replay → cursor (planned)", a2a: "SubscribeToTask + GetTask", acp: "session/load (native)", mcp: "n/a" },
  { dim: "Streaming", custom: "SSE events (exists)", a2a: "SSE status/artifact updates", acp: "session/update notifications", mcp: "n/a" },
  { dim: "Worker cost", custom: "speak /invoke or be a CLI driver", a2a: "A2A SDK or minimal impl", acp: "be an ACP agent (omp is)", mcp: "trivial but wrong layer" },
  { dim: "Harness cost", custom: "zero — extension exists", a2a: "new extension or bridge", acp: "client lib + transport", mcp: "facade exists (internal/mcp)" },
  { dim: "Maturity (2026-09)", custom: "ours, working", a2a: "spec v1.0; SDKs lag; no harness adoption", acp: "real adopters; stdio-bound", mcp: "huge — but not agent control" }
];

window.DATA.decisions = [
  {
    id: "D1", title: "Keep HTTP+SSE; align vocabulary with A2A",
    body: "The gateway contract already works and the pi extension already speaks it. Cursor-based replay (since=<eventId>) matches A2A SubscribeToTask semantics; renaming states to A2A's vocabulary keeps a compliant facade a thin adapter later. Full A2A v1.0 (3 bindings, signed cards, push CRUD) buys nothing — no harness speaks it natively.",
    verdict: "borrow semantics, not dependencies"
  },
  {
    id: "D2", title: "Workers are omp RPC processes, not one-shot /invoke",
    body: "omp --mode rpc over stdio gives prompt/steer/abort, tool events, agent_settled completion, session file on disk, --resume reattach — all verified by probes. runtime: adk|pi resolves to runtime: harness; a HarnessDriver per CLI (herdr's abstraction) builds the command. ADK is a framework for writing agents — it doesn't drive harnesses.",
    verdict: "drop ADK"
  },
  {
    id: "D3", title: "Durable state + re-adoption on restart",
    body: "Dolt TaskStore + event log (events, not just task rows, so broker replay survives). On boot: re-adopt unacked tasks and live workers. Attach-on-connect: sessions announce attached:true, upsert idempotent, grace-reap after 60s — pi-hub's proven pattern. Session files are append-only JSONL: crash-resumable by design.",
    verdict: "statefulness is a property, not a feature"
  },
  {
    id: "D4", title: "One supervisor model everywhere",
    body: "Bounded-backoff restart (max N, doubling, stability reset), SIGTERM → grace → SIGKILL, deadline → kill, adoption of durable state. Port pi-hub's watcher + omp_agent adapter shape.",
    verdict: "copy the proven shape"
  },
  {
    id: "D5", title: "omp extension owns UX parity with native subagents",
    body: "Widget via ctx.ui.setWidget matching the native Subagents list (name, agent type, task preview, state, elapsed; blue while active, amber when all open). Results steer back via pi.sendMessage({customType:'remote_agent_result'}, {triggerTurn:true, deliverAs:'steer'}). /agents commands for attach/steer/kill/logs. session_start reconciliation steers unacked results instead of a notify.",
    verdict: "remote agents should feel native"
  },
  {
    id: "D6", title: "Secrets via auth-broker for multi-worker hosts",
    body: "omp auth-broker + auth-gateway let workers fetch provider tokens from a vault instead of shipping raw API keys to every host. Kubernetes keeps ESO.",
    verdict: "credentials stay centralized"
  }
];

window.DATA.milestones = [
  { id: "M1", name: "Provider seam + local process", proof: "spawn a remote agent from omp; tool events stream; result steered back", detail: "Extract WorkloadLauncher from the two g.k8s call sites; implement local (spawn omp RPC worker); gateway on localhost; existing extension drives it end-to-end." },
  { id: "M2", name: "Statefulness", proof: "kill the gateway mid-task; restart; client reconnects with cursor; worker finishes; result delivered", detail: "Dolt TaskStore + event log; cursor replay; re-adoption + POST /attach; extension reconciliation steers results." },
  { id: "M3", name: "omp UI parity", proof: "widget screenshot parity with the native Subagents list", detail: "Widget rows + /agents commands + steer + kill + logs." },
  { id: "M4", name: "Docker provider", proof: "container worker survives gateway restart via --resume", detail: "Sessions volume + docker run -i driver." },
  { id: "M5", name: "Remote runner", proof: "subagent runs on another machine, steerable from local omp", detail: "agents runner daemon (same Go binary) exposing start/kill/logs; bearer auth + auth-broker integration; gateway remote provider." },
  { id: "M6", name: "k8s provider parity", proof: "cluster workers through the same WorkloadLauncher", detail: "k8s impl wrapping the existing controller; CRD regenerated; gateway sets the full spec." }
];

window.DATA.ui = {
  widget: {
    title: "Subagents widget (setWidget, aboveEditor)",
    sample: [
      "╭─ Remote Agents ─────────── 2 active · 1 open ─╮",
      "│ 02:31  Refactor auth (worker)     running     │",
      "│ 01:12  Research ADK (scout)       running     │",
      "│ 00:45  Fix flake (worker)         waiting     │",
      "╰───────────────────────────────────────────────╯"
    ],
    behavior: [
      "Rows: elapsed · name (agent type) · state — mirroring the native Subagents list",
      "Header counts active vs open; border blue while active, amber when all open",
      "Tool activity detail (bash 7m, write 3m) from worker event stream",
      "Elapsed freezes at finalizing/completed/failed"
    ]
  },
  commands: [
    { cmd: "/agents", what: "list tasks: id, state, agent, task preview" },
    { cmd: "/agents attach <id>", what: "re-stream a running or finished task into the session" },
    { cmd: "/agents steer <id> <msg>", what: "inject a mid-run message (POST /tasks/{id}/steer)" },
    { cmd: "/agents kill <id>", what: "cancel a task (provider-backed kill)" },
    { cmd: "/agents logs <id>", what: "tail the task's event log" }
  ],
  steering: [
    "completion → pi.sendMessage({customType:'remote_agent_result'}, {triggerTurn:true, deliverAs:'steer'})",
    "failure → same channel, colored box, resume hints (session file + /agents attach)",
    "session_start reconciliation → POST /attach → unacked results steered into the fresh session (no more notify-only)",
    "caller_ping-style help requests: child exits with a ping sidecar; parent resumes with guidance"
  ]
};

window.DATA.statefulness = [
  { restarts: "parent omp session", survives: "remote tasks, session file, results", how: "session_start → POST /attach → steer unacked results into the fresh session" },
  { restarts: "agent server", survives: "task rows + event log (Dolt), worker processes, broker replay", how: "re-adopt live workers; re-open brokers; cursor replay for clients" },
  { restarts: "worker process", survives: "session JSONL", how: "supervisor relaunches with --resume (verified same-file binding)" },
  { restarts: "worker host", survives: "session files (volume / daemon disk)", how: "same --resume pickup from a new host" },
  { restarts: "everything", survives: "the record", how: "task rows are the record; sessions on durable volume" }
];

window.DATA.dropped = [
  { what: "pi as a separate runtime", why: "omp already is pi (fork) + plugin host; same SDK. pi stays available as a worker CLI via a driver." },
  { what: "Google ADK", why: "framework for authoring agents, not driving harnesses. The tool loop comes free from the harness." },
  { what: "A2A SDK / ACP client in v1", why: "ACP's HTTP transport is a draft; A2A SDKs lag the spec; neither adds capability the gateway + RPC workers lack. Borrow semantics instead." },
  { what: "k8s as the only runtime", why: "demoted to one provider among four; controller/CRD stay for the cluster story." }
];
