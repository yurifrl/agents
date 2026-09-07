// Throwaway fake omp RPC worker (CONTRACT §2 fake-omp protocol shape), used by
// server tests until packages/rpc-driver/test/fake-omp.ts lands; integration
// unifies on the shared fake. Flags:
//   --settled-delay MS        delay between agent_end and agent_settled
//   --no-settled              never emit agent_settled (installed omp builds)
//   --tool NAME               emit a tool_execution_start/end pair
//   --exit-after N            exit after N agent_settled events (alias --exit-after-settled)
//   --double-settled          emit agent_settled twice (idempotence fixture)
//   --tcp --port N --port-file PATH   serve the protocol over TCP (bridge emulation)
//   --session-dir DIR         base directory for the reported session file
//   --resume-crash            exit(1) when a second connection arrives after settling
import { renameSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";

interface FakeArgs {
  settledDelay: number;
  noSettled: boolean;
  tool: string | null;
  exitAfter: number | null;
  doubleSettled: boolean;
  tcp: boolean;
  port: number;
  portFile: string | null;
  sessionDir: string | null;
  resumeCrash: boolean;
}

function parseArgs(argv: string[]): FakeArgs {
  const args: FakeArgs = {
    settledDelay: 0,
    noSettled: false,
    tool: null,
    exitAfter: null,
    doubleSettled: false,
    tcp: false,
    port: 0,
    portFile: null,
    sessionDir: null,
    resumeCrash: false,
  };
  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i] ?? "";
    const value = argv[i + 1];
    switch (flag) {
      case "--settled-delay":
        args.settledDelay = Number(value ?? 0);
        i += 1;
        break;
      case "--tool":
        args.tool = value ?? null;
        i += 1;
        break;
      case "--exit-after":
      case "--exit-after-settled":
        args.exitAfter = Number(value ?? 0);
        i += 1;
        break;
      case "--double-settled":
        args.doubleSettled = true;
        break;
      case "--no-settled":
        args.noSettled = true;
        break;
      case "--tcp":
        args.tcp = true;
        break;
      case "--port":
        args.port = Number(value ?? 0);
        i += 1;
        break;
      case "--port-file":
        args.portFile = value ?? null;
        i += 1;
        break;
      case "--session-dir":
        args.sessionDir = value ?? null;
        i += 1;
        break;
      case "--resume-crash":
        args.resumeCrash = true;
        break;
      default:
        break; // ignore unknown flags (e.g. --model) for forward compatibility
    }
  }
  return args;
}

const args = parseArgs(process.argv.slice(2));
const sessionFile = path.join(
  args.sessionDir ?? os.tmpdir(),
  `fake-session-${process.pid}.json`,
);
const taskId = process.env.REMOTE_AGENTS_TASK_ID ?? "";

let streaming = false;
let settledCount = 0;
let connectionCount = 0;
let lastText = "";
let settleTimer: Timer | null = null;

type Sender = (line: string) => void;
const senders = new Set<Sender>();

const broadcast = (obj: unknown): void => {
  const line = JSON.stringify(obj);
  for (const sender of senders) sender(line);
};

function checkExit(): void {
  if (args.exitAfter === null || settledCount < args.exitAfter) return;
  process.exit(0);
}

function fireSettled(): void {
  if (settleTimer) {
    clearTimeout(settleTimer);
    settleTimer = null;
  }
  streaming = false;
  settledCount += 1;
  if (args.noSettled) {
    checkExit();
    return;
  }
  broadcast({ type: "agent_settled" });
  checkExit();
}

function runTurn(promptText: string): void {
  streaming = true;
  broadcast({ type: "agent_start" });
  if (args.tool) {
    broadcast({
      type: "tool_execution_start",
      toolName: args.tool,
      args: { command: "echo fake" },
    });
    broadcast({ type: "tool_execution_end", toolName: args.tool });
  }
  lastText = `FAKE_RESULT=${promptText.slice(0, 32)}`;
  broadcast({
    type: "message_end",
    message: { role: "assistant", content: [{ type: "text", text: lastText }] },
  });
  broadcast({ type: "agent_end" });
  if (args.doubleSettled) {
    fireSettled();
    settleTimer = setTimeout(fireSettled, 20);
    settleTimer.unref?.();
  } else if (args.settledDelay > 0) {
    settleTimer = setTimeout(fireSettled, args.settledDelay);
    settleTimer.unref?.();
  } else {
    fireSettled();
  }
}

interface RequestMsg {
  id?: unknown;
  type?: unknown;
  message?: unknown;
}

function handleLine(sender: Sender, raw: string): void {
  const line = raw.trim();
  if (!line) return;
  let msg: RequestMsg;
  try {
    msg = JSON.parse(line) as RequestMsg;
  } catch {
    return;
  }
  if (typeof msg.id !== "number" || typeof msg.type !== "string") return;
  const id = msg.id;
  const respond = (payload: Record<string, unknown>): void => {
    sender(JSON.stringify({ id, type: "response", success: true, ...payload }));
  };
  switch (msg.type) {
    case "prompt":
      respond({});
      runTurn(typeof msg.message === "string" ? msg.message : "");
      return;
    case "steer":
      respond({});
      broadcast({ type: "steer_received", message: msg.message });
      return;
    case "abort":
      respond({});
      fireSettled(); // contract: abort emits agent_settled immediately
      return;
    case "get_state":
      respond({
        isStreaming: streaming,
        sessionFile,
        taskId,
        state: { isStreaming: streaming, sessionFile },
      });
      return;
    case "get_last_assistant_text":
      respond({ text: lastText });
      return;
    case "set_session_name":
      respond({});
      return;
    default:
      sender(
        JSON.stringify({
          id,
          type: "response",
          success: false,
          error: `unknown request type: ${msg.type}`,
        }),
      );
      return;
  }
}

const readyLine = (): string =>
  JSON.stringify({ type: "ready", protocolVersion: 1, sessionFile });

if (!args.tcp) {
  const out: Sender = (line) => process.stdout.write(`${line}\n`);
  senders.add(out);
  out(readyLine());
  const decoder = new TextDecoder();
  let buffer = "";
  for await (const chunk of Bun.stdin.stream()) {
    buffer += decoder.decode(chunk, { stream: true });
    for (;;) {
      const idx = buffer.indexOf("\n");
      if (idx < 0) break;
      const line = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 1);
      handleLine(out, line);
    }
  }
  process.exit(0); // stdin EOF: the parent is gone
}

// TCP mode: emulates the herdr bridge — a ready frame per new connection,
// events broadcast to every connected client, requests served from any.
interface SockState {
  buffer: string;
  decoder: TextDecoder;
  sender: Sender;
}
const sockState = new WeakMap<object, SockState>();

const listener = Bun.listen({
  hostname: "127.0.0.1",
  port: args.port,
  socket: {
    open(socket) {
      connectionCount += 1;
      const state: SockState = {
        buffer: "",
        decoder: new TextDecoder(),
        sender: (line) => socket.write(`${line}\n`),
      };
      sockState.set(socket, state);
      senders.add(state.sender);
      if (args.resumeCrash && connectionCount > 1 && settledCount > 0) process.exit(1);
      state.sender(readyLine());
    },
    data(socket, chunk) {
      const state = sockState.get(socket);
      if (!state) return;
      state.buffer += state.decoder.decode(chunk, { stream: true });
      for (;;) {
        const idx = state.buffer.indexOf("\n");
        if (idx < 0) break;
        const line = state.buffer.slice(0, idx);
        state.buffer = state.buffer.slice(idx + 1);
        handleLine(state.sender, line);
      }
    },
    close(socket) {
      const state = sockState.get(socket);
      if (state) senders.delete(state.sender);
    },
    error() {},
  },
});

if (args.portFile) {
  const tmp = `${args.portFile}.tmp`;
  writeFileSync(tmp, `${listener.port}\n`);
  renameSync(tmp, args.portFile);
}
