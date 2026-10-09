import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, describe, expect, test } from "bun:test";
import { DEFAULT_CONFIG, readConfig } from "../src/config.ts";

const dir = mkdtempSync(join(tmpdir(), "ra-config-"));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

function cfgFile(body: string): string {
  const path = join(dir, `cfg-${Math.random().toString(36).slice(2)}.json`);
  writeFileSync(path, body);
  return path;
}

describe("readConfig", () => {
  test("missing file → defaults (enabled)", () => {
    expect(readConfig(join(dir, "absent.json"))).toEqual(DEFAULT_CONFIG);
    expect(DEFAULT_CONFIG.enabled).toBe(true);
  });

  test("enabled:false is honored", () => {
    expect(readConfig(cfgFile(`{"enabled":false}`))).toEqual({ enabled: false, intervalMs: 2_000 });
  });

  test("intervalMs override kept when positive", () => {
    expect(readConfig(cfgFile(`{"enabled":true,"intervalMs":500}`))).toEqual({
      enabled: true,
      intervalMs: 500,
    });
  });

  test("malformed JSON, non-object, and bad field types fall back per-field", () => {
    expect(readConfig(cfgFile("not json"))).toEqual(DEFAULT_CONFIG);
    expect(readConfig(cfgFile(`[1,2]`))).toEqual(DEFAULT_CONFIG);
    expect(readConfig(cfgFile(`{"enabled":"yes","intervalMs":-1}`))).toEqual(DEFAULT_CONFIG);
  });
});
