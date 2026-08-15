import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { PackagesPage } from "../pages/PackagesPage.js";

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

function renderAt(path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/orgs/:org/packages" element={<PackagesPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const deletedPkg = {
  id: 9,
  name: "ghost-pkg",
  package_type: "container",
  visibility: "private",
  version_count: 1,
  updated_at: "2026-01-01T00:00:00Z",
};

describe("PackagesPage deleted packages", () => {
  it("lists deleted packages and restores one via POST .../restore", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/packages/container/ghost-pkg/restore") && init?.method === "POST") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("state=deleted")) return Promise.resolve(jsonResponse([deletedPkg]));
      // active list (any package_type) → empty
      if (u.includes("/packages")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/packages");

    const restoreBtn = await screen.findByRole("button", { name: "Restore package ghost-pkg" });
    fireEvent.click(restoreBtn);
    fireEvent.click(await screen.findByRole("button", { name: "Restore" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        ([u, i]) =>
          u.toString().includes("/api/v3/orgs/acme/packages/container/ghost-pkg/restore") &&
          (i as RequestInit)?.method === "POST",
      );
      expect(post).toBeTruthy();
    });
  });
});
