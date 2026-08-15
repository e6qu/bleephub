import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { UsersPage } from "../pages/UsersPage.js";

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
      <BrowserRouter>
        <UsersPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const bob = {
  id: 7,
  login: "bob",
  type: "User",
  site_admin: false,
  created_at: "2026-01-01T00:00:00Z",
  suspended_at: null,
};

describe("UsersPage suspend user", () => {
  it("suspends a user via PUT /users/{login}/suspended after confirming", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url === "/api/v3/users?per_page=100") return Promise.resolve(jsonResponse([bob]));
      if (url === "/api/v3/users/bob" && method === "GET")
        return Promise.resolve(jsonResponse(bob));
      if (url === "/api/v3/users/bob/suspended" && method === "PUT")
        return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(jsonResponse([]));
    });

    renderPage();

    const suspendBtn = await screen.findByRole("button", { name: "Suspend user bob" });
    fireEvent.click(suspendBtn);

    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/users/bob/suspended" && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
  });
});
