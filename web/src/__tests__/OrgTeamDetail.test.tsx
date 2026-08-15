import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { OrgTeamsPage } from "../pages/OrgTeamsPage.js";
import { OrgTeamDetailPage } from "../pages/OrgTeamDetailPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => { cleanup(); mockFetch.mockReset(); });

const team = { id: 3, name: "Platform", slug: "platform", description: "Core platform", privacy: "closed" };

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/orgs/:org/teams" element={<OrgTeamsPage />} />
          <Route path="/ui/orgs/:org/teams/:slug" element={<OrgTeamDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("Org teams", () => {
  it("links each team row to its detail page", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      if (String(input).endsWith("/orgs/acme/teams")) return Promise.resolve(jsonResponse([team]));
      return Promise.resolve(jsonResponse({}));
    });
    renderAt("/ui/orgs/acme/teams");
    const link = await screen.findByRole("link", { name: /Platform/ });
    expect(link).toHaveAttribute("href", "/ui/orgs/acme/teams/platform");
  });

  it("creates a team from the New team modal", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/orgs/acme/teams") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ ...team, name: "New Squad", slug: "new-squad" }, 201));
      }
      if (url.endsWith("/orgs/acme/teams")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({}));
    });
    renderAt("/ui/orgs/acme/teams");
    fireEvent.click(await screen.findByRole("button", { name: "New team" }));
    fireEvent.change(screen.getByLabelText("Team name"), { target: { value: "New Squad" } });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(([u, i]) => String(u).endsWith("/orgs/acme/teams") && i?.method === "POST");
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toMatchObject({ name: "New Squad", privacy: "closed" });
    });
  });

  it("renders team detail with Members / Repositories / Child teams tabs", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/teams/platform")) return Promise.resolve(jsonResponse(team));
      if (url.endsWith("/teams/platform/members")) return Promise.resolve(jsonResponse([{ id: 1, login: "octocat" }]));
      if (url.includes("/members")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/teams/platform");
    expect(await screen.findByText("@octocat")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Repositories" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Child teams" })).toBeInTheDocument();
  });
});
