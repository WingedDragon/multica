import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../../test/i18n";

const {
  getConfig,
  createConnection,
  createProject,
  deleteProject,
  refreshProject,
} = vi.hoisted(() => ({
  getConfig: vi.fn(),
  createConnection: vi.fn(),
  createProject: vi.fn(),
  deleteProject: vi.fn(),
  refreshProject: vi.fn(),
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({
  api: {
    getGitLabConfig: getConfig,
    createGitLabConnection: createConnection,
    createGitLabProject: createProject,
    deleteGitLabProject: deleteProject,
    refreshGitLabProject: refreshProject,
  },
}));

import { GitLabProjectRows } from "./code-gitlab";

function renderRows(
  client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  }),
) {
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <GitLabProjectRows />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  getConfig.mockResolvedValue({
    configured: true,
    can_manage: true,
    base_url: "https://git.example.com",
    credential_storage_configured: true,
    connections: [],
    projects: [],
  });
  createConnection.mockResolvedValue({
    connection: {
      id: "connection-1",
      workspace_id: "ws-1",
      base_url: "https://gitlab.example.com",
      host: "gitlab.example.com",
    },
    webhook_secret: "gitlab-webhook-secret",
    webhook_url: "https://app.example.com/api/gitlab/webhooks/connection-1",
    webhook_path: "/api/gitlab/webhooks/connection-1",
  });
  createProject.mockResolvedValue({});
  deleteProject.mockResolvedValue(undefined);
  refreshProject.mockResolvedValue({});
});

describe("GitLab project settings", () => {
  it("adds a project using the deployment's existing GitLab configuration", async () => {
    const user = userEvent.setup();
    renderRows();

    await user.type(
      await screen.findByLabelText("group/project or URL"),
      "team/project",
    );
    await user.click(screen.getByRole("button", { name: "Add project" }));

    await waitFor(() =>
      expect(createProject).toHaveBeenCalledWith("ws-1", {
        project: "team/project",
      }),
    );
  });

  it("adds an instance and shows its one-time webhook without echoing the access token", async () => {
    const user = userEvent.setup();
    renderRows();

    await user.type(
      await screen.findByLabelText("GitLab instance URL"),
      "https://gitlab.example.com",
    );
    const token = screen.getByLabelText("Personal access token");
    expect(token).toHaveAttribute("type", "password");
    await user.type(token, "glpat-example-access-token");
    await user.click(screen.getByRole("button", { name: "Add GitLab instance" }));

    await waitFor(() =>
      expect(createConnection).toHaveBeenCalledWith("ws-1", {
        base_url: "https://gitlab.example.com",
        access_token: "glpat-example-access-token",
      }),
    );
    expect(await screen.findByLabelText("Webhook URL")).toHaveValue(
      "https://app.example.com/api/gitlab/webhooks/connection-1",
    );
    expect(screen.getByLabelText("Webhook secret")).toHaveValue(
      "gitlab-webhook-secret",
    );
    expect(token).toHaveValue("");
    expect(
      screen.queryByDisplayValue("glpat-example-access-token"),
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "I've saved it" }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Webhook secret")).not.toBeInTheDocument(),
    );
  });
  it("keeps the one-time webhook secret visible when config refresh fails", async () => {
    const user = userEvent.setup();
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    vi.spyOn(client, "invalidateQueries").mockRejectedValue(
      new Error("config refresh failed"),
    );
    renderRows(client);

    await user.type(
      await screen.findByLabelText("GitLab instance URL"),
      "https://gitlab.example.com",
    );
    await user.type(
      screen.getByLabelText("Personal access token"),
      "glpat-example-access-token",
    );
    await user.click(screen.getByRole("button", { name: "Add GitLab instance" }));

    expect(await screen.findByLabelText("Webhook secret")).toHaveValue(
      "gitlab-webhook-secret",
    );
  });
  it("shows the webhook URL without a secret when reconnecting to an existing instance", async () => {
    createConnection.mockResolvedValue({
      connection: {
        id: "connection-1",
        workspace_id: "ws-1",
        base_url: "https://gitlab.example.com",
        host: "gitlab.example.com",
      },
      webhook_url: "https://app.example.com/api/gitlab/webhooks/connection-1",
      webhook_path: "/api/gitlab/webhooks/connection-1",
    });
    const user = userEvent.setup();
    renderRows();

    await user.type(
      await screen.findByLabelText("GitLab instance URL"),
      "https://gitlab.example.com",
    );
    await user.type(
      screen.getByLabelText("Personal access token"),
      "glpat-example-access-token",
    );
    await user.click(screen.getByRole("button", { name: "Add GitLab instance" }));

    expect(await screen.findByLabelText("Webhook URL")).toHaveValue(
      "https://app.example.com/api/gitlab/webhooks/connection-1",
    );
    expect(screen.queryByLabelText("Webhook secret")).not.toBeInTheDocument();
    expect(
      screen.getByText(
        "This instance already exists, so its webhook secret is not shown again.",
      ),
    ).toBeInTheDocument();
  });


  it("groups identical upstream project IDs by connection and manages each binding", async () => {
    getConfig.mockResolvedValue({
      configured: false,
      can_manage: true,
      credential_storage_configured: true,
      manual_webhook_url: "https://app.example.com/api/webhooks/gitlab",
      connections: [
        {
          id: "connection-a",
          workspace_id: "ws-1",
          base_url: "https://gitlab.example.com/install-a",
          host: "gitlab.example.com",
          webhook_url:
            "https://app.example.com/api/webhooks/gitlab/connection-a",
          webhook_path: "/api/webhooks/gitlab/connection-a",
        },
        {
          id: "connection-b",
          workspace_id: "ws-1",
          base_url: "https://gitlab.example.com/install-b",
          host: "gitlab.example.com",
          webhook_url:
            "https://app.example.com/api/webhooks/gitlab/connection-b",
          webhook_path: "/api/webhooks/gitlab/connection-b",
        },
      ],
      projects: [
        {
          id: "binding-a",
          workspace_id: "ws-1",
          connection_id: "connection-a",
          gitlab_project_id: 42,
          path_with_namespace: "team/project",
          web_url: "https://gitlab-a.example.com/team/project",
          hook_enabled: false,
          created_at: "",
        },
        {
          id: "binding-b",
          workspace_id: "ws-1",
          connection_id: "connection-b",
          gitlab_project_id: 42,
          path_with_namespace: "team/project",
          web_url: "https://gitlab-b.example.com/team/project",
          hook_enabled: false,
          created_at: "",
        },
      ],
    });
    const user = userEvent.setup();
    renderRows();

    expect(await screen.findAllByRole("link", { name: "team/project" })).toHaveLength(2);
    expect(
      screen.getByText(
        "https://app.example.com/api/webhooks/gitlab/connection-a",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "https://app.example.com/api/webhooks/gitlab/connection-b",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("https://app.example.com/api/webhooks/gitlab"),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/GITLAB_WEBHOOK_SECRET/)).not.toBeInTheDocument();

    expect(screen.queryByText("Not connected")).not.toBeInTheDocument();
    const inputA = screen.getByLabelText(
      "group/project or URL — https://gitlab.example.com/install-a",
    );
    const inputB = screen.getByLabelText(
      "group/project or URL — https://gitlab.example.com/install-b",
    );
    await user.type(inputA, "team/new-a");
    await user.type(inputB, "team/new-b");
    await user.click(
      within(inputA.closest("form")!).getByRole("button", { name: "Add project" }),
    );
    await user.click(
      within(inputB.closest("form")!).getByRole("button", { name: "Add project" }),
    );

    await waitFor(() => {
      expect(createProject).toHaveBeenNthCalledWith(1, "ws-1", {
        connection_id: "connection-a",
        project: "team/new-a",
      });
      expect(createProject).toHaveBeenNthCalledWith(2, "ws-1", {
        connection_id: "connection-b",
        project: "team/new-b",
      });
    });
    await user.click(
      screen.getAllByRole("button", { name: "Refresh project" })[0]!,
    );
    await waitFor(() =>
      expect(refreshProject).toHaveBeenNthCalledWith(1, "ws-1", "binding-a"),
    );
    await waitFor(() =>
      expect(
        screen.getAllByRole("button", { name: "Refresh project" })[0],
      ).toBeEnabled(),
    );
    await user.click(
      screen.getAllByRole("button", { name: "Refresh project" })[1]!,
    );
    await waitFor(() =>
      expect(refreshProject).toHaveBeenNthCalledWith(2, "ws-1", "binding-b"),
    );
    await waitFor(() =>
      expect(
        screen.getAllByRole("button", { name: "Refresh project" })[0],
      ).toBeEnabled(),
    );


    await user.click(
      screen.getAllByRole("button", { name: "Remove project" })[0]!,
    );
    await user.click(
      within(screen.getByRole("alertdialog")).getByRole("button", {
        name: "Remove project",
      }),
    );
    await waitFor(() =>
      expect(deleteProject).toHaveBeenNthCalledWith(1, "ws-1", "binding-a"),
    );
    await waitFor(() =>
      expect(
        screen.getAllByRole("button", { name: "Remove project" })[0],
      ).toBeEnabled(),
    );

    await user.click(
      screen.getAllByRole("button", { name: "Remove project" })[1]!,
    );
    await user.click(
      within(screen.getByRole("alertdialog")).getByRole("button", {
        name: "Remove project",
      }),
    );
    await waitFor(() =>
      expect(deleteProject).toHaveBeenNthCalledWith(2, "ws-1", "binding-b"),
    );
  });
  it("does not claim stored instances are connected without credential encryption", async () => {
    getConfig.mockResolvedValue({
      configured: false,
      can_manage: true,
      credential_storage_configured: false,
      connections: [
        {
          id: "connection-1",
          workspace_id: "ws-1",
          base_url: "https://gitlab.example.com",
          host: "gitlab.example.com",
        },
      ],
      projects: [
        {
          id: "binding-1",
          workspace_id: "ws-1",
          connection_id: "connection-1",
          gitlab_project_id: 42,
          path_with_namespace: "team/project",
          web_url: "https://gitlab.example.com/team/project",
          hook_enabled: true,
          created_at: "",
        },
      ],
    });
    renderRows();

    expect(await screen.findByText("team/project")).toBeInTheDocument();
    expect(screen.getAllByText("Not connected")).toHaveLength(2);
    expect(screen.queryByRole("button", { name: "Add project" })).toBeNull();
    expect(
      screen.getByRole("button", { name: "Refresh project" }),
    ).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove project" })).toBeEnabled();
  });

  it("uses server-configured GitLab without encryption and shows legacy projects once", async () => {
    getConfig.mockResolvedValue({
      configured: true,
      can_manage: true,
      base_url: "https://gitlab.example.com",
      manual_webhook_url: "https://app.example.com/api/webhooks/gitlab",
      credential_storage_configured: false,
      connections: [
        {
          id: "env-connection",
          workspace_id: "ws-1",
          base_url: "https://gitlab.example.com",
          host: "gitlab.example.com",
          server_configured: true,
          webhook_url: "https://app.example.com/api/webhooks/gitlab",
          webhook_path: "/api/webhooks/gitlab",
        },
      ],
      projects: [
        {
          id: "legacy-binding",
          workspace_id: "ws-1",
          gitlab_project_id: 42,
          path_with_namespace: "team/legacy",
          web_url: "https://gitlab.example.com/team/legacy",
          hook_enabled: false,
          created_at: "",
        },
      ],
    });
    const user = userEvent.setup();
    renderRows();

    expect(await screen.findAllByRole("link", { name: "team/legacy" })).toHaveLength(1);
    expect(screen.getAllByText("https://gitlab.example.com")).toHaveLength(1);
    expect(screen.queryByText("Not connected")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add project" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Refresh project" })).toBeEnabled();
    expect(
      screen.getByText("https://app.example.com/api/webhooks/gitlab"),
    ).toBeInTheDocument();
    expect(screen.getByText(/GITLAB_WEBHOOK_SECRET/)).toBeInTheDocument();

    await user.type(
      screen.getByLabelText(
        "group/project or URL — https://gitlab.example.com",
      ),
      "team/new-project",
    );
    expect(screen.getByRole("button", { name: "Add project" })).toBeEnabled();
    await user.click(screen.getByRole("button", { name: "Add project" }));
    await waitFor(() =>
      expect(createProject).toHaveBeenCalledWith("ws-1", {
        connection_id: "env-connection",
        project: "team/new-project",
      }),
    );
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Refresh project" }),
      ).toBeEnabled(),
    );
    await user.click(screen.getByRole("button", { name: "Refresh project" }));
    await waitFor(() =>
      expect(refreshProject).toHaveBeenCalledWith("ws-1", "legacy-binding"),
    );
  });


  it("keeps deployment-bound projects visible and lets an admin remove them", async () => {
    getConfig.mockResolvedValue({
      configured: false,
      can_manage: true,
      credential_storage_configured: false,
      manual_webhook_url: "https://app.example.com/api/webhooks/gitlab",
      connections: [],
      projects: [
        {
          id: "p1",
          workspace_id: "ws-1",
          gitlab_project_id: 42,
          path_with_namespace: "team/project",
          web_url: "https://git.example.com/team/project",
          hook_enabled: false,
          created_at: "",
        },
      ],
    });
    const user = userEvent.setup();
    renderRows();

    expect(await screen.findByText("team/project")).toBeInTheDocument();
    expect(
      screen.getByText("https://app.example.com/api/webhooks/gitlab"),
    ).toBeInTheDocument();
    expect(screen.getByText(/GITLAB_WEBHOOK_SECRET/)).toBeInTheDocument();

    expect(
      screen.getByText("GitLab integration is not configured"),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove project" }));
    await user.click(
      within(screen.getByRole("alertdialog")).getByRole("button", {
        name: "Remove project",
      }),
    );

    await waitFor(() => expect(deleteProject).toHaveBeenCalledWith("ws-1", "p1"));
  });

  it("refreshes a project by its workspace binding id", async () => {
    getConfig.mockResolvedValue({
      configured: false,
      can_manage: true,
      credential_storage_configured: false,
      connections: [],
      projects: [
        {
          id: "p1",
          workspace_id: "ws-1",
          gitlab_project_id: 42,
          path_with_namespace: "team/project",
          web_url: "https://git.example.com/team/project",
          hook_enabled: true,
          created_at: "",
        },
      ],
    });
    const user = userEvent.setup();
    renderRows();

    await user.click(
      await screen.findByRole("button", { name: "Refresh project" }),
    );
    await waitFor(() => expect(refreshProject).toHaveBeenCalledWith("ws-1", "p1"));
  });

  it("keeps instance and project management admin-only", async () => {
    getConfig.mockResolvedValue({
      configured: false,
      can_manage: false,
      credential_storage_configured: true,
      connections: [
        {
          id: "connection-1",
          workspace_id: "ws-1",
          base_url: "https://gitlab.example.com",
          host: "gitlab.example.com",
        },
      ],
      projects: [
        {
          id: "p1",
          workspace_id: "ws-1",
          connection_id: "connection-1",
          gitlab_project_id: 42,
          path_with_namespace: "team/project",
          web_url: "https://gitlab.example.com/team/project",
          hook_enabled: true,
          created_at: "",
        },
      ],
    });
    renderRows();

    expect(await screen.findByText("team/project")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add GitLab instance" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add project" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Refresh project" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove project" })).toBeNull();
  });

  it("does not ask for a token when secure credential storage is unavailable", async () => {
    getConfig.mockResolvedValue({
      configured: false,
      can_manage: true,
      credential_storage_configured: false,
      connections: [],
      projects: [],
    });
    renderRows();

    expect(
      await screen.findByText(
        "This server is not configured to store GitLab access tokens securely. Ask an administrator to configure encryption.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Personal access token")).toBeNull();
  });
});
