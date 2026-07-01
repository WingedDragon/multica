import { describe, it, expect, beforeEach, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import type { GitLabConfigResponse } from "@multica/core/types";
import { gitlabKeys } from "@multica/core/gitlab";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const mockGetGitLabConfig = vi.hoisted(() => vi.fn());
const mockCreateGitLabProject = vi.hoisted(() => vi.fn());
const mockDeleteGitLabProject = vi.hoisted(() => vi.fn());
const mockToastSuccess = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/api", () => ({
  api: {
    getGitLabConfig: mockGetGitLabConfig,
    createGitLabProject: mockCreateGitLabProject,
    deleteGitLabProject: mockDeleteGitLabProject,
  },
}));

vi.mock("sonner", () => ({
  toast: {
    success: mockToastSuccess,
    error: mockToastError,
  },
}));

import { GitLabTab } from "./gitlab-tab";

const TEST_RESOURCES = {
  en: { common: enCommon, settings: enSettings },
};

const baseConfig: GitLabConfigResponse = {
  configured: true,
  can_manage: true,
  base_url: "https://code.mlamp.cn",
  manual_webhook_url: "https://multica.example/api/webhooks/gitlab",
  projects: [],
};

function createQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
}

function renderGitLabTab(config: GitLabConfigResponse = baseConfig) {
  const queryClient = createQueryClient();
  const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
  mockGetGitLabConfig.mockResolvedValue(config);

  render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <GitLabTab />
      </I18nProvider>
    </QueryClientProvider>,
  );

  return { queryClient, invalidateSpy };
}

function project(overrides: Partial<GitLabConfigResponse["projects"][number]> = {}) {
  return {
    id: "binding-1",
    workspace_id: "workspace-1",
    gitlab_project_id: 101,
    path_with_namespace: "platform/api",
    web_url: "https://code.mlamp.cn/platform/api",
    hook_enabled: true,
    last_sync_error: null,
    created_at: "2026-07-01T00:00:00Z",
    ...overrides,
  };
}

describe("GitLabTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("shows the configured base URL and add form", async () => {
    renderGitLabTab();

    expect(await screen.findByText("Configured on this deployment")).toBeTruthy();
    expect(screen.getByText("https://code.mlamp.cn")).toBeTruthy();
    expect(screen.getByRole("textbox", { name: "group/project or URL" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add project" })).toBeTruthy();
  });

  it("shows manual webhook details for projects without a registered hook without exposing secrets", async () => {
    renderGitLabTab({
      ...baseConfig,
      projects: [
        project({
          hook_enabled: false,
          last_sync_error:
            'hook failed: GITLAB_WEBHOOK_SECRET=super-secret "token":"hook-secret-value" token: header-secret secret-token: raw-secret glpat-AbCdEfGhIjKlMnOpQrStUvWx',
        }),
      ],
    });

    expect(await screen.findByText("Manual webhook setup required")).toBeTruthy();
    expect(screen.getByText("https://multica.example/api/webhooks/gitlab")).toBeTruthy();
    expect(screen.getByText(/The secret is not shown here\./)).toBeTruthy();

    const renderedText = document.body.textContent ?? "";
    expect(renderedText).toContain("GITLAB_WEBHOOK_SECRET");
    expect(renderedText).not.toContain("=super-secret");
    expect(renderedText).not.toContain("hook-secret-value");
    expect(renderedText).not.toContain("header-secret");
    expect(renderedText).not.toContain("raw-secret");
    expect(renderedText).not.toContain("glpat-AbCdEfGhIjKlMnOpQrStUvWx");
  });

  it("creates a project from trimmed input, clears the field, and invalidates the config query", async () => {
    const user = userEvent.setup();
    const { invalidateSpy } = renderGitLabTab();
    mockCreateGitLabProject.mockResolvedValue({
      project: project({ id: "binding-2", path_with_namespace: "platform/web" }),
    });

    const input = await screen.findByRole("textbox", { name: "group/project or URL" });
    await user.type(input, "  https://code.mlamp.cn/platform/web  ");
    await user.click(screen.getByRole("button", { name: "Add project" }));

    await waitFor(() => {
      expect(mockCreateGitLabProject).toHaveBeenCalledWith("workspace-1", {
        project: "https://code.mlamp.cn/platform/web",
      });
    });
    expect((input as HTMLInputElement).value).toBe("");
    expect(invalidateSpy).toHaveBeenCalledWith({
      queryKey: gitlabKeys.config("workspace-1"),
    });
  });

  it("hides add and delete controls in read-only mode", async () => {
    renderGitLabTab({
      ...baseConfig,
      can_manage: false,
      projects: [project()],
    });

    expect(
      await screen.findByText(
        "Read-only view. Only workspace admins and owners can add or remove GitLab projects.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "group/project or URL" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove project" })).toBeNull();
  });

  it("confirms before deleting a project", async () => {
    const user = userEvent.setup();
    const { invalidateSpy } = renderGitLabTab({
      ...baseConfig,
      projects: [project()],
    });
    mockDeleteGitLabProject.mockResolvedValue(undefined);

    await screen.findByText("platform/api");
    await user.click(screen.getByRole("button", { name: "Remove project" }));

    expect(mockDeleteGitLabProject).not.toHaveBeenCalled();
    expect(screen.getByText("Remove GitLab project?")).toBeTruthy();

    const confirm = screen
      .getAllByRole("button", { name: "Remove project" })
      .find((button) => button.getAttribute("data-slot")?.includes("alert-dialog"));
    await user.click(confirm ?? screen.getAllByRole("button", { name: "Remove project" })[1]!);

    await waitFor(() => {
      expect(mockDeleteGitLabProject).toHaveBeenCalledWith("workspace-1", "binding-1");
    });
    expect(invalidateSpy).toHaveBeenCalledWith({
      queryKey: gitlabKeys.config("workspace-1"),
    });
    expect(mockToastSuccess).toHaveBeenCalledWith("GitLab project removed");
  });
});
