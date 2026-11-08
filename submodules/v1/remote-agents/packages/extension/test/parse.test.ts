import { expect, test } from "bun:test";
import { parseAgentsArgs } from "../src/parse.ts";

test("empty args mean list", () => {
  expect(parseAgentsArgs("")).toEqual({ verb: "list" });
  expect(parseAgentsArgs("   ")).toEqual({ verb: "list" });
});

test("connect takes no arguments", () => {
  expect(parseAgentsArgs("connect")).toEqual({ verb: "connect" });
  expect(parseAgentsArgs("  connect  ")).toEqual({ verb: "connect" });
});

test("steer keeps the multi-word message intact", () => {
  expect(parseAgentsArgs("steer t1 fix the build now")).toEqual({
    verb: "steer",
    id: "t1",
    message: "fix the build now",
  });
  expect(parseAgentsArgs("steer t1  padded   spaces ")).toEqual({
    verb: "steer",
    id: "t1",
    message: "padded   spaces",
  });
});

test("steer without a message is an error", () => {
  const parsed = parseAgentsArgs("steer t1");
  expect(parsed.verb).toBe("error");
  if (parsed.verb !== "error") throw new Error("expected error verb");
  expect(parsed.message).toContain("usage: /remote-agents steer");
});

test("kill, logs, attach take exactly an id", () => {
  expect(parseAgentsArgs("kill t9")).toEqual({ verb: "kill", id: "t9" });
  expect(parseAgentsArgs("logs t2")).toEqual({ verb: "logs", id: "t2" });
  expect(parseAgentsArgs("attach t3")).toEqual({ verb: "attach", id: "t3" });
});

test("kill/logs/attach without an id are errors", () => {
  expect(parseAgentsArgs("kill").verb).toBe("error");
  expect(parseAgentsArgs("logs").verb).toBe("error");
  expect(parseAgentsArgs("attach").verb).toBe("error");
});

test("unknown verbs are errors with usage", () => {
  const parsed = parseAgentsArgs("bogus x y");
  expect(parsed.verb).toBe("error");
  if (parsed.verb !== "error") throw new Error("expected error verb");
  expect(parsed.message).toContain('unknown command "bogus"');
});
