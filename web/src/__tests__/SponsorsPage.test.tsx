import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { formatCents, SponsorsDashboardPage, SponsorsPage } from "../pages/SponsorsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

const fiveDollarTier = {
  id: 1,
  node_id: "ST_kgDO00000001",
  name: "$5 a month",
  description: "Coffee",
  monthly_price_in_cents: 500,
  monthly_price_in_dollars: 5,
  is_one_time: false,
  is_custom_amount: false,
  is_draft: false,
  is_published: true,
  is_retired: false,
};
const tenDollarTier = { ...fiveDollarTier, id: 2, node_id: "ST_kgDO00000002", name: "$10 a month", monthly_price_in_cents: 1000, monthly_price_in_dollars: 10 };

const listing = {
  id: 1,
  node_id: "SL_kgDO00000001",
  slug: "mona",
  name: "Mona",
  sponsorable_login: "mona",
  sponsorable_type: "User",
  short_description: "Keeps the lights on",
  full_description: "Full profile",
  is_public: true,
  patreon_enabled: false,
  fiscal_host: "",
  goal: { kind: "MONTHLY_SPONSORSHIP_AMOUNT", target_value: 2000, percent_complete: 25, description: "", title: "Earn $20 per month" },
  tiers: [fiveDollarTier, tenDollarTier],
  featured_items: [],
};

function renderProfile(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/sponsors" element={<SponsorsPage />} />
          <Route path="/ui/sponsors/:login" element={<SponsorsPage />} />
          <Route path="/ui/sponsors/:login/dashboard" element={<SponsorsDashboardPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

describe("formatCents", () => {
  it("renders integer cents without floating-point arithmetic", () => {
    expect(formatCents(0)).toBe("$0.00");
    expect(formatCents(5)).toBe("$0.05");
    expect(formatCents(500)).toBe("$5.00");
    expect(formatCents(123456)).toBe("$1234.56");
    expect(formatCents(-250)).toBe("-$2.50");
  });
});

describe("SponsorsPage", () => {
  it("lists sponsorable accounts and the viewer's own sponsorships", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/sponsoring")) {
        return Promise.resolve(
          jsonResponse([
            {
              id: 9,
              node_id: "SP_kgDO00000009",
              sponsor_login: "octocat",
              sponsorable_login: "mona",
              privacy_level: "PUBLIC",
              is_one_time_payment: false,
              is_active: true,
              amount_in_cents: 500,
              tier: fiveDollarTier,
              pending_cancellation: false,
              pending_tier_id: 0,
              next_billing_date: null,
              pending_effective_date: null,
            },
          ]),
        );
      }
      return Promise.resolve(jsonResponse([listing]));
    });
    renderProfile("/ui/sponsors");
    expect(await screen.findByRole("link", { name: "Mona" })).toHaveAttribute("href", "/ui/sponsors/mona");
    expect((await screen.findAllByText("$5.00")).length).toBe(2);
  });

  it("shows the goal progress bar and lets a viewer select a tier", async () => {
    mockFetch.mockImplementation((_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "POST") return Promise.resolve(jsonResponse({ id: 5 }, 201));
      return Promise.resolve(jsonResponse({ listing, viewer_can_admin: false, viewer_sponsorship: null, sponsors: [] }));
    });
    renderProfile("/ui/sponsors/mona");
    const progress = await screen.findByRole("progressbar", { name: "Earn $20 per month" });
    expect(progress).toHaveAttribute("aria-valuenow", "25");

    fireEvent.click(screen.getAllByRole("button", { name: "Select" })[0]!);
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === "POST");
      expect(post).toBeTruthy();
      expect(String(post![0])).toBe("/ui-data/sponsors/mona/sponsorship");
      expect(JSON.parse(String((post![1] as RequestInit).body))).toMatchObject({ tier_id: 1, is_recurring: true });
    });
  });

  it("offers to open a profile only when the viewer may administer the account", async () => {
    mockFetch.mockImplementation(() =>
      Promise.resolve(jsonResponse({ listing: null, viewer_can_admin: false, viewer_sponsorship: null, sponsors: [] })),
    );
    renderProfile("/ui/sponsors/mona");
    expect(await screen.findByText("No GitHub Sponsors profile")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open a Sponsors profile" })).not.toBeInTheDocument();

    cleanup();
    mockFetch.mockReset();
    mockFetch.mockImplementation(() =>
      Promise.resolve(jsonResponse({ listing: null, viewer_can_admin: true, viewer_sponsorship: null, sponsors: [] })),
    );
    renderProfile("/ui/sponsors/mona");
    expect(await screen.findByRole("button", { name: "Open a Sponsors profile" })).toBeInTheDocument();
  });

  it("surfaces a failed sponsorship as an error banner rather than a silent no-op", async () => {
    mockFetch.mockImplementation((_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "POST") return Promise.resolve(jsonResponse({ message: "tier is not published" }, 422));
      return Promise.resolve(jsonResponse({ listing, viewer_can_admin: false, viewer_sponsorship: null, sponsors: [] }));
    });
    renderProfile("/ui/sponsors/mona");
    fireEvent.click((await screen.findAllByRole("button", { name: "Select" }))[0]!);
    expect(await screen.findByText(/tier is not published/)).toBeInTheDocument();
  });
});

describe("SponsorsDashboardPage", () => {
  const dashboard = {
    listing: { ...listing, next_payout_date: "2026-09-01", contact_email: "mona@example.test" },
    sponsorships: [
      {
        id: 9,
        node_id: "SP_kgDO00000009",
        sponsor_login: "octocat",
        sponsorable_login: "mona",
        privacy_level: "PRIVATE",
        is_one_time_payment: false,
        is_active: true,
        amount_in_cents: 500,
        tier: fiveDollarTier,
        pending_cancellation: false,
        pending_tier_id: 2,
        next_billing_date: null,
        pending_effective_date: null,
      },
    ],
    activities: [{ id: 1, action: "NEW_SPONSORSHIP", sponsor_login: "octocat", timestamp: "2026-08-01T00:00:00Z" }],
    invoices: [
      {
        id: 1,
        sponsor_login: "octocat",
        amount_in_cents: 500,
        period_start: "2026-08-01T00:00:00Z",
        period_end: "2026-09-01T00:00:00Z",
        one_time: false,
        prorated: false,
        status: "paid",
        payout_id: 0,
      },
    ],
    payouts: [],
    lifetime_values: [{ sponsor_login: "octocat", amount_in_cents: 500 }],
    monthly_estimated_income_in_cents: 500,
    estimated_next_payout_in_cents: 500,
  };

  it("renders the maintainer's money in integer cents and runs a payout", async () => {
    mockFetch.mockImplementation((_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "POST") return Promise.resolve(jsonResponse({ id: 1 }, 201));
      return Promise.resolve(jsonResponse(dashboard));
    });
    renderProfile("/ui/sponsors/mona/dashboard");
    expect(await screen.findByRole("heading", { name: "Sponsors dashboard" })).toBeInTheDocument();
    expect(screen.getAllByText("$5.00").length).toBeGreaterThan(0);
    expect(screen.getByText("Tier change pending")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Run payout" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        ([url, init]) => (init as RequestInit | undefined)?.method === "POST" && String(url).endsWith("/payouts"),
      );
      expect(post).toBeTruthy();
    });
  });

  it("disables the payout button when nothing is owed", async () => {
    mockFetch.mockImplementation(() =>
      Promise.resolve(jsonResponse({ ...dashboard, estimated_next_payout_in_cents: 0 })),
    );
    renderProfile("/ui/sponsors/mona/dashboard");
    expect(await screen.findByRole("button", { name: "Run payout" })).toBeDisabled();
  });
});

describe("SponsorsPage privacy", () => {
  it("marks a private sponsorship the viewer is a party to and omits others", async () => {
    const privateSponsorship = {
      id: 3,
      node_id: "SP_kgDO00000003",
      sponsor_login: "octocat",
      sponsorable_login: "mona",
      privacy_level: "PRIVATE",
      is_one_time_payment: false,
      is_active: true,
      amount_in_cents: 1000,
      tier: tenDollarTier,
      pending_cancellation: false,
      pending_tier_id: 0,
      next_billing_date: null,
      pending_effective_date: null,
    };
    mockFetch.mockImplementation(() =>
      Promise.resolve(
        jsonResponse({ listing, viewer_can_admin: false, viewer_sponsorship: null, sponsors: [privateSponsorship] }),
      ),
    );
    renderProfile("/ui/sponsors/mona");
    expect(await screen.findByText(/octocat · \$10\.00 a month \(private\)/)).toBeInTheDocument();

    cleanup();
    mockFetch.mockReset();
    // The server filters a private sponsorship out for a third party, so the
    // page shows the honest empty state rather than a redacted row.
    mockFetch.mockImplementation(() =>
      Promise.resolve(jsonResponse({ listing, viewer_can_admin: false, viewer_sponsorship: null, sponsors: [] })),
    );
    renderProfile("/ui/sponsors/mona");
    expect(await screen.findByText("No public sponsors yet")).toBeInTheDocument();
  });
});
