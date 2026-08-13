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
