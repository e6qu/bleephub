import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { MyOrganizationsPage } from "../pages/MyOrganizationsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown) {
  return new Response(JSON.stringify(data), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <MyOrganizationsPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const org = {
  id: 1,
  login: "acme",
  name: "Acme Inc",
  description: "We make everything",
  members_can_create_repositories: true,
  created_at: "2024-01-01T00:00:00Z",
  avatar_url: "https://example.test/acme.png",
};

describe("MyOrganizationsPage", () => {
  it("lists the authenticated user's organizations via GET /user/orgs", async () => {
    mockFetch.mockResolvedValue(jsonResponse([org]));
    renderPage();
    const link = await screen.findByRole("link", { name: /acme/i });
    expect(link.getAttribute("href")).toBe("/ui/orgs/acme");
    expect(screen.getByText("We make everything")).toBeTruthy();
    const called = mockFetch.mock.calls[0]?.[0] as string;
    expect(called).toContain("/api/v3/user/orgs");
  });

  it("shows an empty state when the user has no organizations", async () => {
    mockFetch.mockResolvedValue(jsonResponse([]));
    renderPage();
    await waitFor(() => expect(screen.getByText("No organizations yet")).toBeTruthy());
  });

  it("accepts a pending invitation via PATCH /user/memberships/orgs/{org}", async () => {
    const pending = {
      state: "pending",
      role: "member",
      organization: { login: "acme", id: 1, avatar_url: "", description: null },
    };
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url.includes("/api/v3/user/memberships/orgs?state=pending"))
        return Promise.resolve(jsonResponse([pending]));
      if (url.includes("/api/v3/user/memberships/orgs/acme") && method === "PATCH")
        return Promise.resolve(jsonResponse({ state: "active", role: "member" }));
      if (url.includes("/api/v3/user/orgs")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Join" }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => String(c[0]).includes("/api/v3/user/memberships/orgs/acme") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeTruthy();
      expect(String(patch![1].body)).toContain('"state":"active"');
    });
  });
});
