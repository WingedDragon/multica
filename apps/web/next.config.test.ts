// @vitest-environment node
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it, vi } from "vitest";
import { hasLocalMatch } from "next/dist/shared/lib/match-local-pattern";

vi.mock("dotenv", () => ({ config: vi.fn() }));
vi.mock("fumadocs-mdx/next", () => ({
  createMDX: () => (config: unknown) => config,
}));

import nextConfig from "./next.config";

const configSource = readFileSync(resolve(process.cwd(), "next.config.ts"), "utf8");

describe("Next.js build memory", () => {
  it("enables webpack memory optimizations for constrained selfhost builds", () => {
    expect(configSource).toMatch(/webpackMemoryOptimizations:\s*true/);
  });
});

describe("image optimization paths", () => {
  it.each([
    "/uploads/workspaces/workspace/attachment.png",
    "/api/attachments/attachment/download",
    "/api/attachments/attachment/signed-download?exp=1&sig=signature",
    "/images/hero.png?url=/uploads/workspaces/workspace/attachment.png",
  ])("rejects private or unexpected sources: %s", (url) => {
    expect(hasLocalMatch(nextConfig.images?.localPatterns, url)).toBe(false);
  });

  it.each(["/images/hero.png", "/usecases/team/screenshot.webp"])(
    "keeps landing images available: %s",
    (url) => {
      expect(hasLocalMatch(nextConfig.images?.localPatterns, url)).toBe(true);
    },
  );
});
