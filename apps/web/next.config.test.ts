import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const configSource = readFileSync(resolve(process.cwd(), "next.config.ts"), "utf8");

describe("Next.js build memory", () => {
  it("enables webpack memory optimizations for constrained selfhost builds", () => {
    expect(configSource).toMatch(/webpackMemoryOptimizations:\s*true/);
  });
});
