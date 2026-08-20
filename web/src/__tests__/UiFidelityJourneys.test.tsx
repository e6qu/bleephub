import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { ReactElement } from "react";

import { DependabotPage } from "../pages/DependabotPage.js";
import { BranchProtectionPage } from "../pages/BranchProtectionPage.js";
import { OrgsPage } from "../pages/OrgsPage.js";
import { projectCardContentHref } from "../pages/ProjectsClassicPage.js";
import { SecretScanningPage } from "../pages/SecretScanningPage.js";
import { SecurityAdvisoriesPage } from "../pages/SecurityAdvisoriesPage.js";

vi.mock("../components/Shell.js", () => ({
  RepoHeader: ({ owner, repo }: { owner: string; repo: string }) => (
    <div>
      {owner}/{repo}
    </div>
  ),
}));

vi.mock("../hooks/useOpenCounts.js", () => ({
  useOpenCounts: () => ({}),
}));

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderAt(routePath: string, entry: string, element: ReactElement) {
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[entry]}>
        <Routes>
          <Route path={routePath} element={element} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

describe("global creation journeys", () => {
  it("opens organization creation directly from ?new=1", async () => {
    mockFetch.mockResolvedValue(jsonResponse([]));

    renderAt("/ui/operations/orgs", "/ui/operations/orgs?new=1", <OrgsPage />);

    expect(
      await screen.findByRole("heading", { name: "Create organization" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Login")).toBeEnabled();
  });

  it("lets an administrator change the member repository-creation policy", async () => {
    const org = {
      id: 1,
      login: "acme",
      name: "Acme",
      description: "",
      billing_email: "admin@acme.test",
      members_can_create_repositories: true,
      created_at: "2026-01-01T00:00:00Z",
    };
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === "PATCH" && url.endsWith("/api/v3/orgs/acme")) {
        return Promise.resolve(jsonResponse(org));
      }
      if (url.endsWith("/api/v3/organizations?per_page=100")) {
        return Promise.resolve(jsonResponse([{ id: org.id, login: org.login }]));
      }
      if (url.endsWith("/api/v3/orgs/acme")) {
        return Promise.resolve(jsonResponse(org));
      }
      return Promise.resolve(jsonResponse([]));
    });

    renderAt("/ui/operations/orgs", "/ui/operations/orgs", <OrgsPage />);

    fireEvent.click(await screen.findByRole("button", { name: "edit" }));
    const policy = screen.getByRole("checkbox", { name: /Repository creation/ });
    expect(policy).toBeChecked();
    fireEvent.click(policy);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        ([input, init]) =>
          String(input).endsWith("/api/v3/orgs/acme") && init?.method === "PATCH",
      );
      expect(patch).toBeDefined();
      expect(JSON.parse(String(patch?.[1]?.body))).toMatchObject({
        members_can_create_repositories: false,
      });
    });
  });
});

describe("project-card routing fidelity", () => {
  it("maps GitHub API issue content URLs to the repository issue user interface", () => {
    expect(
      projectCardContentHref(
        "https://bleephub.test/api/v3/repos/oak/repo/issues/27",
      ),
    ).toBe("/ui/repos/oak/repo/issues/27");
  });
});

describe("security journey fidelity", () => {
  it("treats a normal unprotected branch as editable state and does not issue a failing delete", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/branch-protection-patterns")) {
        return Promise.resolve(jsonResponse([]));
      }
      if (url.endsWith("/branches/main/protection")) {
        return Promise.resolve(
          jsonResponse({ message: "Branch not protected" }, 404),
        );
      }
      if (url.split("?")[0]!.endsWith("/branches")) {
        return Promise.resolve(jsonResponse([{ name: "main" }]));
      }
      if (url.endsWith("/repos/admin/repo")) {
        return Promise.resolve(
          jsonResponse({ name: "repo", full_name: "admin/repo", default_branch: "main", permissions: { admin: true, push: true, pull: true } }),
        );
      }
      if (init?.method === "DELETE") {
        return Promise.resolve(jsonResponse({ message: "unexpected delete" }, 500));
      }
      return Promise.resolve(jsonResponse({}));
    });

    renderAt(
      "/ui/repos/:owner/:repo/settings/branches",
      "/ui/repos/admin/repo/settings/branches",
      <BranchProtectionPage />,
    );

    await waitFor(() => {
      expect(
        mockFetch.mock.calls.some(([input]) =>
          String(input).endsWith("/branches/main/protection"),
        ),
      ).toBe(true);
      expect(screen.queryByText(/loading protection for main/i)).not.toBeInTheDocument();
    });
    const protectedToggle = screen.getByRole("checkbox", {
      name: "Protect this branch",
    });
    expect(protectedToggle).not.toBeChecked();
    expect(screen.queryByText(/404/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Protection rules saved.")).toBeInTheDocument();
    expect(
      mockFetch.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === "DELETE"),
    ).toBe(false);
  });

  it("saves the complete branch protection journey including zero reviews and push restrictions", async () => {
    const protection = {
      required_status_checks: null,
      required_pull_request_reviews: null,
      restrictions: null,
      enforce_admins: { enabled: false },
      allow_force_pushes: { enabled: false },
      allow_deletions: { enabled: false },
      required_linear_history: { enabled: false },
      required_conversation_resolution: { enabled: false },
      block_creations: { enabled: false },
      required_signatures: { enabled: false },
    };
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/branch-protection-patterns")) {
        return Promise.resolve(jsonResponse([]));
      }
      if (url.endsWith("/restrictions/users") || url.endsWith("/restrictions/teams")) {
        return Promise.resolve(jsonResponse([]));
      }
      if (url.endsWith("/branches/main/protection")) {
        return Promise.resolve(jsonResponse(protection));
      }
      if (url.split("?")[0]!.endsWith("/branches")) {
        return Promise.resolve(jsonResponse([{ name: "main", protected: true, commit: { sha: "a".repeat(40) } }]));
      }
      if (url.endsWith("/repos/admin/repo")) {
        return Promise.resolve(jsonResponse({ name: "repo", full_name: "admin/repo", default_branch: "main", permissions: { admin: true, push: true, pull: true } }));
      }
      return Promise.resolve(jsonResponse({ method: init?.method }));
    });

    renderAt(
      "/ui/repos/:owner/:repo/settings/branches",
      "/ui/repos/admin/repo/settings/branches",
      <BranchProtectionPage />,
    );

    await waitFor(() => {
      expect(screen.getByRole("checkbox", { name: "Protect this branch" })).toBeChecked();
    });
    fireEvent.click(screen.getByRole("checkbox", { name: "Require a pull request before merging" }));
    fireEvent.change(screen.getByLabelText("Required approving reviews"), { target: { value: "0" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "Restrict who can push to this branch" }));
    fireEvent.change(screen.getByLabelText("Users (one login per line)"), { target: { value: "admin\nreviewer" } });
    fireEvent.change(screen.getByLabelText("Teams (one slug per line)"), { target: { value: "platform" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "Require signed commits" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await screen.findByText("Protection rules saved.");
    const writes = mockFetch.mock.calls.filter(([, init]) => init?.method === "PUT");
    const topLevel = writes.find(([input]) => String(input).endsWith("/branches/main/protection"));
    expect(JSON.parse(String(topLevel?.[1]?.body))).toMatchObject({
      required_pull_request_reviews: { required_approving_review_count: 0 },
      restrictions: { users: [], teams: [], apps: [] },
      enforce_admins: { enabled: false },
      required_signatures: { enabled: true },
    });
    const userWrite = writes.find(([input]) => String(input).endsWith("/restrictions/users"));
    const teamWrite = writes.find(([input]) => String(input).endsWith("/restrictions/teams"));
    expect(JSON.parse(String(userWrite?.[1]?.body))).toEqual({ users: ["admin", "reviewer"] });
    expect(JSON.parse(String(teamWrite?.[1]?.body))).toEqual({ teams: ["platform"] });
  });

  it("uses a keyboard-operable Dependabot alert control and surfaces detail failures", async () => {
    const alert = {
      number: 7,
      state: "open",
      dependency: {
        package: { ecosystem: "npm", name: "example" },
        manifest_path: "package.json",
      },
      security_advisory: {
        ghsa_id: "GHSA-dependabot",
        cve_id: null,
        summary: "Dependency problem",
        description: "Upgrade the package.",
      },
      security_vulnerability: {
        severity: "high",
        vulnerable_version_range: "< 2",
        first_patched_version: { identifier: "2.0.0" },
      },
    };
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/dependabot/alerts/7")) {
        return Promise.resolve(jsonResponse({ message: "boom" }, 500));
      }
      if (url.includes("/dependabot/alerts")) {
        return Promise.resolve(jsonResponse([alert]));
      }
      if (url.includes("/dependabot/secrets")) {
        return Promise.resolve(jsonResponse({ total_count: 0, secrets: [] }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    renderAt(
      "/ui/repos/:owner/:repo/security/dependabot",
      "/ui/repos/admin/repo/security/dependabot",
      <DependabotPage />,
    );

    const alertButton = await screen.findByRole("button", {
      name: /#7 Dependency problem/i,
    });
    expect(alertButton).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(alertButton);

    expect(
      await screen.findByText("Failed to load Dependabot alert"),
    ).toBeInTheDocument();
  });

  it("surfaces secret-scanning location failures instead of claiming there are no locations", async () => {
    const alert = {
      number: 3,
      state: "open",
      secret_type: "github_pat",
      secret_type_display_name: "GitHub personal access token",
      resolution: null,
      created_at: "2026-01-01T00:00:00Z",
    };
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/secret-scanning/alerts/3/locations")) {
        return Promise.resolve(jsonResponse({ message: "boom" }, 500));
      }
      if (url.endsWith("/secret-scanning/alerts/3")) {
        return Promise.resolve(jsonResponse(alert));
      }
      if (url.includes("/secret-scanning/alerts")) {
        return Promise.resolve(jsonResponse([alert]));
      }
      return Promise.resolve(jsonResponse({}));
    });

    renderAt(
      "/ui/repos/:owner/:repo/security/secret-scanning",
      "/ui/repos/admin/repo/security/secret-scanning",
      <SecretScanningPage />,
    );

    fireEvent.click(
      await screen.findByRole("button", {
        name: /#3 GitHub personal access token/i,
      }),
    );

    expect(
      await screen.findByText("Failed to load alert locations"),
    ).toBeInTheDocument();
    expect(screen.queryByText("No locations.")).not.toBeInTheDocument();
  });

  it("publishes an advisory through PATCH and refreshes its detail lifecycle", async () => {
    let advisory = {
      id: 1,
      ghsa_id: "GHSA-1234-5678-9012",
      cve_id: null,
      summary: "Draft vulnerability",
      description: "Private details",
      severity: "high",
      cwe_ids: ["CWE-79"],
      state: "draft",
      author: { login: "admin" },
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      published_at: null as string | null,
      url: "https://bleephub.test/api/v3/repos/admin/repo/security-advisories/GHSA-1234-5678-9012",
      html_url: "https://bleephub.test/admin/repo/security/advisories/GHSA-1234-5678-9012",
    };
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (
        url.endsWith(`/security-advisories/${advisory.ghsa_id}`) &&
        init?.method === "PATCH"
      ) {
        const body = JSON.parse(String(init.body)) as { state?: string };
        advisory = {
          ...advisory,
          state: body.state ?? advisory.state,
          published_at:
            body.state === "published"
              ? "2026-01-02T00:00:00Z"
              : advisory.published_at,
        };
        return Promise.resolve(jsonResponse(advisory));
      }
      if (url.endsWith(`/security-advisories/${advisory.ghsa_id}`)) {
        return Promise.resolve(jsonResponse(advisory));
      }
      if (url.endsWith("/security-advisories")) {
        return Promise.resolve(jsonResponse([advisory]));
      }
      return Promise.resolve(jsonResponse({}));
    });

    renderAt(
      "/ui/repos/:owner/:repo/security/advisories",
      "/ui/repos/admin/repo/security/advisories",
      <SecurityAdvisoriesPage />,
    );

    fireEvent.click(
      await screen.findByRole("button", { name: /Draft vulnerability/i }),
    );
    fireEvent.click(
      await screen.findByRole("button", { name: "Publish advisory" }),
    );

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        `/api/v3/repos/admin/repo/security-advisories/${advisory.ghsa_id}`,
        expect.objectContaining({
          method: "PATCH",
          body: JSON.stringify({ state: "published" }),
        }),
      );
    });
    expect(
      await screen.findByRole("button", { name: "Close advisory" }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Publish advisory" })).not.toBeInTheDocument();
  });
});
