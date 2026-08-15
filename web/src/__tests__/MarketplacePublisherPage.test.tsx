import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { MarketplacePublisherPage } from "../pages/MarketplacePublisherPage.js";

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
      <MemoryRouter initialEntries={["/ui/apps/acme-app/marketplace"]}>
        <Routes>
          <Route path="/ui/apps/:publisher/marketplace" element={<MarketplacePublisherPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const plan = {
  url: "",
  accounts_url: "",
  id: 42,
  number: 1,
  name: "Pro",
  description: "Pro plan",
  monthly_price_in_cents: 1000,
  yearly_price_in_cents: 10000,
  price_model: "FLAT_RATE" as const,
  has_free_trial: false,
  unit_name: null,
  state: "published" as const,
  bullets: ["fast"],
};

const listing = {
  slug: "acme-app",
  name: "Acme",
  description: "Acme listing",
  full_description: "",
  setup_url: "https://acme.test/setup",
  installation_url: null,
  github_app_id: 1,
  oauth_app_client_id: null,
  published: true,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
  webhook_url: "https://acme.test/hook",
  webhook_content_type: "json" as const,
  webhook_active: true,
  webhook_id: 5,
  plans: [plan],
};

describe("MarketplacePublisherPage", () => {
  it("loads the listing and renders its pricing plans", async () => {
    mockFetch.mockResolvedValue(jsonResponse({ listing }));
    renderPage();
    await waitFor(() => expect(screen.getByText("Pro")).toBeInTheDocument());
    expect(mockFetch).toHaveBeenCalledWith(
      "/ui-data/settings/apps/acme-app/marketplace",
      expect.anything(),
    );
  });

  it("edits a plan via PUT .../marketplace/plans/{id}", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u === "/settings/apps/acme-app/marketplace/plans/42" && opts?.method === "PUT") {
        return Promise.resolve(jsonResponse({ ...plan, name: "Pro Plus" }));
      }
      return Promise.resolve(jsonResponse({ listing }));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("Pro")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    const nameInput = await screen.findByDisplayValue("Pro");
    fireEvent.change(nameInput, { target: { value: "Pro Plus" } });
    fireEvent.click(screen.getByRole("button", { name: "Save plan" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0] === "/settings/apps/acme-app/marketplace/plans/42" && c[1]?.method === "PUT",
      );
      expect(put).toBeDefined();
      expect(JSON.parse(String(put![1].body))).toMatchObject({
        name: "Pro Plus",
        price_model: "FLAT_RATE",
        state: "published",
      });
    });
  });

  it("deletes a plan via DELETE .../marketplace/plans/{id} after confirmation", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u === "/settings/apps/acme-app/marketplace/plans/42" && opts?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(jsonResponse({ listing }));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("Pro")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    // Confirm inside the confirmAction modal (its confirm button is labelled "Delete").
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0] === "/settings/apps/acme-app/marketplace/plans/42" && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });
});
