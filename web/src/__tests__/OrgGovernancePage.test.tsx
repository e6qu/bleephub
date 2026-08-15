import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgGovernancePage } from "../pages/OrgGovernancePage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderAt(path: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/orgs/:org/governance" element={<OrgGovernancePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const invitation = {
  id: 5,
  login: null,
  email: "new@example.com",
  role: "direct_member",
  created_at: "2026-06-01T00:00:00Z",
  failed_at: null,
  failed_reason: null,
  inviter: { id: 1, login: "admin", avatar_url: "", type: "User", site_admin: true },
  team_count: 0,
  invitation_source: "member",
};

const account = (id: number, login: string) => ({
  id,
  login,
  avatar_url: "",
  type: "User",
  site_admin: false,
});

function mockPeopleEndpoints(overrides: Record<string, () => Response> = {}) {
  mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
    const u = url.toString();
    for (const [needle, make] of Object.entries(overrides)) {
      if (u.includes(needle)) return Promise.resolve(make());
    }
    if (u.includes("/failed_invitations")) return Promise.resolve(jsonResponse([]));
    if (u.includes("/invitations") && init?.method === "DELETE") {
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (u.includes("/invitations")) return Promise.resolve(jsonResponse([invitation]));
    if (u.includes("/outside_collaborators")) {
      return Promise.resolve(jsonResponse([account(9, "freelancer")]));
    }
    if (u.includes("/blocks")) return Promise.resolve(jsonResponse([account(6, "spammer")]));
    return Promise.resolve(jsonResponse([]));
  });
}

describe("OrgGovernancePage people tab", () => {
  it("lists invitations, outside collaborators, and blocked users", async () => {
    mockPeopleEndpoints();
    renderAt("/ui/orgs/acme/governance");

    await waitFor(() => {
      expect(screen.getByText("new@example.com")).toBeInTheDocument();
    });
    expect(screen.getByText(/direct_member · invited by @admin/)).toBeInTheDocument();
    expect(screen.getByText(/no failed invitations/i)).toBeInTheDocument();
    expect(screen.getByText("@freelancer")).toBeInTheDocument();
    expect(screen.getByText("@spammer")).toBeInTheDocument();
  });

  it("surfaces an error when the invitation list fails", async () => {
    mockPeopleEndpoints({
      "/invitations": () => jsonResponse({ message: "forbidden" }, 403),
    });
    renderAt("/ui/orgs/acme/governance");
    await waitFor(() => {
      expect(screen.getAllByText(/failed to load/i).length).toBeGreaterThan(0);
    });
  });

  it("sends an invitation with the chosen role", async () => {
    mockPeopleEndpoints({});
    renderAt("/ui/orgs/acme/governance");
    await waitFor(() => {
      expect(screen.getByLabelText("Invitee email")).toBeInTheDocument();
    });
    fireEvent.change(screen.getByLabelText("Invitee email"), {
      target: { value: "hire@example.com" },
    });
    fireEvent.change(screen.getByLabelText("Invitation role"), { target: { value: "admin" } });
    fireEvent.click(screen.getByRole("button", { name: "Invite" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/api/v3/orgs/acme/invitations") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toEqual({
        email: "hire@example.com",
        role: "admin",
      });
    });
  });
});

describe("OrgGovernancePage roles tab", () => {
  it("lists predefined roles and loads assignments on expand", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/organization-roles/143/teams")) {
        return Promise.resolve(
          jsonResponse([{ id: 3, slug: "sec-team", name: "Sec team", description: null, assignment: "direct" }]),
        );
      }
      if (u.includes("/organization-roles/143/users")) {
        return Promise.resolve(jsonResponse([]));
      }
      if (u.includes("/organization-roles")) {
        return Promise.resolve(
          jsonResponse({
            total_count: 1,
            roles: [
              {
                id: 143,
                name: "security_manager",
                description: "Manages security.",
                base_role: "read",
                source: "Predefined",
                permissions: ["manage_security_products"],
                created_at: "2026-01-01T00:00:00Z",
                updated_at: "2026-01-01T00:00:00Z",
              },
            ],
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/governance");

    fireEvent.click(screen.getByRole("tab", { name: "Roles" }));
    await waitFor(() => {
      expect(screen.getByText("security_manager")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText("security_manager"));
    await waitFor(() => {
      expect(screen.getByText("@sec-team")).toBeInTheDocument();
    });
    expect(screen.getByText(/no users assigned/i)).toBeInTheDocument();
  });
});

const envProperty = {
  property_name: "env",
  value_type: "single_select",
  required: true,
  default_value: "dev",
  description: "Deployment environment",
  allowed_values: ["dev", "prod"],
  values_editable_by: "org_actors",
  require_explicit_values: false,
};

describe("OrgGovernancePage custom properties tab", () => {
  it("lists the schema and deletes a property", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/properties/schema/env") && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("/properties/schema")) {
        return Promise.resolve(jsonResponse([envProperty]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/governance");

    fireEvent.click(screen.getByRole("tab", { name: "Custom properties" }));
    await waitFor(() => {
      expect(
        screen.getByText(/single_select · required · default: dev · \[dev, prod\]/),
      ).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: "delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/properties/schema/env") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("edits required and default_value through the schema PATCH", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/properties/schema") && init?.method === "PATCH") {
        return Promise.resolve(jsonResponse([{ ...envProperty, default_value: "prod" }]));
      }
      if (u.includes("/properties/schema")) {
        return Promise.resolve(jsonResponse([envProperty]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/governance");

    fireEvent.click(screen.getByRole("tab", { name: "Custom properties" }));
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "edit" })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: "edit" }));

    const requiredBox = screen.getByLabelText(/repositories without an explicit value/i);
    expect(requiredBox).toBeChecked();
    fireEvent.change(screen.getByLabelText("Default value"), { target: { value: "prod" } });
    fireEvent.click(screen.getByRole("button", { name: /save property/i }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/properties/schema") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeTruthy();
      expect(JSON.parse(String(patch![1]!.body))).toEqual({
        properties: [
          {
            property_name: "env",
            value_type: "single_select",
            required: true,
            default_value: "prod",
            description: "Deployment environment",
            allowed_values: ["dev", "prod"],
          },
        ],
      });
    });
  });

  it("lists repository values and sets a value on selected repositories", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/properties/values") && init?.method === "PATCH") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("/properties/values")) {
        return Promise.resolve(
          jsonResponse([
            {
              repository_id: 7,
              repository_name: "api",
              repository_full_name: "acme/api",
              properties: [{ property_name: "env", value: "dev" }],
            },
          ]),
        );
      }
      if (u.includes("/properties/schema")) {
        return Promise.resolve(jsonResponse([envProperty]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/governance");

    fireEvent.click(screen.getByRole("tab", { name: "Custom properties" }));
    await waitFor(() => {
      expect(screen.getByText("acme/api")).toBeInTheDocument();
    });
    expect(screen.getByText("env=dev")).toBeInTheDocument();

    fireEvent.click(screen.getByLabelText("Select acme/api"));
    fireEvent.change(screen.getByLabelText("Property value"), { target: { value: "prod" } });
    fireEvent.click(screen.getByRole("button", { name: /set on selected/i }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/properties/values") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeTruthy();
      expect(JSON.parse(String(patch![1]!.body))).toEqual({
        repository_names: ["api"],
        properties: [{ property_name: "env", value: "prod" }],
      });
    });
  });
});

describe("OrgGovernancePage issue types tab", () => {
  it("lists issue types and creates a new one", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/issue-types") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({
            id: 2,
            node_id: "IT_2",
            name: "Epic",
            description: null,
            color: "purple",
            is_enabled: true,
            created_at: "2026-06-01T00:00:00Z",
            updated_at: "2026-06-01T00:00:00Z",
          }),
        );
      }
      if (u.includes("/issue-types")) {
        return Promise.resolve(
          jsonResponse([
            {
              id: 1,
              node_id: "IT_1",
              name: "Bug",
              description: "Something broken",
              color: "red",
              is_enabled: true,
              created_at: "2026-06-01T00:00:00Z",
              updated_at: "2026-06-01T00:00:00Z",
            },
          ]),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/governance");

    fireEvent.click(screen.getByRole("tab", { name: "Issue types" }));
    await waitFor(() => {
      expect(screen.getByText("Bug")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByLabelText("Issue type name"), { target: { value: "Epic" } });
    fireEvent.change(screen.getByLabelText("Issue type color"), { target: { value: "purple" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/api/v3/orgs/acme/issue-types") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toMatchObject({
        name: "Epic",
        is_enabled: true,
        color: "purple",
      });
    });
  });

  describe("Code security", () => {
    it("creates a code security configuration with feature toggles", async () => {
      mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const base = "/api/v3/orgs/acme/code-security/configurations";
        if (url === base && init?.method === "POST") {
          return Promise.resolve(jsonResponse({ id: 1, name: "Baseline", description: "d" }, 201));
        }
        if (url === base) return Promise.resolve(jsonResponse([]));
        return Promise.resolve(jsonResponse({}));
      });
      renderAt("/ui/orgs/acme/governance?tab=code-security");
      fireEvent.click(await screen.findByRole("button", { name: "New configuration" }));
      fireEvent.change(screen.getByLabelText("Configuration name"), { target: { value: "Baseline" } });
      fireEvent.change(screen.getByLabelText("Configuration description"), { target: { value: "Org baseline" } });
      fireEvent.change(screen.getByLabelText("Secret scanning"), { target: { value: "enabled" } });
      fireEvent.click(screen.getByRole("button", { name: "Create configuration" }));

      await waitFor(() => {
        const post = mockFetch.mock.calls.find(([u, i]) => u === "/api/v3/orgs/acme/code-security/configurations" && i?.method === "POST");
        expect(post).toBeTruthy();
        expect(JSON.parse(String(post![1]!.body))).toMatchObject({
          name: "Baseline",
          description: "Org baseline",
          enforcement: "enforced",
          secret_scanning: "enabled",
        });
      });
    });
  });

  describe("Secrets and variables", () => {
    it("lists org Actions secrets and variables from the org endpoints", async () => {
      mockFetch.mockImplementation((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.startsWith("/api/v3/orgs/acme/actions/secrets")) {
          return Promise.resolve(jsonResponse({ total_count: 1, secrets: [{ name: "ORG_TOKEN", visibility: "all", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-02T00:00:00Z" }] }));
        }
        if (url.startsWith("/api/v3/orgs/acme/actions/variables")) {
          return Promise.resolve(jsonResponse({ total_count: 1, variables: [{ name: "ORG_VAR", value: "prod", visibility: "all", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-02T00:00:00Z" }] }));
        }
        return Promise.resolve(jsonResponse({}));
      });
      renderAt("/ui/orgs/acme/governance?tab=secrets");
      expect(await screen.findByText("ORG_TOKEN")).toBeInTheDocument();
      expect(await screen.findByText("ORG_VAR")).toBeInTheDocument();
    });
  });

  describe("Actions", () => {
    it("saves org Actions policy and workflow permissions", async () => {
      mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const base = "/api/v3/orgs/acme/actions/permissions";
        if ((url === base || url === `${base}/workflow`) && init?.method === "PUT") return Promise.resolve(jsonResponse({}, 204));
        if (url === base) return Promise.resolve(jsonResponse({ enabled_repositories: "all", allowed_actions: "all" }));
        if (url === `${base}/workflow`) return Promise.resolve(jsonResponse({ default_workflow_permissions: "read", can_approve_pull_request_reviews: false }));
        return Promise.resolve(jsonResponse({}));
      });
      renderAt("/ui/orgs/acme/governance?tab=actions");

      const selected = await screen.findByLabelText("Selected repositories");
      fireEvent.click(selected);
      await waitFor(() => {
        const put = mockFetch.mock.calls.find(([u, i]) => u === "/api/v3/orgs/acme/actions/permissions" && i?.method === "PUT");
        expect(put).toBeTruthy();
        expect(JSON.parse(String(put![1]!.body))).toEqual({ enabled_repositories: "selected" });
      });

      fireEvent.click(screen.getByLabelText("Allow GitHub Actions to create and approve pull requests"));
      await waitFor(() => {
        const put = mockFetch.mock.calls.find(([u, i]) => u === "/api/v3/orgs/acme/actions/permissions/workflow" && i?.method === "PUT");
        expect(put).toBeTruthy();
        expect(JSON.parse(String(put![1]!.body))).toMatchObject({ default_workflow_permissions: "read", can_approve_pull_request_reviews: true });
      });
    });
  });

  describe("Member privileges", () => {
    it("loads and saves base permission + member repo creation + commit signoff", async () => {
      mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/api/v3/orgs/acme" && init?.method === "PATCH") return Promise.resolve(jsonResponse({}, 200));
        if (url === "/api/v3/orgs/acme") {
          return Promise.resolve(jsonResponse({
            default_repository_permission: "read",
            members_can_create_repositories: true,
            web_commit_signoff_required: false,
          }));
        }
        return Promise.resolve(jsonResponse({}));
      });
      renderAt("/ui/orgs/acme/governance?tab=member-privileges");

      const base = await screen.findByLabelText("Base permissions");
      await waitFor(() => expect((base as HTMLSelectElement).value).toBe("read"));
      fireEvent.change(base, { target: { value: "write" } });
      fireEvent.click(screen.getByLabelText("Require contributors to sign off on web-based commits"));
      fireEvent.click(screen.getByRole("button", { name: "Save" }));

      await waitFor(() => {
        const patch = mockFetch.mock.calls.find(([u, i]) => u === "/api/v3/orgs/acme" && i?.method === "PATCH");
        expect(patch).toBeTruthy();
        expect(JSON.parse(String(patch![1]!.body))).toMatchObject({
          default_repository_permission: "write",
          members_can_create_repositories: true,
          web_commit_signoff_required: true,
        });
      });
    });
  });
});
