import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { GitLabProjectBinding, VCSConnection } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

const { createProject, deleteProject, refreshProject, listGroupProjects } = vi.hoisted(() => ({
  createProject: vi.fn(),
  deleteProject: vi.fn(),
  refreshProject: vi.fn(),
  listGroupProjects: vi.fn(),
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({
  api: {
    createGitLabProject: createProject,
    deleteGitLabProject: deleteProject,
    refreshGitLabProject: refreshProject,
    listGitLabGroupProjects: listGroupProjects,
  },
}));
const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));
vi.mock("sonner", () => ({ toast }));

import { GitLabProjectRows } from "./code-gitlab";

const connection = (id: string, instanceUrl: string): VCSConnection => ({
  id,
  workspace_id: "ws-1",
  provider: "gitlab",
  instance_url: instanceUrl,
  account_login: "bot",
  webhook_url: `https://app.example.test/api/webhooks/vcs/${id}`,
  webhook_path: `/api/webhooks/vcs/${id}`,
  created_at: "",
});

const project = (
  id: string,
  connectionId: string,
  overrides: Partial<GitLabProjectBinding> = {},
): GitLabProjectBinding => ({
  id,
  workspace_id: "ws-1",
  connection_id: connectionId,
  gitlab_project_id: 42,
  path_with_namespace: "team/project",
  web_url: `https://gitlab.example.test/${id}/team/project`,
  hook_enabled: true,
  last_sync_error: null,
  last_refresh_at: null,
  last_refresh_error: null,
  created_at: "",
  ...overrides,
});

function renderRows(
  connections: VCSConnection[],
  projects: GitLabProjectBinding[],
  canManage = true,
  hasLoaded = true,
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      {connections.map((item) => (
        <GitLabProjectRows
          key={item.id}
          connection={item}
          projects={projects.filter((projectItem) => projectItem.connection_id === item.id)}
          canManage={canManage}
          hasLoaded={hasLoaded}
        />
      ))}
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  createProject.mockResolvedValue({});
  deleteProject.mockResolvedValue(undefined);
  refreshProject.mockResolvedValue({});
  listGroupProjects.mockResolvedValue({ projects: [] });
});

describe("GitLab projects within VCS connections", () => {
  it("adds a project to the selected GitLab connection", async () => {
    const first = connection("connection-a", "https://gitlab.example.test/install-a");
    const second = connection("connection-b", "https://gitlab.example.test/install-b");
    const user = userEvent.setup();
    renderRows([first, second], []);

    const firstInput = await screen.findByLabelText(
      "group/project or URL — https://gitlab.example.test/install-a",
    );
    const secondInput = screen.getByLabelText(
      "group/project or URL — https://gitlab.example.test/install-b",
    );
    await user.type(firstInput, "team/subgroup/new-project");
    await user.type(secondInput, "other/new-project");
    await user.click(
      within(firstInput.closest("form")!).getByRole("button", { name: "Add project" }),
    );
    await user.click(
      within(secondInput.closest("form")!).getByRole("button", { name: "Add project" }),
    );

    await waitFor(() => {
      expect(createProject).toHaveBeenNthCalledWith(1, "ws-1", {
        connection_id: "connection-a",
        project: "team/subgroup/new-project",
      });
      expect(createProject).toHaveBeenNthCalledWith(2, "ws-1", {
        connection_id: "connection-b",
        project: "other/new-project",
      });
    });
  });

  it("keeps identical project identities separated by connection and retains hook diagnostics", async () => {
    const first = connection("connection-a", "https://gitlab.example.test/install-a");
    const second = connection("connection-b", "https://gitlab.example.test/install-b");
    const projects = [
      project("project-a", first.id, {
        hook_enabled: false,
        last_sync_error: "Cannot create webhook",
        last_refresh_error: "Refresh permission denied",
      }),
      project("project-b", second.id, { hook_enabled: true }),
    ];
    const user = userEvent.setup();
    renderRows([first, second], projects);

    const projectLinks = await screen.findAllByRole("link", { name: "team/project" });
    expect(projectLinks).toHaveLength(2);
    const firstProjectRow = projectLinks[0]!.parentElement!.parentElement!;
    const secondProjectRow = projectLinks[1]!.parentElement!.parentElement!;
    expect(firstProjectRow).toHaveTextContent("Manual webhook setup required");
    expect(firstProjectRow).toHaveTextContent("Cannot create webhook");
    expect(firstProjectRow).toHaveTextContent("Refresh permission denied");
    expect(firstProjectRow).toHaveTextContent(
      "https://app.example.test/api/webhooks/vcs/connection-a",
    );
    expect(secondProjectRow).toHaveTextContent("Webhook registered");
    expect(projectLinks[0]!).toHaveAttribute(
      "href",
      "https://gitlab.example.test/project-a/team/project",
    );
    expect(projectLinks[1]!).toHaveAttribute(
      "href",
      "https://gitlab.example.test/project-b/team/project",
    );

    await user.click(screen.getAllByRole("button", { name: "Refresh project" })[0]!);
    await waitFor(() =>
      expect(refreshProject).toHaveBeenCalledWith("ws-1", "project-a"),
    );
    await user.click(screen.getAllByRole("button", { name: "Remove project" })[1]!);
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: "Remove project" }));
    await waitFor(() =>
      expect(deleteProject).toHaveBeenCalledWith("ws-1", "project-b"),
    );
  });

  it("keeps projects visible without exposing admin actions to members", async () => {
    const gitlab = connection("connection-a", "https://gitlab.example.test");
    renderRows([gitlab], [project("project-a", gitlab.id)], false);

    expect(await screen.findByRole("link", { name: "team/project" })).toBeInTheDocument();
    expect(
      screen.getByText(
        "Read-only view. Only workspace admins and owners can add or remove GitLab projects.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add project" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Refresh project" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove project" })).toBeNull();
  });

  it("does not show an empty state before the project list has loaded", () => {
    const gitlab = connection("connection-a", "https://gitlab.example.test");
    renderRows([gitlab], [], true, false);
    expect(screen.queryByText("No GitLab projects bound yet.")).toBeNull();
  });
  it("imports selected group projects, preserves partial failures, and reports manual hooks", async () => {
    const gitlab = connection("connection-a", "https://gitlab.example.test");
    listGroupProjects.mockResolvedValue({
      projects: [
        {
          id: 1,
          path_with_namespace: "team/already-bound",
          web_url: "https://gitlab.example.test/team/already-bound",
        },
        {
          id: 2,
          path_with_namespace: "team/manual-hook",
          web_url: "https://gitlab.example.test/team/manual-hook",
        },
        {
          id: 3,
          path_with_namespace: "team/retry-me",
          web_url: "https://gitlab.example.test/team/retry-me",
        },
      ],
    });
    createProject
      .mockResolvedValueOnce({ project: { hook_enabled: false } })
      .mockRejectedValueOnce(new Error("Permission denied"))
      .mockResolvedValue({ project: { hook_enabled: true } });
    const user = userEvent.setup();
    renderRows(
      [gitlab],
      [
        project("project-bound", gitlab.id, {
          gitlab_project_id: 1,
          path_with_namespace: "team/already-bound",
        }),
      ],
    );

    await user.click(
      screen.getByRole("button", { name: "Choose projects from a group" }),
    );
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByPlaceholderText("Group path or URL"), "team");
    await user.click(within(dialog).getByRole("button", { name: "Search" }));
    expect(
      await within(dialog).findByRole("link", { name: "team/already-bound" }),
    ).toBeInTheDocument();

    const boundProjectCheckbox = within(dialog).getByRole("checkbox", {
      name: "team/already-bound",
    });
    expect(boundProjectCheckbox).toHaveAttribute("aria-disabled", "true");
    await user.click(
      within(dialog).getByRole("button", {
        name: "Select all available projects",
      }),
    );
    await user.click(
      within(dialog).getByRole("button", { name: "Import selected (2)" }),
    );

    expect(
      await within(dialog).findByText(
        "Added, but manual webhook setup is required for:",
      ),
    ).toBeInTheDocument();
    const failures = within(dialog).getByRole("alert");
    expect(within(failures).getByText("team/retry-me")).toBeInTheDocument();
    expect(within(failures).getByText(/Permission denied/)).toBeInTheDocument();
    expect(within(dialog).getAllByText("Already added")).toHaveLength(2);
    expect(createProject).toHaveBeenNthCalledWith(1, "ws-1", {
      connection_id: gitlab.id,
      project: "team/manual-hook",
    });
    expect(createProject).toHaveBeenNthCalledWith(2, "ws-1", {
      connection_id: gitlab.id,
      project: "team/retry-me",
    });

    await user.click(
      within(dialog).getByRole("button", { name: "Import selected (1)" }),
    );
    await waitFor(() =>
      expect(within(dialog).getByRole("checkbox", { name: "team/retry-me" })).toHaveAttribute("aria-disabled", "true"),
    );
    expect(within(dialog).queryByRole("alert")).toBeNull();
    expect(
      within(dialog).getByText(
        "Added, but manual webhook setup is required for:",
      ),
    ).toBeInTheDocument();
  });

  it("does not create projects when a group selection is canceled", async () => {
    const gitlab = connection("connection-a", "https://gitlab.example.test");
    listGroupProjects.mockResolvedValue({
      projects: [
        {
          id: 42,
          path_with_namespace: "team/new-project",
          web_url: "https://gitlab.example.test/team/new-project",
        },
      ],
    });
    const user = userEvent.setup();
    renderRows([gitlab], []);

    await user.click(
      screen.getByRole("button", { name: "Choose projects from a group" }),
    );
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByPlaceholderText("Group path or URL"), "team");
    await user.click(within(dialog).getByRole("button", { name: "Search" }));
    await within(dialog).findByRole("checkbox", { name: "team/new-project" });
    await user.click(
      within(dialog).getByRole("checkbox", { name: "team/new-project" }),
    );
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(createProject).not.toHaveBeenCalled();
  });

  it("can retry a failed group lookup", async () => {
    const gitlab = connection("connection-a", "https://gitlab.example.test");
    listGroupProjects
      .mockRejectedValueOnce(new Error("Group unavailable"))
      .mockResolvedValueOnce({ projects: [] });
    const user = userEvent.setup();
    renderRows([gitlab], []);

    await user.click(
      screen.getByRole("button", { name: "Choose projects from a group" }),
    );
    const dialog = await screen.findByRole("dialog");
    await user.type(
      within(dialog).getByPlaceholderText("Group path or URL"),
      "team/subgroup",
    );
    await user.click(within(dialog).getByRole("button", { name: "Search" }));
    expect(
      await within(dialog).findByText(
        /Could not load projects from this group\./,
      ),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Retry" }));
    expect(
      await within(dialog).findByText("No projects found in this group."),
    ).toBeInTheDocument();
    expect(listGroupProjects).toHaveBeenNthCalledWith(
      1,
      "ws-1",
      gitlab.id,
      "team/subgroup",
    );
    expect(listGroupProjects).toHaveBeenNthCalledWith(
      2,
      "ws-1",
      gitlab.id,
      "team/subgroup",
    );
  });
});
