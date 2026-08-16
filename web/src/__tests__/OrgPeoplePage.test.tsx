import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgPeoplePage } from "../pages/OrgPeoplePage.js";

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
      <MemoryRouter initialEntries={["/ui/orgs/acme/people"]}>
        <Routes>
          <Route path="/ui/orgs/:org/people" element={<OrgPeoplePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const alice = {
  id: 1,
  login: "alice",
  type: "User",
  site_admin: false,
  avatar_url: "",
};

describe("OrgPeoplePage publicize membership", () => {
  it("publicizes own membership via PUT /orgs/{org}/public_members/{login}", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse(alice));
      if (url.startsWith("/api/v3/orgs/acme/members")) return Promise.resolve(jsonResponse([alice]));
      if (url === "/api/v3/orgs/acme/public_members" && method === "GET")
        return Promise.resolve(jsonResponse([]));
      if (url === "/api/v3/orgs/acme/public_members/alice" && method === "PUT")
        return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(jsonResponse([]));
    });

    renderPage();

    const makePublic = await screen.findByRole("button", { name: "Make membership public" });
    fireEvent.click(makePublic);

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/orgs/acme/public_members/alice" && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
  });
});
