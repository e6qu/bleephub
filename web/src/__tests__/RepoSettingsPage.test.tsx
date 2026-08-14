import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { RepoSettingsPage } from "../pages/RepoSettingsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/ui/repos/admin/settings-repo/settings"]}>
        <Routes>
          <Route path="/ui/repos/:owner/:repo/settings" element={<RepoSettingsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const repo = {
  id: 1,
  name: "settings-repo",
  full_name: "admin/settings-repo",
  description: "before",
  homepage: "https://before.test",
  default_branch: "main",
  visibility: "public",
  private: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
  pushed_at: "2026-01-02T00:00:00Z",
  size: 0,
  owner: { login: "admin", type: "User" },
  license: null,
  has_issues: true,
  has_projects: false,
  has_wiki: false,
  has_pull_requests: true,
  is_template: false,
  archived: false,
  web_commit_signoff_required: false,
  allow_squash_merge: true,
  allow_merge_commit: true,
  allow_rebase_merge: true,
  allow_auto_merge: false,
  allow_update_branch: false,
  delete_branch_on_merge: false,
  use_squash_pr_title_as_default: false,
  squash_merge_commit_title: "COMMIT_OR_PR_TITLE",
  squash_merge_commit_message: "COMMIT_MESSAGES",
  merge_commit_title: "PR_TITLE",
  merge_commit_message: "PR_BODY",
  pull_request_creation_policy: "open",
};

describe("RepoSettingsPage", () => {
  it("loads repo details and renders the settings form", async () => {
    mockFetch.mockResolvedValue(jsonResponse(repo));
    renderPage();
    await waitFor(() => {
      expect(screen.getByDisplayValue("before")).toBeInTheDocument();
    });
    expect(mockFetch).toHaveBeenCalledWith(
      "/api/v3/repos/admin/settings-repo",
      expect.anything(),
    );
  });

  it("renders a left settings sub-nav with grouped sections", async () => {
    mockFetch.mockResolvedValue(jsonResponse(repo));
    renderPage();
    await waitFor(() => screen.getByDisplayValue("before"));

    const nav = screen.getByRole("navigation", { name: "Settings" });
    expect(nav).toBeInTheDocument();
    expect(screen.getByText("Access")).toBeInTheDocument();
    expect(screen.getByText("Code and automation")).toBeInTheDocument();
    expect(screen.getByText("Danger zone")).toBeInTheDocument();
    // General is the default-active item.
    expect(screen.getByRole("button", { name: "General" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("button", { name: "Collaborators" })).not.toHaveAttribute("aria-current");
  });

  it("submits PATCH /api/v3/repos/{owner}/{repo} on save", async () => {
    mockFetch
      .mockResolvedValueOnce(jsonResponse(repo)) // fetchRepoDetail
      .mockResolvedValueOnce(jsonResponse([])) // issues count
      .mockResolvedValueOnce(jsonResponse([])) // prs count
      .mockResolvedValueOnce(jsonResponse({ ...repo, description: "after" })); // PATCH
    renderPage();
    await waitFor(() => screen.getByDisplayValue("before"));

    fireEvent.change(screen.getByDisplayValue("before"), { target: { value: "after" } });
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    await waitFor(() => {
      const patchCall = mockFetch.mock.calls.find((call) => call[1]?.method === "PATCH");
      expect(patchCall).toBeDefined();
      expect(patchCall![0]).toBe("/api/v3/repos/admin/settings-repo");
      expect(patchCall![1].body).toContain("after");
    });
  });

  it("creates an autolink via POST .../autolinks from the autolinks tab", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u === "/api/v3/repos/admin/settings-repo/autolinks" && opts?.method === "POST") {
        return Promise.resolve(
          jsonResponse({ id: 1, key_prefix: "TICKET-", url_template: "https://x/<num>", is_alphanumeric: true }, 201),
        );
      }
      if (u === "/api/v3/repos/admin/settings-repo/autolinks") return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues") || u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse(repo));
    });
    renderPage();
    await waitFor(() => screen.getByDisplayValue("before"));

    fireEvent.click(screen.getByRole("button", { name: "Autolinks" }));
    fireEvent.change(await screen.findByLabelText("Reference prefix"), { target: { value: "TICKET-" } });
    fireEvent.change(screen.getByLabelText("Target URL"), { target: { value: "https://x/<num>" } });
    fireEvent.click(screen.getByRole("button", { name: "Add autolink" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0] === "/api/v3/repos/admin/settings-repo/autolinks" && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({
        key_prefix: "TICKET-",
        url_template: "https://x/<num>",
      });
    });
  });

  it("creates an environment via PUT .../environments/{name} from the Environments tab", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.includes("/environments/production") && opts?.method === "PUT") {
        return Promise.resolve(jsonResponse({ id: 1, name: "production", node_id: "e", url: "u" }, 200));
      }
      if (u.endsWith("/environments")) return Promise.resolve(jsonResponse({ environments: [] }));
      if (u.includes("/issues") || u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse(repo));
    });
    renderPage();
    await waitFor(() => screen.getByDisplayValue("before"));

    fireEvent.click(screen.getByRole("button", { name: "Environments" }));
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "production" } });
    fireEvent.click(screen.getByRole("button", { name: "Configure environment" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).includes("/environments/production") && c[1]?.method === "PUT",
      );
      expect(put).toBeDefined();
    });
  });

  it("saves an environment wait-timer protection rule via PUT", async () => {
    const envObj = {
      id: 1,
      name: "production",
      node_id: "e",
      url: "u",
      protection_rules: [{ id: 1, node_id: "r", type: "wait_timer", wait_timer: 5 }],
      deployment_branch_policy: null,
    };
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.includes("/environments/production") && opts?.method === "PUT") {
        return Promise.resolve(jsonResponse(envObj, 200));
      }
      if (u.includes("/environments/production/variables")) return Promise.resolve(jsonResponse({ variables: [] }));
      if (u.includes("/environments/production/secrets")) return Promise.resolve(jsonResponse({ secrets: [] }));
      if (u.endsWith("/environments")) return Promise.resolve(jsonResponse({ environments: [envObj] }));
      if (u.includes("/issues") || u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse(repo));
    });
    renderPage();
    await waitFor(() => screen.getByDisplayValue("before"));

    fireEvent.click(screen.getByRole("button", { name: "Environments" }));
    fireEvent.click(await screen.findByRole("button", { name: "production" }));
    const wait = await screen.findByLabelText("Wait timer for production");
    // Let the env detail (wait_timer 5) load before editing, matching real use.
    await waitFor(() => expect(wait).toHaveValue(5));
    fireEvent.change(wait, { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: "Save protection" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).includes("/environments/production") && c[1]?.method === "PUT",
      );
      expect(put).toBeDefined();
      expect(JSON.parse(String(put![1].body))).toEqual({ wait_timer: 30 });
    });
  });

  it("lists and creates a repository webhook from the Webhooks tab", async () => {
    const existing = {
      id: 7,
      name: "web",
      active: true,
      events: ["push"],
      config: { url: "https://old.example/hook", content_type: "json" },
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      url: "",
      deliveries_url: "",
      last_response: { code: null, status: "unused", message: null },
    };
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u === "/api/v3/repos/admin/settings-repo/hooks" && opts?.method === "POST") {
        return Promise.resolve(jsonResponse({ ...existing, id: 8, config: { url: "https://new.example/hook", content_type: "json" } }, 201));
      }
      if (u === "/api/v3/repos/admin/settings-repo/hooks") return Promise.resolve(jsonResponse([existing]));
      if (u.includes("/issues") || u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse(repo));
    });
    renderPage();
    await waitFor(() => screen.getByDisplayValue("before"));

    fireEvent.click(screen.getByRole("button", { name: "Webhooks" }));
    // Existing hook is listed (not a stub).
    expect(await screen.findByText(/old\.example\/hook/)).toBeInTheDocument();

    fireEvent.change(await screen.findByLabelText("Payload URL"), { target: { value: "https://new.example/hook" } });
    fireEvent.click(screen.getByRole("button", { name: "Add webhook" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0] === "/api/v3/repos/admin/settings-repo/hooks" && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      const body = JSON.parse(String(post![1].body));
      expect(body).toMatchObject({ name: "web", active: true, events: ["push"], config: { url: "https://new.example/hook", content_type: "json" } });
    });
  });

  it("deletes the repository via DELETE /repos/{owner}/{repo} from the danger zone", async () => {
    // Route by URL/method so any number of background GETs stay valid; only the
    // DELETE returns an empty 204.
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (opts?.method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
      const u = url.toString();
      if (u.includes("/issues") || u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse(repo));
    });
    renderPage();
    await waitFor(() => screen.getByDisplayValue("before"));

    // Open the Danger zone (Transfer) tab, then trigger the delete card.
    fireEvent.click(screen.getByRole("button", { name: "Transfer" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete this repository" }));
    // Confirm in the confirmAction modal (its confirm button is labelled "Delete").
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0] === "/api/v3/repos/admin/settings-repo" && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("saves Actions fork-PR approval, retention days, and create/approve-PRs from the Actions tab", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      const perm = "/api/v3/repos/admin/settings-repo/actions/permissions";
      if ((u === `${perm}/workflow` || u === `${perm}/fork-pr-contributor-approval` || u === `${perm}/artifact-and-log-retention`) && opts?.method === "PUT") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u === `${perm}/workflow`) return Promise.resolve(jsonResponse({ default_workflow_permissions: "read", can_approve_pull_request_reviews: false }));
      if (u === `${perm}/fork-pr-contributor-approval`) return Promise.resolve(jsonResponse({ approval_policy: "first_time_contributors" }));
      if (u === `${perm}/artifact-and-log-retention`) return Promise.resolve(jsonResponse({ days: 90, maximum_allowed_days: 400 }));
      if (u === perm) return Promise.resolve(jsonResponse({ enabled: true, allowed_actions: "all" }));
      if (u.includes("/issues") || u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse(repo));
    });
    renderPage();
    await waitFor(() => screen.getByDisplayValue("before"));

    fireEvent.click(screen.getByRole("button", { name: "Actions" }));

    // Allow GitHub Actions to create and approve pull requests
    const approve = await screen.findByRole("checkbox", { name: "Allow GitHub Actions to create and approve pull requests" });
    fireEvent.click(approve);
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/actions/permissions/workflow") && c[1]?.method === "PUT",
      );
      expect(put).toBeDefined();
      expect(JSON.parse(String(put![1].body)).can_approve_pull_request_reviews).toBe(true);
    });

    // Fork PR approval policy
    fireEvent.change(screen.getByLabelText("Fork pull request approval policy"), { target: { value: "all_external_contributors" } });
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/fork-pr-contributor-approval") && c[1]?.method === "PUT",
      );
      expect(put).toBeDefined();
      expect(JSON.parse(String(put![1].body))).toEqual({ approval_policy: "all_external_contributors" });
    });

    // Artifact and log retention days
    const days = await screen.findByLabelText("Artifact and log retention days");
    await waitFor(() => expect(days).toHaveValue(90));
    fireEvent.change(days, { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/artifact-and-log-retention") && c[1]?.method === "PUT",
      );
      expect(put).toBeDefined();
      expect(JSON.parse(String(put![1].body))).toEqual({ days: 30 });
    });
  });
});
