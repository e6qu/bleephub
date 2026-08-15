import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgSettingsPage } from "../pages/OrgSettingsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

const orgProfile = {
  login: "acme",
  id: 2,
  avatar_url: "",
  description: "Acme org",
  name: "Acme Inc",
  company: null,
  blog: null,
  location: null,
  email: "billing@acme.test",
  twitter_username: null,
  public_repos: 3,
  followers: 0,
  following: 0,
  html_url: "",
  created_at: "2026-01-01T00:00:00Z",
};

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/ui/orgs/acme/settings"]}>
        <Routes>
          <Route path="/ui/orgs/:org/settings" element={<OrgSettingsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("OrgSettingsPage", () => {
  it("renders the org profile form and settings section links", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      if (url.toString().endsWith("/api/v3/orgs/acme")) return Promise.resolve(jsonResponse(orgProfile));
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    // Prefilled display name + a settings-section link.
    expect(await screen.findByDisplayValue("Acme Inc")).toBeInTheDocument();
    // "Member privileges" is unique to the settings landing (the header tab is "Governance").
    expect(screen.getByRole("link", { name: /Member privileges/ })).toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: /Webhooks/ }).length).toBeGreaterThan(0);
  });

  it("saves org profile edits via PATCH /orgs/{org}", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/orgs/acme") && init?.method === "PATCH") return Promise.resolve(jsonResponse({ ...orgProfile, name: "Acme Corp" }));
      if (u.endsWith("/api/v3/orgs/acme")) return Promise.resolve(jsonResponse(orgProfile));
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    const nameInput = await screen.findByDisplayValue("Acme Inc");
    fireEvent.change(nameInput, { target: { value: "Acme Corp" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => {
      const patch = mockFetch.mock.calls.find((c) => c[0].toString().endsWith("/api/v3/orgs/acme") && c[1]?.method === "PATCH");
      expect(patch).toBeDefined();
      expect(String(patch![1].body)).toContain("Acme Corp");
    });
  });
});

const budget = {
  id: "b-1",
  budget_scope: "organization",
  budget_entity_name: "",
  budget_amount: 100,
  prevent_further_usage: false,
  budget_product_sku: "actions_linux",
  budget_type: "ProductPricing",
  budget_alerting: { will_alert: false, alert_recipients: [] },
};

function mockBillingEndpoints(overrides: Record<string, (init?: RequestInit) => Response> = {}) {
  mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
    const u = url.toString();
    for (const [needle, make] of Object.entries(overrides)) {
      if (u.includes(needle)) return Promise.resolve(make(init));
    }
    if (u.includes("/settings/billing/budgets")) {
      return Promise.resolve(jsonResponse({ budgets: [budget], total_count: 1, has_next_page: false }));
    }
    if (u.includes("/settings/billing/usage/summary")) {
      return Promise.resolve(jsonResponse({ timePeriod: { year: 2026, month: 8 }, organization: "acme", usageItems: [] }));
    }
    if (u.endsWith("/api/v3/orgs/acme")) {
      return Promise.resolve(jsonResponse(orgProfile));
    }
    return Promise.resolve(jsonResponse({}));
  });
}


describe("OrgSettingsPage billing", () => {
  it("lists existing budgets", async () => {
    mockBillingEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("actions_linux")).toBeInTheDocument();
    });
  });

  it("creates a budget with the product SKU via POST", async () => {
    mockBillingEndpoints({
      "/settings/billing/budgets": (init) => {
        if (init?.method === "POST") {
          return jsonResponse({ message: "Budget created successfully", budget });
        }
        return jsonResponse({ budgets: [budget], total_count: 1, has_next_page: false });
      },
    });
    renderPage();

    await waitFor(() => {
      expect(screen.getByLabelText("Product SKU")).toBeInTheDocument();
    });
    fireEvent.change(screen.getByLabelText("Product SKU"), { target: { value: "copilot_premium" } });
    fireEvent.change(screen.getByLabelText("Amount (USD)"), { target: { value: "250" } });
    fireEvent.click(screen.getByRole("button", { name: "Create budget" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/organizations/acme/settings/billing/budgets") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      const body = JSON.parse(String(post![1]!.body));
      expect(body).toMatchObject({ budget_product_sku: "copilot_premium", budget_amount: 250, budget_scope: "organization" });
    });
  });

  it("deletes a budget after confirmation via DELETE", async () => {
    mockBillingEndpoints({
      "/settings/billing/budgets/b-1": (init) => {
        if (init?.method === "DELETE") return jsonResponse({ message: "Budget deleted successfully", id: "b-1" });
        return jsonResponse({});
      },
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Delete actions_linux budget" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/settings/billing/budgets/b-1") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });
});
