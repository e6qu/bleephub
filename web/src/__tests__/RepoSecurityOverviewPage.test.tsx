import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { RepoSecurityOverviewPage } from "../pages/RepoSecurityOverviewPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => { cleanup(); mockFetch.mockReset(); });

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/ui/admin/r/security"]}>
        <Routes>
          <Route path="/ui/:owner/:repo/security" element={<RepoSecurityOverviewPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RepoSecurityOverviewPage", () => {
  it("shows the set-up prompt when no SECURITY.md exists and links every feature", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/contents/")) return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      if (url.includes("/issues") || url.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ full_name: "admin/r", owner: { login: "admin", type: "User" } }));
    });
    renderPage();
    expect(await screen.findByText(/No security policy found/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Set up a security policy" })).toBeInTheDocument();
    // Each feature is linked from both the header security sub-nav and the
    // overview cards, so assert at least one link exists per feature.
    for (const name of ["Security advisories", "Secret scanning", "Code scanning", "Dependabot"]) {
      expect(screen.getAllByRole("link", { name }).length).toBeGreaterThan(0);
    }
  });

  it("links the existing policy when a SECURITY.md is present", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/contents/SECURITY.md")) return Promise.resolve(jsonResponse({ path: "SECURITY.md", name: "SECURITY.md", html_url: "x" }));
      if (url.includes("/contents/")) return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      if (url.includes("/issues") || url.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ full_name: "admin/r", owner: { login: "admin", type: "User" } }));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText(/This repository has a security policy/)).toBeInTheDocument());
    expect(screen.getByRole("link", { name: "View policy" })).toBeInTheDocument();
  });
});
