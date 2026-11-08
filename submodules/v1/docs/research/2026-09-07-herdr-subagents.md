# Research: pi-herdr-subagents teardown

_2026-09-07. Source: `~/DotFiles/pi-extentions/pi-herdr-subagents/`. This is the UX/architecture benchmark: async subagents with a live widget, result steering, and multi-harness support. All refs: `pi-extension/subagents/` (`idx`=index.ts, `lc`=lifecycle.ts, `cp`=completion.ts, `sd`=subagent-done.ts, `hr`=herdr.ts)._

## 1. Spawn flow end-to-end

1. `subagent()` tool call → guards (self-spawn block via `PI_SUBAGENT_AGENT`, `HERDR_ENV=1` + herdr CLI available, parent session file exists).
2. Agent defs loaded by name — discovery order: bundled `agents/` → global `~/.pi/agent/agents/` → project `.pi/agents/` (later wins). Runtime resolution precedence: tool arg → agent frontmatter → per-agent `config.json` → global default → parent.
3. **Deterministic pre-generated child session file** `<ts>_<uuid>.jsonl` in the child cwd's session dir — race-free parallel spawns. Artifact dir: `<sessionDir>/artifacts/<parentSessionId>/`.
4. Harness driver pick: `params.command` → generic template driver; else frontmatter `cli:` (default `pi`).
5. **Pane create**: `herdr tab create --workspace <ws> --label <name> --cwd <parent> --no-focus`; task metadata via `herdr pane report-metadata`.
6. Shell-ready delay (default 500 ms, `PI_SUBAGENT_SHELL_READY_DELAY_MS`) — direnv/shell-init can drop typed commands.
7. **Session seeding** (`session.ts`): `standalone` = no seed; `lineage-only` = header with `parentSession`; `fork` = header + parent lines copied up to last user message. `fork` inherits conversation context and delivers the task inline; other modes deliver the task as an `@artifact.md` file.
8. `driver.buildCommand(ctx)` → full shell line: `cd <cwd> && VAR='…' … omp/pi … '<task>'; echo '__SUBAGENT_DONE_'$?'__'`.
9. Launch script persisted to `<artifactDir>/subagent-scripts/<name>-<id>.sh` (0755, preamble comments) and run via `herdr pane run <pane>` (types text+Enter atomically).
10. Registry entry (`RunningSubagent`) kept on `globalThis[Symbol.for("pi-subagents/runtime")]` — survives `/reload`; each watcher has its own AbortController.
11. On settle → **`pi.sendMessage({customType:"subagent_result", …}, {triggerTurn:true, deliverAs:"steer"})`** — the wake-the-parent mechanism.

### Env contract passed to children

| Var | Purpose |
|---|---|
| `PI_SUBAGENT_NAME` | display name, widget label, ping identity |
| `PI_SUBAGENT_AGENT` | agent name; self-spawn guard key |
| `PI_SUBAGENT_ID` | 8-hex run id; activity files stamped with it (stale-file rejection) |
| `PI_SUBAGENT_SESSION` | child session .jsonl path; sidecar written to `<path>.exit` |
| `PI_SUBAGENT_AUTO_EXIT=1` | arms child auto-exit |
| `PI_SUBAGENT_AUTO_EXIT_MODE=process` | **omp only**: hard `process.exit(0)` at `agent_end` (omp ignores `ctx.shutdown()` at idle boundaries pane children never reach) |
| `PI_SUBAGENT_ACTIVITY_FILE` | activity snapshot JSON path (pi children) |
| `PI_SUBAGENT_SURFACE` | pane id — written, never read; reserved child-side seam |
| `PI_DENY_TOOLS` | comma list; child-side extension skips registering denied tools |
| `PI_CODING_AGENT_DIR` | child config dir isolation (role folders) |

### Completion protocol (portable core)

- **Sidecar**: child writes `<sessionFile>.exit` JSON, atomically (`.tmp` + `mv`): `{"type":"done"}` | `{"type":"error","errorMessage","stopReason"}` | `{"type":"ping","name","message"}` | wrapper-authored `{"type":"done","exitCode":N}`. Deleted after read.
- **Sentinel**: trailer `echo '__SUBAGENT_DONE_'$?'__'` matched against terminal tail → exit code. The omp driver writes the sidecar BEFORE echoing the sentinel because the pane dies instantly on exit and the 1 s terminal-tail poll loses the race — durable evidence beats racing the reaper.
- Interpretation (`completion.ts`): ping→notify; error→failure; done→success; invalid→error.

## 2. HarnessDriver abstraction

```ts
interface HarnessDriver {
  id; name;
  hasActivitySnapshots?: boolean;      // pi only
  supportsTurnInterrupt?: boolean;     // pi only
  formatModel(plan): string;
  validateRuntimePlan?(plan, parentThinking);
  buildCommand(ctx: SubagentLaunchContext): BuiltHarnessCommand; // {command, sentinelFile?, sessionFile?, launchScriptPreamble?}
  extractResult?(ctx): HarnessResult | null;
}
```
Drivers: **pi** (`pi --session <file> -e subagent-done.ts --model --thinking --tools …`), **omp** (`omp --model --thinking --append-system-prompt -e subagent-done.ts '<task>'` + env prefix + sidecar/sentinel trailer), **claude** (`--dangerously-skip-permissions` + Stop-hook plugin writing a sentinel file), **opencode/codex/grok** (one-shot run CLIs + sidecar wrapper), **generic** (`command:` template with `{agent} {model} {task} {cwd} {name} {id}` placeholders — values shell-quoted THEN substituted via replacer functions so `$`/`$&` in untrusted text stay literal).

## 3. Supervision loop

Three 1 s timers:
- **Widget**: `ctx.ui.setWidget("subagent-status", lines, {placement:"aboveEditor"})`; row = `MM:SS Name (agent) model <state+duration>`; header `N active · M open`; blue border while any active, amber when all open.
- **Status refresh**: activity file read → `observeActivity` → `projectLifecycle` → transitions; only `stalled`/`recovered` produce parent steers (suppressed for `interactive` children).
- **`watchSubagent`**: per tick, ordered — (1) sidecar `existsSync` → parse/delete; (2) sentinel file; (3) pane tail regex; (4) `herdr pane get` (present/missing/unavailable + coarse `idle|working|blocked|done`); (5) activity snapshot. Missing pane → 500 ms grace → `pane_lost` error.

**Lifecycle state machine** (`lifecycle.ts`, pure functions — fully portable):
- Process axis: `starting → running → finalizing → completed | failed` (terminal absorbing).
- Turn axis: `unknown | starting | active{source, detail} | blocked | waiting | interrupted{requestedAt}`.
- Projection labels: `starting, running, active, blocked, waiting, interrupted, stalled, finalizing, completed, failed`.
- **Watchdog**: `stalled` = pane inspection failing ≥ 60 s; recovers on next healthy observation. Time alone never stalls a healthy run.
- Interactive vs autonomous: `interactive` children (planner, /iterate) update the widget but never wake the parent.

## 4. Result extraction

Order: driver `extractResult` (claude: sentinel text; others: 200-line pane tail) → pi path: session-file `findLastAssistantMessage` (fallback to error message when `stopReason:"error"`). Failure diagnosis kinds: `startup_failure` (exit≠0, no session file → attach launch-script path), `provider_error` (sidecar error), `empty_exit`, `pane_lost`. Delivery: `subagent_result` steer with `{name, task, agent, exitCode, elapsed, sessionFile, failureKind?, terminalOutput?…}`; renderer shows a colored box with `pi --session <file>` resume hint.

## 5. Interrupt / resume / ping

- `subagent_interrupt`: Escape via `herdr pane send-keys` (pi children only — `supportsTurnInterrupt`); turn-level only; registry/lifecycle survives.
- `subagent_resume`: new pane + `pi --session <path> [-e subagent-done.ts] [@resume-message.md]`; short-circuits `already-completed` sessions (avoids doomed bare relaunch); summary extracted from NEW entries after `entryCountBefore`.
- `caller_ping` (child-side tool): writes `{"type":"ping"}` sidecar → exits → parent steered with "Sub-agent X needs help" → parent resumes the child session with guidance. Exit-and-notify is the whole trick.

## 6. Portability analysis

**Pane-coupled (thin, isolated in herdr.ts/terminal.ts)**: create/run/read/inspect/send-keys/close via herdr CLI; coarse agent status from `herdr pane get`; pane-tail summaries; Escape delivery.

**Portable as-is**: harness drivers + command templates; sidecar+sentinel completion protocol; session seeding/fork/resume; activity snapshots (atomic JSON, wrong-id rejection); lifecycle machine + 60 s stall rule; runtime routing; registry + steer delivery; launch-script artifacts; deny/spawning gating.

**What a `Surface` interface needs** (exactly the six operations supervision uses):
1. `create(name, cwd, opts) → surfaceId` (pane | process | container | SSH session)
2. `run(surfaceId, text)` — type a command (stdin write)
3. `read(surfaceId, lines) → tail` (free with stdio capture; remote needs PTY/log streaming)
4. `inspect(surfaceId) → present|missing|unavailable + agentStatus` (herdr gets this free; other surfaces synthesize it from activity snapshots)
5. `sendKeys(surfaceId, "Escape")` (turn interrupt; remote agents without turn-cancel lose this)
6. `close(surfaceId)`

**Non-negotiable invariants for remote/container**: shared filesystem for sidecar + session + activity files (else tunnel them over the channel); atomic sidecar publish before process exit; surface-assigned ids replace `HERDR_*` envs; `PI_SUBAGENT_SURFACE` is the reserved seam.

| Capability | Status | Depends on |
|---|---|---|
| Drivers, templates, flags | Portable | none |
| Sidecar + sentinel protocol | Portable | shared FS |
| Session seeding/resume/last-assistant | Portable | shared FS |
| Activity snapshots | Portable | shared FS |
| Lifecycle machine, stall watchdog | Portable | pure functions |
| Registry survival + steer delivery | Parent-pi-coupled | pi API |
| Child auto-exit | Child-pi-coupled | pi extension events |
| Pane create/run/read/inspect/keys/close | **Pane-coupled** | herdr CLI |
| Coarse agent status | **Pane-coupled** | herdr |
| Escape turn-interrupt | **Pane-coupled** | pane send-keys |

## Open questions

1. `PI_SUBAGENT_SURFACE` written but never read — intended child-side seam or dead?
2. Stall asymmetry: only `read-error` reaches `stalled`; a forever-`unknown` pane never stalls — intentional?
3. omp driver's wrapper sidecar can clobber a child-authored `error` sidecar (tmp+mv ordering).
4. No explicit kill of orphan panes on parent teardown — cleanup left to herdr defaults.
