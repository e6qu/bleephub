import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { EnterprisePage } from "../pages/EnterprisePage.js";

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

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/ui/operations/enterprise"]}>
        <EnterprisePage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const platformTeam = {
  id: 1,
  name: "Platform",
  slug: "platform",
  description: "Cross-org platform crew",
  organization_selection_type: "all",
  group_id: null,
  notification_setting: "notifications_enabled",
  created_at: "2026-05-01T00:00:00Z",
  updated_at: "2026-05-01T00:00:00Z",
};

function healthResponse(slug = "bleephub") {
  return jsonResponse({ status: "ok", service: "bleephub", enterprise_slug: slug });
}

describe("EnterprisePage teams tab", () => {
  it("uses the configured enterprise slug from Bleephub health", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u === "/internal/status") {
        return Promise.resolve(healthResponse("octo-enterprise"));
      }
      if (u.includes("/api/v3/enterprises/octo-enterprise/teams")) {
        return Promise.resolve(jsonResponse([platformTeam]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Platform")).toBeInTheDocument();
    });
    expect(
      screen.getByText('Instance-wide administration for the "octo-enterprise" enterprise.'),
    ).toBeInTheDocument();
    const listCall = mockFetch.mock.calls.find((c) =>
      c[0].toString().includes("/api/v3/enterprises/octo-enterprise/teams"),
    );
    expect(listCall).toBeTruthy();
    expect(
      mockFetch.mock.calls.some((c) => c[0].toString().includes("/api/v3/enterprises/bleephub/teams")),
    ).toBe(false);
  });

  it("lists enterprise teams via the bleephub enterprise slug", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u === "/internal/status") return Promise.resolve(healthResponse());
      if (u.includes("/api/v3/enterprises/bleephub/teams")) {
        return Promise.resolve(jsonResponse([platformTeam]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Platform")).toBeInTheDocument();
    });
    expect(screen.getByText("@platform")).toBeInTheDocument();
    expect(screen.getByText(/Cross-org platform crew · organizations: all/)).toBeInTheDocument();
    const listCall = mockFetch.mock.calls.find((c) =>
      c[0].toString().includes("/api/v3/enterprises/bleephub/teams"),
    );
    expect(listCall).toBeTruthy();
  });

  it("shows an empty state when no teams exist", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      if (url.toString() === "/internal/status") return Promise.resolve(healthResponse());
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/no enterprise teams yet/i)).toBeInTheDocument();
    });
  });

  it("creates a team through the dialog", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u === "/internal/status") return Promise.resolve(healthResponse());
      if (u.endsWith("/enterprises/bleephub/teams") && init?.method === "POST") {
        return Promise.resolve(jsonResponse(platformTeam, 201));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /new enterprise team/i })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /new enterprise team/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Platform" } });
    fireEvent.change(screen.getByLabelText("Organization selection"), { target: { value: "all" } });
    fireEvent.click(screen.getByRole("button", { name: /create team/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/enterprises/bleephub/teams") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toMatchObject({
        name: "Platform",
        organization_selection_type: "all",
      });
    });
  });

  it("manages team memberships from the members dialog", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u === "/internal/status") return Promise.resolve(healthResponse());
      if (u.includes("/teams/platform/memberships/dev1") && init?.method === "PUT") {
        return Promise.resolve(
          jsonResponse({ id: 4, login: "dev1", avatar_url: "", type: "User", site_admin: false }, 201),
        );
      }
      if (u.includes("/teams/platform/memberships")) {
        return Promise.resolve(
          jsonResponse([{ id: 3, login: "existing", avatar_url: "", type: "User", site_admin: false }]),
        );
      }
      if (u.includes("/enterprises/bleephub/teams")) {
        return Promise.resolve(jsonResponse([platformTeam]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "members" })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: "members" }));
    await waitFor(() => {
      expect(screen.getByText("@existing")).toBeInTheDocument();
    });
    fireEvent.change(screen.getByLabelText("Username to add"), { target: { value: "dev1" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) =>
          c[0].toString().includes("/teams/platform/memberships/dev1") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
  });

  it("manages organization assignments for a selected-type team", async () => {
    const selectedTeam = { ...platformTeam, organization_selection_type: "selected" };
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u === "/internal/status") return Promise.resolve(healthResponse());
      if (u.includes("/teams/platform/organizations/acme") && init?.method === "PUT") {
        return Promise.resolve(
          jsonResponse({ id: 12, login: "acme", avatar_url: "", description: null }, 201),
        );
      }
      if (u.includes("/teams/platform/organizations/legacy") && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("/teams/platform/organizations")) {
        return Promise.resolve(
          jsonResponse([{ id: 11, login: "legacy", avatar_url: "", description: "Old org" }]),
        );
      }
      if (u.includes("/enterprises/bleephub/teams")) {
        return Promise.resolve(jsonResponse([selectedTeam]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "organizations" })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: "organizations" }));
    await waitFor(() => {
      expect(screen.getByText("@legacy")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByLabelText("Organization to assign"), {
      target: { value: "acme" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Assign" }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) =>
          c[0].toString().includes("/teams/platform/organizations/acme") &&
          c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: "unassign" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) =>
          c[0].toString().includes("/teams/platform/organizations/legacy") &&
          c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("disables organization assignment editing unless the selection type is selected", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u === "/internal/status") return Promise.resolve(healthResponse());
      if (u.includes("/teams/platform/organizations")) {
        return Promise.resolve(
          jsonResponse([{ id: 11, login: "legacy", avatar_url: "", description: null }]),
        );
      }
      if (u.includes("/enterprises/bleephub/teams")) {
        return Promise.resolve(jsonResponse([platformTeam]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "organizations" })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: "organizations" }));
    await waitFor(() => {
      expect(screen.getByText("@legacy")).toBeInTheDocument();
    });
    expect(
      screen.getByText(/assignments can only be edited when the selection type is "selected"/i),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Organization to assign")).toBeDisabled();
    expect(screen.getByRole("button", { name: "unassign" })).toBeDisabled();
  });
});

describe("EnterprisePage settings tab", () => {
  function mockSettings() {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u === "/internal/status") return Promise.resolve(healthResponse());
      if (u.includes("/actions/cache/storage-limit") && init?.method === "PUT") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("/actions/cache/storage-limit")) {
        return Promise.resolve(jsonResponse({ max_cache_size_gb: 10 }));
      }
      if (u.includes("/dependabot/repository-access/default-level") && init?.method === "PUT") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("/dependabot/repository-access")) {
        return Promise.resolve(
          jsonResponse({
            default_level: "public",
            accessible_repositories: [
              { id: 7, full_name: "acme/private-lib", name: "private-lib" },
            ],
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
  }

  it("shows the Actions cache limit and Dependabot access, and updates the limit", async () => {
    mockSettings();
    renderPage();
    fireEvent.click(screen.getByRole("tab", { name: "Settings" }));

    await waitFor(() => {
      expect(screen.getByText("10 GB")).toBeInTheDocument();
    });
    expect(screen.getByText("acme/private-lib")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("New cache size limit in GB"), {
      target: { value: "25" },
    });
    fireEvent.click(screen.getByRole("button", { name: /update limit/i }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/actions/cache/storage-limit") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse(String(put![1]!.body))).toEqual({ max_cache_size_gb: 25 });
    });
  });

  it("surfaces settings load failures", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u === "/internal/status") return Promise.resolve(healthResponse());
      if (u.includes("/actions/cache/storage-limit")) {
        return Promise.resolve(jsonResponse({ message: "boom" }, 500));
      }
      if (u.includes("/dependabot/repository-access")) {
        return Promise.resolve(jsonResponse({ default_level: null, accessible_repositories: [] }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    fireEvent.click(screen.getByRole("tab", { name: "Settings" }));
    await waitFor(() => {
      expect(screen.getByText(/failed to load actions cache limit/i)).toBeInTheDocument();
    });
    expect(
      screen.getByText(/no repositories granted private-repository dependabot access/i),
    ).toBeInTheDocument();
  });
});

const budget = {
  id: "b-1",
  budget_scope: "enterprise",
  budget_entity_name: "",
  budget_amount: 500,
  prevent_further_usage: true,
  budget_product_sku: "actions",
  budget_type: "ProductPricing",
  budget_alerting: { will_alert: false, alert_recipients: [] },
};

const BUDGETS_PATH = "/api/v3/enterprises/acme/settings/billing/budgets";

// Base handler: enterprise slug + empty default (teams) tab + one budget on the
// billing list. Callers pass overrides keyed by a URL substring.
function mockBilling(overrides: Array<[string, (init?: RequestInit) => Response]> = []) {
  mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
    const u = url.toString();
    for (const [needle, make] of overrides) {
      if (u.includes(needle)) return Promise.resolve(make(init));
    }
    if (u.includes("/internal/status")) {
      return Promise.resolve(jsonResponse({ enterprise_slug: "acme" }));
    }
    if (u.includes(BUDGETS_PATH)) {
      return Promise.resolve(jsonResponse({ budgets: [budget], total_count: 1, has_next_page: false }));
    }
    // Default (teams) tab list and anything else.
    return Promise.resolve(jsonResponse([]));
  });
}


describe("EnterprisePage billing tab", () => {
  it("renders spending budgets on the Billing tab", async () => {
    mockBilling();
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Billing" }));

    await waitFor(() => {
      expect(screen.getByText("actions")).toBeInTheDocument();
    });
    expect(screen.getByText(/\$500/)).toBeInTheDocument();
    expect(screen.getByText(/scope: enterprise/)).toBeInTheDocument();
  });

  it("creates a budget via POST to the budgets endpoint", async () => {
    mockBilling([
      [
        BUDGETS_PATH,
        (init) =>
          init?.method === "POST"
            ? jsonResponse({ budget })
            : jsonResponse({ budgets: [budget], total_count: 1, has_next_page: false }),
      ],
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Billing" }));
    fireEvent.click(await screen.findByRole("button", { name: "New budget" }));

    fireEvent.change(screen.getByLabelText("Product SKU"), { target: { value: "copilot" } });
    fireEvent.change(screen.getByLabelText("Budget amount (USD)"), { target: { value: "250" } });
    fireEvent.click(screen.getByLabelText(/prevent further usage/i));
    fireEvent.click(screen.getByRole("button", { name: "Create budget" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString() === BUDGETS_PATH && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toEqual({
        budget_amount: 250,
        prevent_further_usage: true,
        budget_scope: "enterprise",
        budget_type: "ProductPricing",
        budget_product_sku: "copilot",
        budget_entity_name: "",
        budget_alerting: { will_alert: false, alert_recipients: [] },
      });
    });
  });

  it("deletes a budget via DELETE after confirmation", async () => {
    mockBilling([
      [
        `${BUDGETS_PATH}/b-1`,
        () => jsonResponse({ message: "Budget successfully deleted.", budget_id: "b-1" }),
      ],
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Billing" }));
    fireEvent.click(await screen.findByRole("button", { name: "delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString() === `${BUDGETS_PATH}/b-1` && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });
});
