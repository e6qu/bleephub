import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { OrgSettingsPage } from "../pages/OrgSettingsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

const response = (body: unknown) =>
  new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

it("creates an expiring user budget with the fields GitHub requires", async () => {
  mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
    const path = url.toString();
    if (path.endsWith("/api/v3/orgs/acme")) {
      return Promise.resolve(
        response({ login: "acme", name: "Acme", description: "", email: "" }),
      );
    }
    if (path.includes("/settings/billing/usage/summary")) {
      return Promise.resolve(
        response({
          timePeriod: { year: 2026 },
          organization: "acme",
          usageItems: [],
        }),
      );
    }
    if (path.includes("/settings/billing/budgets") && init?.method === "POST") {
      return Promise.resolve(
        response({ message: "Budget created successfully", budget: {} }),
      );
    }
    if (path.includes("/settings/billing/budgets")) {
      return Promise.resolve(
        response({ budgets: [], total_count: 0, has_next_page: false }),
      );
    }
    return Promise.resolve(response({}));
  });

  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <MemoryRouter initialEntries={["/ui/orgs/acme/settings"]}>
        <Routes>
          <Route path="/ui/orgs/:org/settings" element={<OrgSettingsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );

  fireEvent.change(await screen.findByLabelText("Product SKU"), {
    target: { value: "premium_requests" },
  });
  fireEvent.change(screen.getByLabelText("Scope"), {
    target: { value: "user" },
  });
  expect(screen.getByRole("option", { name: "One user" })).toHaveValue("user");
  const user = screen.getByLabelText("User");
  expect(screen.getByLabelText("Expiration date (optional)")).toHaveAttribute(
    "type",
    "date",
  );
  const prevent = screen.getByLabelText(
    "Prevent further usage when the budget is exceeded",
  );
  expect(user).toBeRequired();
  expect(prevent).toBeChecked();
  expect(prevent).toBeDisabled();
  expect(screen.getByRole("button", { name: "Create budget" })).toBeDisabled();

  fireEvent.change(screen.getByLabelText("Scope"), {
    target: { value: "organization" },
  });
  fireEvent.change(screen.getByLabelText("Product SKU"), {
    target: { value: "actions_linux" },
  });
  fireEvent.change(screen.getByLabelText("Scope"), {
    target: { value: "multi_user_customer" },
  });
  expect(screen.getByLabelText("Product SKU")).toHaveValue("ai_credits");
  expect(prevent).toBeChecked();
  expect(prevent).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Scope"), {
    target: { value: "user" },
  });
  fireEvent.change(screen.getByLabelText("Product SKU"), {
    target: { value: "premium_requests" },
  });

  fireEvent.change(screen.getByLabelText("User"), {
    target: { value: "octocat" },
  });
  fireEvent.change(screen.getByLabelText("Expiration date (optional)"), {
    target: { value: "2099-12-31" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create budget" }));

  await waitFor(() => {
    const post = mockFetch.mock.calls.find(
      (call) =>
        call[0]
          .toString()
          .includes("/organizations/acme/settings/billing/budgets") &&
        call[1]?.method === "POST",
    );
    expect(post).toBeDefined();
    expect(JSON.parse(String(post![1]!.body))).toMatchObject({
      budget_product_sku: "premium_requests",
      budget_scope: "user",
      user: "octocat",
      expires_at: "2099-12-31",
      prevent_further_usage: true,
    });
  });
});
