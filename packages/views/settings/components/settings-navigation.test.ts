// @vitest-environment node
import { describe, expect, it } from "vitest";
import { resolveSettingsLocation, settingsHref } from "./settings-navigation";

describe("settings location", () => {
  it.each([
    ["github", "integrations", null, "github"],
    ["gitlab", "integrations", null, "gitlab"],
    ["lark", "integrations", null, "lark"],
  ])("resolves the retired %s entry", (old, tab, section, integration) => {
    expect(resolveSettingsLocation(new URLSearchParams({ tab: old! }))).toEqual(
      { tab, section, integration },
    );
  });

  it("preserves unrelated query state while replacing page-specific state", () => {
    expect(
      settingsHref(
        "/acme/settings",
        new URLSearchParams("tab=preferences&section=chat&keep=1"),
        "integrations",
        { integration: "slack" },
      ),
    ).toBe("/acme/settings?tab=integrations&keep=1&integration=slack");
  });
});
