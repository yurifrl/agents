// Pure parser for /remote-agents command arguments (contract §5).

export type AgentsCommand =
  | { verb: "connect" }
  | { verb: "list" }
  | { verb: "steer"; id: string; message: string }
  | { verb: "kill"; id: string }
  | { verb: "logs"; id: string }
  | { verb: "attach"; id: string }
  | { verb: "on" }
  | { verb: "off" }
  | { verb: "error"; message: string };

const USAGE =
  "usage: /remote-agents [connect | on | off | steer <id> <message> | kill <id> | logs <id> | attach <id>] (bare /remote-agents lists)";

export function parseAgentsArgs(args: string): AgentsCommand {
  const input = args.trim();
  if (input === "") return { verb: "list" };
  if (input === "connect") return { verb: "connect" };
  if (input === "on") return { verb: "on" };
  if (input === "off") return { verb: "off" };

  const steer = /^steer\s+(\S+)\s+(\S[\s\S]*)$/.exec(input);
  if (steer?.[1] != null && steer[2] != null) {
    return { verb: "steer", id: steer[1], message: steer[2].trim() };
  }

  const single = /^(kill|logs|attach)\s+(\S+)$/.exec(input);
  if (single?.[1] != null && single[2] != null) {
    if (single[1] === "kill") return { verb: "kill", id: single[2] };
    if (single[1] === "logs") return { verb: "logs", id: single[2] };
    return { verb: "attach", id: single[2] };
  }

  const verb = input.split(/\s+/, 1)[0] ?? input;
  if (verb === "steer") return { verb: "error", message: "usage: /remote-agents steer <id> <message>" };
  if (verb === "kill" || verb === "logs" || verb === "attach") {
    return { verb: "error", message: `usage: /remote-agents ${verb} <id>` };
  }
  return { verb: "error", message: `unknown command "${verb}" — ${USAGE}` };
}
