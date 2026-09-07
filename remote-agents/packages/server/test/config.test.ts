import { describe, expect, test } from "bun:test";
import { DEFAULT_PORT, ENV } from "@remote-agents/protocol";
import { loadConfig, resolveSurfaceKind, SERVER_VERSION, surfaceOptions } from "../src/config";

describe("config", () => {
  test("defaults per contract", () => {
    const config = loadConfig({});
    expect(config.port).toBe(DEFAULT_PORT);
    expect(config.stateDir).toContain(".remote-agents");
    expect(config.surface).toBe("auto");
    expect(config.ompBin).toBe("omp");
    expect(config.model).toBeUndefined();
    expect(config.maxTimeS).toBe(1800);
    expect(config.url).toBe(`http://127.0.0.1:${DEFAULT_PORT}`);
  });

  test("parses env overrides", () => {
    const config = loadConfig({
      [ENV.port]: "9000",
      [ENV.stateDir]: "/tmp/ra-state",
      [ENV.surface]: "herdr",
      [ENV.ompBin]: "/usr/local/bin/omp",
      [ENV.model]: "glm-5.3",
      [ENV.maxTimeS]: "60",
      [ENV.url]: "http://example:9000",
    });
    expect(config.port).toBe(9000);
    expect(config.stateDir).toBe("/tmp/ra-state");
    expect(config.surface).toBe("herdr");
    expect(config.ompBin).toBe("/usr/local/bin/omp");
    expect(config.model).toBe("glm-5.3");
    expect(config.maxTimeS).toBe(60);
    expect(config.url).toBe("http://example:9000");
  });

  test("expands ~ in stateDir", () => {
    const config = loadConfig({ [ENV.stateDir]: "~/.ra-test" });
    expect(config.stateDir.startsWith("~")).toBe(false);
    expect(config.stateDir).toContain(".ra-test");
  });

  test("empty strings fall back to defaults", () => {
    const config = loadConfig({ [ENV.port]: "", [ENV.ompBin]: "  ", [ENV.model]: "" });
    expect(config.port).toBe(DEFAULT_PORT);
    expect(config.ompBin).toBe("omp");
    expect(config.model).toBeUndefined();
  });

  test("rejects invalid values", () => {
    expect(() => loadConfig({ [ENV.port]: "not-a-port" })).toThrow(ENV.port);
    expect(() => loadConfig({ [ENV.port]: "99999" })).toThrow();
    expect(() => loadConfig({ [ENV.surface]: "bogus" })).toThrow(ENV.surface);
    expect(() => loadConfig({ [ENV.maxTimeS]: "0" })).toThrow();
    expect(() => loadConfig({ [ENV.maxTimeS]: "-5" })).toThrow();
  });

  test("resolveSurfaceKind auto-detects herdr via a which-style check", () => {
    const auto = loadConfig({ [ENV.surface]: "auto" });
    expect(resolveSurfaceKind(auto, () => "/usr/bin/herdr")).toBe("herdr");
    expect(resolveSurfaceKind(auto, () => null)).toBe("process");
    expect(resolveSurfaceKind(loadConfig({ [ENV.surface]: "process" }), () => "/x")).toBe("process");
    expect(resolveSurfaceKind(loadConfig({ [ENV.surface]: "herdr" }), () => null)).toBe("herdr");
  });

  test("surfaceOptions feeds createSurface", () => {
    const opts = surfaceOptions(loadConfig({ [ENV.stateDir]: "/tmp/ra" }));
    expect(opts.ompBin).toBe("omp");
    expect(opts.stateDir).toBe("/tmp/ra");
    expect(opts.bridgePath).toBe("/tmp/ra/bridge.mjs");
    expect(opts.herdrBin).toBe("herdr");
  });

  test("server version matches the package", () => {
    expect(SERVER_VERSION).toBe("0.1.0");
  });
});
