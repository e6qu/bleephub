import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgOverviewPage } from "../pages/OrgOverviewPage.js";

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

function renderPage(org = "acme") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[`/ui/orgs/${org}`]}>
        <Routes>
          <Route path="/ui/orgs/:org" element={<OrgOverviewPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const orgProfile = {
  login: "acme",
  id: 1,
  avatar_url: "",
  description: "Acme Corp",
  name: "Acme",
  company: null,
  blog: "https://acme.example",
  location: "Cloud",
  email: "team@acme.example",
  twitter_username: null,
  public_repos: 2,
  followers: 0,
  following: 0,
  html_url: "http://x/acme",
  created_at: "2026-01-01T00:00:00Z",
};

const repo = {
  id: 1,
  name: "api",
  full_name: "acme/api",
  description: "the api",
  default_branch: "main",
  visibility: "public",
  private: false,
  updated_at: "2026-02-01T00:00:00Z",
};

// The overview also probes the org profile README ({org}/.github), the pinned
// list (/ui-data) and the viewer's own membership; answer them all so a test
// exercising one facet doesn't crash on the others.
function mockOverview(overrides: (u: string) => Response | null = () => null) {
  mockFetch.mockImplementation((url: RequestInfo | URL) => {
    const u = url.toString();
    const o = overrides(u);
    if (o) return Promise.resolve(o);
    if (u.includes("/.github/readme")) return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    if (u.includes("/ui-data/orgs/acme/pinned")) return Promise.resolve(jsonResponse([]));
    if (u.includes("/user/memberships/orgs/")) return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    if (u.includes("/repos")) return Promise.resolve(jsonResponse([repo], 200, { Link: "" }));
    if (u.includes("/members")) return Promise.resolve(jsonResponse([{ id: 2, login: "dev", avatar_url: "", type: "User", site_admin: false }]));
    return Promise.resolve(jsonResponse(orgProfile));
  });
}

describe("OrgOverviewPage", () => {
  it("renders the org profile, member count, and repository preview", async () => {
    mockOverview();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText("Acme")).toBeInTheDocument();
    });
    expect(screen.getByText("Acme Corp")).toBeInTheDocument();
    expect(screen.getByText("1 member")).toBeInTheDocument();
    expect(screen.getByText("2 repositories")).toBeInTheDocument();
    expect(screen.getByText("api")).toBeInTheDocument();
    // Without pins the repo grid keeps its plain label.
    expect(screen.queryByText("Recently updated")).not.toBeInTheDocument();
    // OrgHeader is present (uniform org chrome).
    expect(screen.getByRole("navigation", { name: /organization/i })).toBeInTheDocument();
  });

  it("renders the org profile README from the {org}/.github repo", async () => {
    // "# Welcome to Acme" in contents-API base64.
    const b64 = btoa("# Welcome to Acme");
    mockOverview((u) =>
      u.includes("/ui-data/users/acme/profile-readme")
        ? jsonResponse({ readme: { content: b64, encoding: "base64", name: "README.md", path: "README.md" } })
        : null,
    );
    renderPage();

    expect(await screen.findByRole("heading", { name: "Welcome to Acme" })).toBeInTheDocument();
  });

  it("shows pinned repositories and relabels the recent grid when pins exist", async () => {
    const pinnedRepo = { ...repo, id: 7, name: "flagship", full_name: "acme/flagship", stargazers_count: 12, forks_count: 3, language: "Go" };
    mockOverview((u) => (u.includes("/ui-data/orgs/acme/pinned") ? jsonResponse([pinnedRepo]) : null));
    renderPage();

    expect(await screen.findByText("Pinned repositories")).toBeInTheDocument();
    expect(screen.getByText("flagship")).toBeInTheDocument();
    // Language dot + counts render on the pinned card.
    expect(screen.getByText("Go")).toBeInTheDocument();
    // With pins present the recent grid is relabeled.
    expect(screen.getByText("Recently updated")).toBeInTheDocument();
    // Not an org admin → no edit-pins control.
    expect(screen.queryByRole("button", { name: /edit pins/i })).not.toBeInTheDocument();
  });

  it("lets an org owner edit pins via PUT /ui-data/orgs/{org}/pinned", async () => {
    mockOverview((u) => {
      if (u.includes("/user/memberships/orgs/acme"))
        return jsonResponse({ state: "active", role: "admin" });
      if (u.includes("/ui-data/orgs/acme/pinned")) return jsonResponse([]);
      return null;
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /edit pins/i }));
    // Pick the org repo in the dialog and save.
    fireEvent.click(await screen.findByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: /save pins/i }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/ui-data/orgs/acme/pinned") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse((put![1] as RequestInit).body as string)).toEqual({ repos: ["acme/api"] });
    });
  });

  it("surfaces an org load error", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/repos")) return Promise.resolve(jsonResponse([], 200, { Link: "" }));
      if (u.includes("/members")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage("missing");
    await waitFor(() => {
      expect(screen.getByText(/failed to load organization/i)).toBeInTheDocument();
    });
  });
});
