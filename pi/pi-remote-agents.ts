// pi-remote-agents — explicit remote-agent tools for pi.
//
// Remote agents are a distinct class from local subagents: isolated pods, durable
// results, lifecycle independent of the pi session. These tools let pi (and the
// LLM) deliberately choose remote work and talk to the stateful gateway.
//
// Config (env): AGENTS_GATEWAY_URL (default http://localhost:9090),
//               AGENTS_OWNER (default $USER).
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { StringEnum } from "@earendil-works/pi-ai";
import { Type } from "typebox";
import os from "node:os";

const GATEWAY = process.env.AGENTS_GATEWAY_URL ?? "http://localhost:9090";
const OWNER = process.env.AGENTS_OWNER ?? os.userInfo().username ?? "default";

interface Task {
  id: string;
  owner: string;
  type: string;
  state: string;
  result?: string;
  artifacts?: string[];
  error?: string;
  acknowledged?: boolean;
}

async function api(path: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(`${GATEWAY}${path}`, init);
  if (!res.ok && res.headers.get("content-type")?.includes("event-stream") !== true) {
    const body = await res.text().catch(() => "");
    throw new Error(`gateway ${path} -> ${res.status}: ${body}`);
  }
  return res;
}

// streamTask consumes the task SSE stream, forwarding artifacts to onUpdate, and
// resolves with the terminal event.
async function streamTask(
  id: string,
  signal: AbortSignal | undefined,
  onText: (text: string) => void,
): Promise<{ ok: boolean; output?: string; error?: string }> {
  const res = await fetch(`${GATEWAY}/tasks/${id}/stream`, {
    headers: { Accept: "text/event-stream" },
    signal,
  });
  const reader = res.body?.getReader();
  if (!reader) throw new Error("no stream body");
  const decoder = new TextDecoder();
  let buf = "";

  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    const frames = buf.split("\n\n");
    buf = frames.pop() ?? "";
    for (const frame of frames) {
      const line = frame.split("\n").find((l) => l.startsWith("data:"));
      if (!line) continue;
      const ev = JSON.parse(line.slice(5).trim());
      if (ev.type === "artifact" && ev.artifact?.text) {
        onText(`${ev.artifact.name ?? "artifact"}: ${ev.artifact.text}`);
      } else if (ev.type === "status") {
        onText(`status: ${ev.state}`);
      } else if (ev.type === "result") {
        return { ok: true, output: ev.result?.output ?? "" };
      } else if (ev.type === "error") {
        return { ok: false, error: ev.error ?? "unknown error" };
      }
    }
  }
  return { ok: false, error: "stream ended without a terminal event" };
}

export default function (pi: ExtensionAPI) {
  pi.registerTool({
    name: "RemoteAgent",
    label: "Remote Agent",
    description:
      "Dispatch a unit of work to a remote, pod-isolated agent (distinct from local subagents). " +
      "Foreground streams the result; background returns a task id to reconnect to later. " +
      "output:pr makes the agent open a pull request on repo.",
    promptSnippet: "Run work on a remote ephemeral agent (response or open a PR)",
    parameters: Type.Object({
      type: Type.String({ description: "Agent profile, e.g. researcher or worker" }),
      prompt: Type.String({ description: "The task instruction" }),
      repo: Type.Optional(Type.String({ description: "owner/repo for pr/commit output" })),
      output: Type.Optional(StringEnum(["response", "commit", "pr"] as const)),
      background: Type.Optional(Type.Boolean({ description: "Return immediately with a task id" })),
    }),
    async execute(_id, params, signal, onUpdate) {
      const res = await api("/tasks/create", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          owner: OWNER,
          type: params.type,
          prompt: params.prompt,
          repo: params.repo,
          output: params.output,
        }),
      });
      const task = (await res.json()) as Task;

      if (params.background) {
        return {
          content: [{ type: "text", text: `Dispatched remote agent. task id: ${task.id} (background)` }],
          details: { task },
        };
      }

      const lines: string[] = [];
      const final = await streamTask(task.id, signal, (text) => {
        lines.push(text);
        onUpdate?.({ content: [{ type: "text", text }], details: { taskId: task.id } });
      });
      if (!final.ok) throw new Error(`remote agent ${task.id} failed: ${final.error}`);

      return {
        content: [{ type: "text", text: `[remote ${task.id}] ${final.output}` }],
        details: { taskId: task.id, output: final.output },
      };
    },
  });

  pi.registerTool({
    name: "RemoteAgentList",
    label: "Remote Agent List",
    description: "List this owner's remote agent tasks. Set unacknowledged to see only results not yet seen.",
    parameters: Type.Object({
      unacknowledged: Type.Optional(Type.Boolean()),
    }),
    async execute(_id, params) {
      const q = params.unacknowledged ? "&acknowledged=false" : "";
      const res = await api(`/tasks?owner=${encodeURIComponent(OWNER)}${q}`);
      const tasks = (await res.json()) as Task[];
      const text = tasks.length
        ? tasks.map((t) => `${t.id} [${t.state}] ${t.type}${t.result ? ` -> ${t.result}` : ""}`).join("\n")
        : "no tasks";
      return { content: [{ type: "text", text }], details: { tasks } };
    },
  });

  pi.registerTool({
    name: "RemoteAgentGet",
    label: "Remote Agent Get",
    description: "Get one remote agent task by id (state, result, artifacts).",
    parameters: Type.Object({ id: Type.String() }),
    async execute(_id, params) {
      const res = await api(`/tasks/${params.id}`);
      const task = (await res.json()) as Task;
      return { content: [{ type: "text", text: JSON.stringify(task, null, 2) }], details: { task } };
    },
  });

  pi.registerTool({
    name: "RemoteAgentKill",
    label: "Remote Agent Kill",
    description: "Cancel a running remote agent task (deletes its pod).",
    parameters: Type.Object({ id: Type.String() }),
    async execute(_id, params) {
      await api(`/tasks/${params.id}/cancel`, { method: "POST" });
      return { content: [{ type: "text", text: `canceled ${params.id}` }], details: {} };
    },
  });

  pi.on("session_start", async (_event, ctx) => {
    // Reconciliation: surface results that finished while pi was away.
    try {
      const res = await api(`/tasks?owner=${encodeURIComponent(OWNER)}&acknowledged=false`);
      const tasks = (await res.json()) as Task[];
      const done = tasks.filter((t) => t.state === "succeeded" || t.state === "failed");
      if (done.length) {
        ctx.ui.notify(`${done.length} remote agent result(s) waiting (RemoteAgentList)`, "info");
      }
    } catch {
      // gateway not reachable; stay quiet.
    }
  });
}
