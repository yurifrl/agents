# remote-agents

Spawn omp coding agents as **server-managed workers** from a parent omp session,
with a native-feeling UI and resume-after-restart.

```
omp session (extension: remote-agents)
  │ RemoteAgent tools · widget · /remote-agents commands · session_start reconcile
  ▼ HTTP + SSE (127.0.0.1:8679)
agents server (Go binary, `agents server`)
  ├─ TaskStore (durable JSON + event logs, cursor replay)
  ├─ Supervisor (spawn/steer/cancel/re-adoption)
  └─ Surfaces: process (child of server) · herdr (omp in a herdr pane,
               stdio bridged over TCP by a bun bridge — survives server restart)
        ▼ omp RPC JSONL over stdio
   worker: omp --mode rpc --session-dir … (real omp, real tools)
```

## Pieces

The server is part of the Go `agents` binary (repo root: `internal/server`,
`internal/rpc`, `internal/surface`, `internal/protocol`). This directory keeps
the TypeScript half:

| Package | What it is |
|---|---|
| `packages/protocol` | shared types + wire constants (pure) |
| `packages/rpc-driver` | omp RPC client: framing, handshake, prompt/steer/abort, settle on `agent_settled` or `agent_end` quiet window |
| `packages/extension` | the omp extension (widget, tools, `/remote-agents`, reconcile) |

`CONTRACT.md` is the architecture record: interfaces, wire formats, decisions,
deliberate v1 limits.

## Run

```bash
go build -o agents ../cmd/agents            # from repo root: go build -o agents ./cmd/agents
./agents server                              # or let the extension spawn it on demand
bun install                                  # workspace deps (extension/protocol/rpc-driver)
bash scripts/install-extension.sh            # symlink extension into ~/.omp/agent/extensions/
```

Then in any omp session:

- ask the model to `RemoteAgent` a task, or use `/remote-agents` commands;
- results are steered back into the conversation automatically;
- restart omp — `session_start` reconcile re-attaches: live tasks re-subscribe,
  finished-but-unread results are delivered once.

## Config (env)

| Var | Default | Meaning |
|---|---|---|
| `REMOTE_AGENTS_URL` | `http://127.0.0.1:8679` | server base URL |
| `REMOTE_AGENTS_PORT` | `8679` | server port |
| `REMOTE_AGENTS_STATE_DIR` | `~/.remote-agents` | state, event logs, worker dirs |
| `REMOTE_AGENTS_SURFACE` | `auto` | `auto` (herdr if available) · `process` · `herdr` |
| `REMOTE_AGENTS_OMP_BIN` | `omp` | worker binary |
| `REMOTE_AGENTS_MODEL` | (parent default) | worker model |
| `REMOTE_AGENTS_MAX_TIME_S` | `1800` | worker wall-clock limit |
| `REMOTE_AGENTS_OWNER` | `$USER` | task owner (resume key) |
| `REMOTE_AGENTS_SERVER_BIN` | `agents` on PATH | server binary the extension spawns |

## Statefulness model

- **Parent omp restarts** → extension reconcile: `POST /attach` → live tasks
  re-subscribed; completed results steered back once, then acked.
- **herdr workers** keep running through server restarts (TCP bridge owns the
  worker stdio; the server reconnects and re-adopts, finishing via
  `get_state`/`get_last_assistant_text`).
- **process workers** are children of the server: a server restart marks them
  failed with the session file path (`--resume` handle) — recovery, not pickup.
