# Research: omp/pi core primitives for subagents

_2026-09-07. Compiled from `@earendil-works/pi-coding-agent@0.84.4` docs + dist typings (installed under `~/.omp/plugins/node_modules/`) and live probes of `omp` v18.1.13. omp = Bun-compiled fork of pi-coding-agent; docs apply with deltas noted._

## 1. ExtensionAPI — what an orchestrator extension can do

Loading: TypeScript via jiti, no compile step. Discovery: global `~/.omp/agent/extensions/`, project `.pi/extensions/` (trust-gated), settings `packages`/`extensions` keys, `-e ./file.ts`, `-e npm:pkg`. Factory: `export default function (pi: ExtensionAPI): void | Promise<void>`; async factories are awaited before `session_start`. Never start processes/watchers in the factory — defer to `session_start`, clean up in `session_shutdown`. `/reload`: `session_shutdown` → reload → `session_start {reason:"reload"}`; running handlers continue in the old module frame — the pi-herdr pattern of keeping a registry on `globalThis` via `Symbol.for(...)` survives reloads that way.

### registerTool
```
execute(toolCallId, params, signal: AbortSignal | undefined,
        onUpdate: AgentToolUpdateCallback | undefined,
        ctx: ExtensionContext): Promise<AgentToolResult>
```
- `onUpdate` streams partial results (wired to `tool_execution_update`).
- Errors MUST be thrown (sets `isError: true`); returned values never set it.
- `registerTool` works at load AND after startup (immediately callable, no reload).
- Built-ins accept pluggable operations: `createBashTool(cwd, {spawnHook})` can route shell execution anywhere.

### registerCommand
```
pi.registerCommand(name, { description, getArgumentCompletions?(prefix), handler(args, ctx: ExtensionCommandContext) })
```
Extension commands are checked BEFORE the `input` event and execute immediately even while streaming — ideal for `/agents` management verbs.

### ctx.ui — the widget surface
- Dialogs: `select`, `confirm`, `input`, `editor` (block; `opts.timeout` auto-resolves).
- Fire-and-forget: `notify(msg, "info"|"warning"|"error")`, `setStatus(key, text)`, **`setWidget(key, string[] | factory, {placement: "aboveEditor"|"belowEditor"})`**, `setTitle`, `setEditorText`.
- Works in TUI **and** RPC modes (`ctx.hasUI === true` in both); `custom()` is TUI-only.
- This is how both pi-herdr (`subagent-status` widget) and pi-hub (`omp-hub-stream`) render their subagent boards — the native "Subagents" widget in omp uses the same mechanism.

### Message injection (the steer-back primitive)
- `pi.sendUserMessage(content, {deliverAs?: "steer"|"followUp", expandPromptTemplates?})` — a real user message, ALWAYS triggers a turn. Required `deliverAs` while streaming.
- `pi.sendMessage({customType, content, display, details}, {triggerTurn?, deliverAs?})` — a `custom_message` that enters the LLM context; `triggerTurn: true` wakes the parent agent. pi-herdr delivers every completion as `pi.sendMessage({customType:"subagent_result", …}, {triggerTurn:true, deliverAs:"steer"})`.
- `pi.appendEntry(customType, data)` — persisted `custom` entry, NOT in LLM context; survives reload (durable state writes).

### Key events
`session_start {reason: startup|reload|new|resume|fork, previousSessionFile?}` · `session_shutdown {reason: quit|reload|new|resume|fork}` · `input {text, source: interactive|rpc|extension}` · `message_end {message}` (authoritative final) · `agent_end {messages}` (per-run; retry/compaction may follow) · **`agent_settled`** (fully settled — the reliable "work done" signal) · `tool_execution_start/update/end {toolCallId, toolName, args, partialResult?, result?, isError?}` · `tool_call` (MUTABLE input; can block) · `session_before_switch` (cancelable).

`ctx.sessionManager` (Readonly): `getSessionFile()`, `getSessionId()`, `getSessionDir()`, `getEntries()`, `getBranch()`, `getTree()`. `ctx.mode` ∈ `tui|rpc|json|print`; `ctx.shutdown()` deferred to idle boundary.

## 2. Session JSONL — result flow child→parent

- Location: `<agentDir>/sessions/--<cwd-with-/--replaced-by--->--/<ts>_<uuid>.jsonl`; omp default agentDir `~/.omp/agent`. Overrides: `--session-dir` > `PI_CODING_AGENT_SESSION_DIR` env > settings.
- v3 header: `{"type":"session","version":3,"id","timestamp","cwd","parentSession"?}` — `parentSession` is the lineage field. omp sometimes prepends a `{"type":"title","v":1,…}` line — parsers must skip unknown types.
- Entries: `{type:"message", id, parentId, timestamp, message}` (tree via `parentId`), plus `model_change`, `thinking_level_change`, `compaction`, `custom`, `custom_message`, `label`, `session_info`.
- `AssistantMessage.stopReason`: `stop | length | toolUse | error | aborted` (`pending` never persists).
- **Crash resumability (verified in dist/core/session-manager.js)**: entries before the FIRST assistant message are buffered in memory; after that every entry is `appendFileSync`'d. A child killed before its first model response leaves no file; after that, the file is always append-complete and resumable.
- Final-result extraction: walk branch leaf→root, take last `message` entry with `role:"assistant"`, join `content[].text`. SDK equivalent: `session.getLastAssistantText()`. RPC: `get_last_assistant_text`.

## 3. SDK embedding

`createAgentSession(options?)` — all optional: `cwd, agentDir, model, thinkingLevel, tools, customTools, resourceLoader, sessionManager, settingsManager…` → `{session, extensionsResult}`.
`createAgentSessionRuntime(createRuntime, {cwd, agentDir, sessionManager})` — the replacement layer (`newSession/switchSession/fork/importFromJsonl`); re-subscribe + re-bind extensions after replacement.
`SessionManager.create(cwd, dir?) | open(path) | continueRecent(cwd) | inMemory() | forkFrom(...)`.
`runRpcMode(runtime)` — runs the JSONL RPC loop on stdio.
The SDK ships inside the main npm package (`@earendil-works/pi-coding-agent`) — importable by any extension; same package tree under `~/.omp/plugins/node_modules`. In-process (SDK, type-safe) vs subprocess (`--mode rpc`, isolation, any language) is a documented tradeoff — subprocess is the right default for workers.

## 4. RPC mode — driving a headless worker

Start: `omp --mode rpc [--no-session | --session-dir <dir>]`. **omp handshake (verified live):**
```
{"type":"ready","protocolVersion":1,"supportedProtocolVersions":[1,2],"maxFrameBytes":1048576,"maxReassembledFrameBytes":67108864}
```
Then extension `extension_ui_request` frames (`setWidget`/`setStatus`) and `available_commands_update` — a driver MUST consume/ignore these. Protocol v2 negotiable via `{"type":"negotiate_protocol","protocolVersion":2}`.

Framing: strict JSONL, LF only, strip trailing `\r`; Node `readline` non-compliant (splits U+2028/9). Every command takes optional `id`, echoed on its response.

Commands (omp-verified): `prompt {message, streamingBehavior?}`, `steer`, `follow_up`, `abort`, `abort_and_prompt*`, `get_state` (isStreaming, sessionFile, sessionId, messageCount, contextUsage…), `get_messages`, `get_messages_page*`, `get_last_assistant_text`, `set_model`, `set_thinking_level`, `compact`, `set_auto_compaction`, `set_auto_retry`, `bash {command}` (streams `bash_execution_update`), `switch_session {sessionPath}`, `set_session_name`, `get_available_commands*`, `set_subagent_subscription` / `get_subagents` / `get_subagent_messages` (*omp-native subagent bus, verified: responds `{"subagents":[]}` in a fresh session), `set_host_tools`, `set_todos`, `handoff`. **Missing in omp vs upstream pi**: `get_entries`, `get_tree`, `fork`, `clone`, `clear_queue` (all "Unknown command" — read history by reading the JSONL file directly).

Events: `agent_start → (turn_start → message_start/message_update(delta)/message_end/tool_execution_* → turn_end)+ → agent_end {willRetry} → agent_settled`. **`agent_settled` is the reliable completion signal.** `message_update` is delta-only; wait for `message_end`.

Extension UI over RPC: dialogs emit `extension_ui_request {id, method, …}` and block until `extension_ui_response {id, value|confirmed|cancelled}`; `setWidget`/`notify`/`setStatus` are fire-and-forget.

## 5. Session lifecycle & reattach (verified)

- omp flags: `--resume <path|id-prefix>` (also `--session`), `--continue`, `--fork`, `--session-dir`, `--no-session`, `--profile <name>` (isolates auth+sessions+settings).
- **Verified probe**: `omp --mode rpc --resume <file>` reports `get_state.sessionFile` = the SAME path and same sessionId — a new child rebinds a dead child's session. (Caveat: resume-on-content semantics should be smoke-tested with a real model run before being relied on.)
- History without resume: read the JSONL directly (omp lacks `get_entries`).
- omp has no `--name`; use `set_session_name` RPC.

## 6. Non-interactive trust & approvals (deltas matter)

- omp: `--auto-approve`/`--approval-mode always-ask|write|yolo` (omp default **yolo**), per-tool policies honored in all modes, `--max-time 30m` hard stop, `--no-pty`. Project-trust (`--approve`, `defaultProjectTrust`) is an upstream-pi concept — omp always trusts project resources and gates only explicit extension files (`--trusted-extension`).
- Minimum unattended set: `omp --mode rpc [--session-dir X] --auto-approve --max-time 30m --no-pty` inside a container/VM (security doc: no built-in sandbox for unattended automation).
- Credentials at scale: `omp auth-broker` (credential vault, `serve/token/login/import`) + `omp auth-gateway` (forward proxy) — workers fetch tokens from a broker instead of shipping raw API keys.

## 7. Packaging & distribution

`package.json` manifest: `{"pi": {"extensions": ["./extensions"], "skills": [...], …}}`, keyword `pi-package`. Install: `pi install npm:…|git:…|/path`; user `~/.omp/plugins/` (omp's layout: private npm project + `node_modules/` per plugin + `installed_plugins.json`), project `.pi/`. Runtime deps in `dependencies`; core packages as `peerDependencies: "*"` (never bundled): `@earendil-works/pi-ai`, `@earendil-works/pi-coding-agent`, `@earendil-works/pi-tui`, typebox.

## Deltas omp vs pi (behavior-relevant)

| Area | pi (upstream) | omp |
|---|---|---|
| Handshake | none documented | `ready` frame + protocol v2 negotiation |
| RPC commands | `get_entries/get_tree/fork/clone/clear_queue` | absent; adds subagent bus, paging, host tools, `handoff` |
| `--name` | yes | no (use `set_session_name`) |
| Project trust | `--approve`, `defaultProjectTrust` | always trusts; `--trusted-extension` only |
| Session dir | `~/.pi/agent/sessions/--path--/` | `~/.omp/agent/sessions/` (mixed encodings observed) |
| agent_settled shutdown | honored | pane-embedded children ignore it → `PI_SUBAGENT_AUTO_EXIT_MODE=process` hard-exit trick |

## Open questions

1. Resume-with-content semantics (does the new child continue the same sessionId/leaf?) — needs one real-model smoke test.
2. `tool_result_end` event appears in bundled example code but is undocumented.
3. `@earendil-works/pi-coding-agent/client` exports `PiClient/RemoteSession` (open/create/submit/abort/reconnect/snapshot) — an undocumented client↔host protocol for driving pi sessions over a socket. Worth excavating: it may be exactly the "remote session" primitive this project wants.
4. omp's subagent RPC bus (`get_subagents`/`subagent_lifecycle` events): available in a plain rpc child (verified command exists) — but what populates it (native task subagents only, or any spawned child)?
