# Research: driving omp headless (workers, containers, reattach)

_2026-09-07. Compiled from installed docs + live probes (no model calls). Companion to 2026-09-07-omp-pi-core.md; here: the operational details for running omp as a worker._

## 1. The RPC driver loop (worker contract)

Startup stdout: `{"type":"ready","protocolVersion":1,"supportedProtocolVersions":[1,2],…}` then extension UI frames and `available_commands_update` — a driver consumes/ignores non-response frames.

Minimal driver loop:
1. spawn `omp --mode rpc --session-dir <dir>` (or `--resume <file>`).
2. write `{"id":"1","type":"prompt","message":"<task>"}\n`.
3. read lines; correlate `{"id":"1","type":"response",…}` (acceptance) and consume events until **`agent_settled`** (the reliable completion signal; `agent_end` may be followed by auto-retry/compaction).
4. final text: last `message_end` with `role:"assistant"` → join `content[].text`; or `{"type":"get_last_assistant_text"}`.
5. steer mid-run: `{"type":"steer","message":"…"}`; cancel turn: `{"type":"abort"}`.
6. liveness/state: `{"type":"get_state"}` → `isStreaming`, `sessionFile`, `messageCount`, `contextUsage`.

Direct bash (handy for probes/setup): `{"type":"bash","command":"…"}` → streams `bash_execution_update`, response `{exitCode, output, …}`; output enters LLM context on the NEXT prompt.

omp extras: `get_messages_page` (cursor paging), `get_subagents`/`subagent_lifecycle` events (native subagent bus), `set_host_tools` (host-tool bridge), `handoff`. Missing vs upstream pi: `get_entries/get_tree/fork/clone/clear_queue`.

## 2. Session identity & reattach (verified)

- `get_state.sessionFile` returns the absolute session path — the reattach handle.
- `omp --mode rpc --resume <path>` → new child binds the SAME session file + sessionId (verified by probe). Sessions are append-only JSONL trees; a killed child leaves a valid file (entries flush after the first assistant message — a child killed before that leaves nothing).
- Resume-with-content continuation still needs one real-model smoke test (probe with an empty session wrote a fresh header).
- `--profile <name>` isolates auth+sessions+settings per worker — clean multi-worker isolation on shared hosts (`~/.omp/profiles/<name>`).
- History without resume: read the JSONL directly.

## 3. Unattended flags

`--auto-approve` (omp default approval mode is already `yolo`) · `--approval-mode write` for read+workspace-write only · per-tool `tools.<tool>: allow|prompt|deny` policies honored in every mode · `--max-time 30m` hard stop · `--no-pty` · `--tools`/`--no-tools` allowlists · `--offline`. Project-trust is upstream-pi only; omp always trusts project resources and gates explicit extension files via `--trusted-extension`.

Suggested worker baseline: `omp --mode rpc --session-dir <dir> --max-time <budget> --no-pty` (+ `--approval-mode` per trust level, inside a container for isolation) — plus `--no-extensions`/`--no-skills` if the worker shouldn't load interactive-user machinery.

Credentials: `omp auth-broker serve` + `auth-gateway` → workers fetch provider tokens from the vault; avoids shipping `ANTHROPIC_API_KEY` to every worker host.

## 4. ACP mode

`omp acp` = Zed Agent Client Protocol over stdio (`initialize`, `session/new|load|list|resume|fork|close`, `session/prompt`, `session/cancel`, `session/update`, `request_permission`, client-side `fs/*`, `terminal/create`). stdio-only. Prefer ACP when embedding in Zed-style clients; prefer raw RPC for the richer worker protocol (bash, paging, subagent bus, abort semantics). ACP's `session/load`/`session/resume` are natively the reattach primitives.

## 5. Docker

Official pattern (containerization.md): node:24-slim + global npm install of the harness + `WORKDIR /workspace` + `ENTRYPOINT ["pi"]`; run with `-v "$PWD:/workspace"` (or a checkout volume) and a **named volume for the agent dir** (`/root/.omp/agent` — mounting the host's exposes auth). Headless worker: `docker run -i --rm … omp --mode rpc --session-dir /data/sessions` with stdin/stdout piped — same JSONL protocol over the container's stdio; or publish a port and run the A2A server inside. Reattach = keep the sessions volume, re-exec with `--resume <file>` (file-based, per §2). Extensions run where the process runs (inside the container).

Alternatives: Gondolin (micro-VM tool routing, host-side auth) and OpenShell (managed sandbox gateway, `sandbox upload/download` instead of bind mounts, inference routing keeps raw keys out) — both documented upstream patterns; OpenShell's remote gateway is the closest managed analog to "agent on a server".

## 6. omp deltas vs vanilla pi (operational)

- `omp ps`: daemon-supervised background processes with `list/info/logs/stop/kill/restart`, project + global scopes — the natural home for "local provider" worker daemons, but it does NOT reattach agent sessions (that's `--resume`).
- `rpc-ui` mode: rpc engine + UI surface (drive only with the omp UI client).
- RPC protocol deltas: `ready` handshake + v2 negotiation, added commands/events (see core doc), removed upstream commands.
- `omp agents unpack` exports bundled agent definitions (`~/.omp/agent/agents` or `./.omp/agents`).

## 7. One-shot mode (`omp --mode json -p "<task>"`)

Print mode with JSON events on stdout: first line session header, then the event family (`agent_start`, `turn_start`, `message_start/update/end`, `tool_execution_*`, `turn_end`, `agent_end`) without `response` wrappers or the extension-UI sub-protocol. Final text = last assistant `message_end`. Exit codes: text mode exits 1 on `stopReason error|aborted`; in json mode parse the stream for `stopReason` rather than trusting rc [INFERENCE]. This is the cheapest "worker does a task and exits" path — no protocol state to hold, but no steering, no attach, and the process must outlive the task.

## Open questions

1. Resume-with-content: does `--resume` on a session with turns continue the same sessionId/leaf? (one smoke test)
2. Does the native subagent bus (`get_subagents`/`subagent_lifecycle`) populate for extension-spawned children or only native task subagents?
3. Protocol v2 frame envelope exact wire format (constants known: 1 MiB frame / 64 MiB reassembly).
4. json-mode exit-code semantics on provider errors.
