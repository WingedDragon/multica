import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../../test/i18n";

const { getConfig, createProject, deleteProject } = vi.hoisted(() => ({
  getConfig: vi.fn(), createProject: vi.fn(), deleteProject: vi.fn(),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({ api: { getGitLabConfig: getConfig, createGitLabProject: createProject, deleteGitLabProject: deleteProject } }));
import { GitLabProjectRows } from "./code-gitlab";

function renderRows() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(<QueryClientProvider client={client}><GitLabProjectRows /></QueryClientProvider>);
}

beforeEach(() => {
  vi.clearAllMocks();
  getConfig.mockResolvedValue({ configured: true, can_manage: true, base_url: "https://git.example.com", projects: [] });
});

describe("GitLab project settings", () => {
  it("adds a project when the server GitLab integration is configured", async () => {
    createProject.mockResolvedValue({});
    const user = userEvent.setup();
    renderRows();
    await user.type(await screen.findByLabelText("group/project or URL"), "team/project");
    await user.click(screen.getByRole("button", { name: "Add project" }));
    await waitFor(() => expect(createProject).toHaveBeenCalledWith("ws-1", { project: "team/project" }));
  });

  it("shows existing projects and lets an admin remove a binding", async () => {
    getConfig.mockResolvedValue({ configured: false, can_manage: true, projects: [{ id: "p1", path_with_namespace: "team/project", web_url: "https://git.example.com/team/project", hook_enabled: true }] });
    deleteProject.mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderRows();
    expect(await screen.findByText("team/project")).toBeInTheDocument();
    expect(screen.getByText("GitLab integration is not configured")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove project" }));
    await user.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Remove project" }));
    await waitFor(() => expect(deleteProject).toHaveBeenCalledWith("ws-1", "p1"));
  });

  it("keeps project management read-only for members", async () => {
    getConfig.mockResolvedValue({ configured: true, can_manage: false, projects: [{ id: "p1", path_with_namespace: "team/project", web_url: "https://git.example.com/team/project", hook_enabled: true }] });
    renderRows();
    expect(await screen.findByText("team/project")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add project" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove project" })).toBeNull();
  });
});
