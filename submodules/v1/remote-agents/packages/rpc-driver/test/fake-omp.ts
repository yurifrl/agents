#!/usr/bin/env bun
/**
 * Deterministic fake omp RPC worker — shared test fixture (CONTRACT.md §2).
 * Spawn by path: `bun packages/rpc-driver/test/fake-omp.ts [flags]`.
 *
 * Flags:
 *   --settled-delay MS      delay between agent_end and agent_settled (default 0)
 *   --tool NAME             emit one tool_execution_start/end pair for NAME
 *   --exit-after-settled N  exit(0) once the Nth agent_settled was emitted
 *   --exit-after N          alias for --exit-after-settled
 *   --resume-crash          exit(1) right after ready — but ONLY when argv also
 *                           contains --resume (compose into ompBin args so that
 *                           first spawns work and resume spawns crash)
 *
 * Ignored omp-style flags (accepted for argv realism): --mode, --session-dir,
 * --no-pty, --auto-approve, --max-time, --model (session-dir/model are also
 * echoed in get_state for test assertions).
 *
 * Wire behavior:
 *   ready frame first; then per line on stdin:
 *   prompt  -> response, agent_start, [tool_execution_start/end],
 *              message_end (assistant text "FAKE_RESULT=<first 32 chars of
 *              prompt>"), agent_end, agent_settled (after --settled-delay)
 *   get_state -> {isStreaming:false, sessionFile, messageCount, taskId?, model?}
 *   get_last_assistant_text -> {text}
 *   steer   -> {delivered:true}
 *   abort   -> response + agent_settled immediately (pending settle canceled)
 *   set_session_name -> ok; unknown methods -> success:false
 */
import { createInterface } from "node:readline";

type Frame = Record<string, unknown>;

function argValue(name: string): string | undefined {
  const argv = process.argv.slice(2);
  const idx = argv.indexOf(name);
  return idx >= 0 ? argv[idx + 1] : undefined;
}

const argv = process.argv.slice(2);
const settledDelayMs = Number(argValue("--settled-delay") ?? "0");
const toolName = argValue("--tool");
const exitAfterSettled = Number(argValue("--exit-after-settled") ?? argValue("--exit-after") ?? "0");
const resumeCrash = argv.includes("--resume-crash");
const resumed = argv.some((arg) => arg === "--resume" || arg.startsWith("--resume="));
const sessionDir = argValue("--session-dir");
const model = argValue("--model");
const taskId = process.env.REMOTE_AGENTS_TASK_ID;

const sessionFile = `${sessionDir ?? "/tmp"}/fake-omp-${process.pid}.jsonl`;
let lastText = "";
let messageCount = 0;
let settledCount = 0;
let pendingSettle: Timer | undefined;

function emit(frame: Frame): void {
  process.stdout.write(JSON.stringify(frame) + "\n");
}

function respond(id: unknown, extra: Frame = {}): void {
  emit({ id, type: "response", success: true, ...extra });
}

function emitSettled(): void {
  emit({ type: "agent_settled" });
  settledCount += 1;
  if (exitAfterSettled > 0 && settledCount >= exitAfterSettled) {
    // Give the event loop a beat to flush the settled line before exiting.
    setTimeout(() => process.exit(0), 25);
  }
}

function runTask(message: string): void {
  const result = `FAKE_RESULT=${message.slice(0, 32)}`;
  emit({ type: "agent_start" });
  if (toolName !== undefined) {
    emit({ type: "tool_execution_start", toolCallId: "call_1", toolName, args: {} });
    emit({ type: "tool_execution_end", toolCallId: "call_1", toolName, result: "ok" });
  }
  emit({
    type: "message_end",
    message: { role: "assistant", content: [{ type: "text", text: result }] },
  });
  lastText = result;
  messageCount += 1;
  emit({ type: "agent_end", willRetry: false });
  if (settledDelayMs > 0) {
    clearTimeout(pendingSettle);
    pendingSettle = setTimeout(emitSettled, settledDelayMs);
  } else {
    emitSettled();
  }
}

function handle(frame: Frame): void {
  const type = typeof frame.type === "string" ? frame.type : undefined;
  const id = frame.id;
  switch (type) {
    case "prompt": {
      const message = typeof frame.message === "string" ? frame.message : "";
      respond(id);
      runTask(message);
      return;
    }
    case "get_state": {
      respond(id, {
        isStreaming: false,
        sessionFile,
        messageCount,
        ...(taskId !== undefined ? { taskId } : {}),
        ...(model !== undefined ? { model } : {}),
      });
      return;
    }
    case "get_last_assistant_text": {
      respond(id, { text: lastText });
      return;
    }
    case "steer": {
      respond(id, { delivered: true });
      return;
    }
    case "abort": {
      clearTimeout(pendingSettle);
      respond(id);
      emitSettled();
      return;
    }
    case "set_session_name": {
      respond(id);
      return;
    }
    default: {
      respond(id, { success: false, error: `unknown method: ${String(type)}` });
    }
  }
}

emit({
  type: "ready",
  protocolVersion: 1,
  supportedProtocolVersions: [1, 2],
  sessionFile,
});

if (resumeCrash && resumed) {
  process.stderr.write("fake-omp: simulated resume crash\n");
  setTimeout(() => process.exit(1), 25);
}

const rl = createInterface({ input: process.stdin });
rl.on("line", (line) => {
  const trimmed = line.replace(/\r$/, "").trim();
  if (trimmed.length === 0) return;
  let frame: unknown;
  try {
    frame = JSON.parse(trimmed);
  } catch {
    return;
  }
  if (typeof frame !== "object" || frame === null || Array.isArray(frame)) return;
  handle(frame as Frame);
});
rl.on("close", () => process.exit(0));
