import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgPeoplePage } from "../pages/OrgPeoplePage.js";
import { OrgTeamsPage } from "../pages/OrgTeamsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderPeople(org = "acme") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[`/ui/orgs/${org}/people`]}>
        <Routes>
          <Route path="/ui/orgs/:org/people" element={<OrgPeoplePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function renderTeams(org = "acme") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[`/ui/orgs/${org}/teams`]}>
        <Routes>
          <Route path="/ui/orgs/:org/teams" element={<OrgTeamsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("OrgPeoplePage", () => {
  it("lists members from /api/v3/orgs/{org}/members and links to profiles", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse([{ id: 2, login: "dev", avatar_url: "", type: "User", site_admin: false }]),
    );
    renderPeople();
    await waitFor(() => {
      expect(screen.getByText("dev")).toBeInTheDocument();
    });
    const link = screen.getByRole("link", { name: "dev" });
    expect(link).toHaveAttribute("href", "/ui/dev");
    expect(mockFetch).toHaveBeenCalledWith("/api/v3/orgs/acme/members?per_page=100", expect.anything());
  });

  it("surfaces a members load error", async () => {
    mockFetch.mockResolvedValue(jsonResponse({ message: "Not Found" }, 404));
    renderPeople("missing");
    await waitFor(() => {
      expect(screen.getByText(/failed to load members/i)).toBeInTheDocument();
    });
  });

  const member = { id: 2, login: "dev", avatar_url: "", type: "User", site_admin: false };
  // The write controls are gated on the viewer being an org owner.
  const adminMembership = (u: string) =>
    u === "/api/v3/user/memberships/orgs/acme"
      ? jsonResponse({ state: "active", role: "admin" })
      : null;

  it("invites a member via PUT memberships", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      const gate = adminMembership(u);
      if (gate) return Promise.resolve(gate);
      if (u.endsWith("/memberships/newdev") && init?.method === "PUT") {
        return Promise.resolve(jsonResponse({ state: "active", role: "member" }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPeople();
    fireEvent.change(await screen.findByLabelText(/invite a member/i), { target: { value: "newdev" } });
    fireEvent.click(screen.getByRole("button", { name: /^invite$/i }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/memberships/newdev") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse((put![1] as RequestInit).body as string)).toEqual({ role: "member" });
    });
  });

  it("changes a member's role via the per-member select", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      const gate = adminMembership(u);
      if (gate) return Promise.resolve(gate);
      if (u.endsWith("/memberships/dev") && init?.method === "PUT") {
        return Promise.resolve(jsonResponse({ state: "active", role: "admin" }));
      }
      if (u.split("?")[0]!.endsWith("/members")) return Promise.resolve(jsonResponse([member]));
      return Promise.resolve(jsonResponse([]));
    });
    renderPeople();
    fireEvent.change(await screen.findByLabelText(/set role for dev/i), { target: { value: "admin" } });
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/memberships/dev") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse((put![1] as RequestInit).body as string)).toEqual({ role: "admin" });
    });
  });

  it("removes a member after confirmation", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      const gate = adminMembership(u);
      if (gate) return Promise.resolve(gate);
      if (u.endsWith("/memberships/dev") && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.split("?")[0]!.endsWith("/members")) return Promise.resolve(jsonResponse([member]));
      return Promise.resolve(jsonResponse([]));
    });
    renderPeople();
    fireEvent.click(await screen.findByRole("button", { name: /remove dev/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^remove$/i }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/memberships/dev") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });
});

describe("OrgTeamsPage", () => {
  it("lists teams from /api/v3/orgs/{org}/teams", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse([
        { id: 1, slug: "core", name: "Core", description: "core team", privacy: "closed", permission: "push", html_url: "http://x", parent: null },
      ]),
    );
    renderTeams();
    await waitFor(() => {
      expect(screen.getByText("Core")).toBeInTheDocument();
    });
    expect(screen.getByText("@core")).toBeInTheDocument();
    expect(mockFetch).toHaveBeenCalledWith("/api/v3/orgs/acme/teams", expect.anything());
  });

  it("shows an honest empty state when the org has no teams", async () => {
    mockFetch.mockResolvedValue(jsonResponse([]));
    renderTeams();
    await waitFor(() => {
      expect(screen.getByText("No teams")).toBeInTheDocument();
    });
  });

  it("nests child teams under their parent with an expand toggle", async () => {
    const parent = { id: 1, slug: "platform", name: "Platform", description: null, privacy: "closed", permission: "push", html_url: "http://x", parent: null };
    const child = { id: 2, slug: "platform-oncall", name: "Oncall", description: null, privacy: "closed", permission: "push", html_url: "http://x", parent: { slug: "platform", name: "Platform" } };
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u === "/api/v3/orgs/acme/teams") return Promise.resolve(jsonResponse([parent, child]));
      // team-full hydration supplies the member/repo counts
      if (u === "/api/v3/orgs/acme/teams/platform")
        return Promise.resolve(jsonResponse({ ...parent, members_count: 4, repos_count: 2 }));
      if (u === "/api/v3/orgs/acme/teams/platform-oncall")
        return Promise.resolve(jsonResponse({ ...child, members_count: 1, repos_count: 0 }));
      return Promise.resolve(jsonResponse([]));
    });
    renderTeams();

    await screen.findByText("Oncall");
    const toggle = screen.getByRole("button", { name: /collapse child teams of platform/i });
    expect(toggle).toHaveAttribute("aria-expanded", "true");

    // Counts hydrate lazily from the team-full endpoint.
    await waitFor(() => {
      expect(screen.getByText("4")).toBeInTheDocument();
    });

    fireEvent.click(toggle);
    await waitFor(() => {
      expect(screen.queryByText("Oncall")).not.toBeInTheDocument();
    });
  });
});
