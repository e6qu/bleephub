// A 404 on a page's PRIMARY resource renders github.com's "not found" page
// instead of the raw error banner; every other failure (500, network) keeps
// the banner. Owning resources (repo, profile, org, gist) get the full-page
// 404 (RepoNotFound); sub-resources inside an existing repo (release, run,
// discussion, wiki page, tree/blob path) keep the repo chrome and render the
// 404 state inside the shell.
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import type { ReactElement } from "react";

import { RepoDetailPage, RepoFilePage } from "../pages/RepoDetailPage.js";
import { ProfilePage } from "../pages/ProfilePage.js";
import { OrgOverviewPage } from "../pages/OrgOverviewPage.js";
import { GistDetailPage } from "../pages/GistDetailPage.js";
import { ReleasesPage } from "../pages/ReleasesPage.js";
import { DiscussionsPage } from "../pages/DiscussionsPage.js";
import { RunDetailPage } from "../pages/RunDetailPage.js";
import { WikiPage } from "../pages/WikiPage.js";
import { RepoNotFound } from "../components/RepoNotFound.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const notFound = () => Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
const serverError = () => Promise.resolve(jsonResponse({ message: "Internal Server Error" }, 500));

function renderAt(routes: Array<{ path: string; element: ReactElement }>, entry: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[entry]}>
        <Routes>
          {routes.map((r) => (
            <Route key={r.path} path={r.path} element={r.element} />
          ))}
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const repoData = (over: Record<string, unknown> = {}) => ({
  id: 1,
  name: "test",
  full_name: "admin/test",
  description: "a repo",
  default_branch: "main",
  visibility: "public",
  private: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
  pushed_at: "2026-01-02T00:00:00Z",
  stargazers_count: 0,
  subscribers_count: 0,
  forks_count: 0,
  owner: { login: "admin", type: "User" },
  permissions: { admin: true, push: true, pull: true },
  ...over,
});

const commitsData = [
  {
    sha: "abc123",
    commit: {
      message: "Initial commit",
      author: { name: "Admin", email: "a@b", date: "2026-01-01T00:00:00Z" },
    },
  },
];

// ─── Repository (owning resource → full-page 404) ───────────────────────────

describe("RepoDetailPage not-found", () => {
  it("renders the full-page 404 for a missing repository", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/nope")) return notFound();
      return Promise.resolve(jsonResponse([]));
    });
    renderAt([{ path: "/ui/:owner/:repo", element: <RepoDetailPage /> }], "/ui/admin/nope");
    expect(await screen.findByText("This page does not exist")).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load/i)).not.toBeInTheDocument();
  });

  it("keeps the raw error banner when the repository read answers 500", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/test")) return serverError();
      return Promise.resolve(jsonResponse([]));
    });
    renderAt([{ path: "/ui/:owner/:repo", element: <RepoDetailPage /> }], "/ui/admin/test");
    expect(await screen.findByText(/Failed to load admin\/test/i)).toBeInTheDocument();
    expect(screen.queryByText("This page does not exist")).not.toBeInTheDocument();
  });

  it("renders an in-shell missing-branch state when the tree ref does not exist", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      if (u.includes("/commits?")) return notFound();
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/tree/:ref/*", element: <RepoDetailPage /> }],
      "/ui/admin/test/tree/ghost",
    );
    expect(await screen.findByText("This branch could not be found")).toBeInTheDocument();
    // The repo shell stays: the header nav still shows the repo tabs.
    expect(screen.getByText("Issues")).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load/i)).not.toBeInTheDocument();
  });

  it("renders an in-shell missing-path state when the tree path does not exist", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      if (u.includes("/commits?")) return Promise.resolve(jsonResponse(commitsData));
      if (u.includes("/contents/")) return notFound();
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/tree/:ref/*", element: <RepoDetailPage /> }],
      "/ui/admin/test/tree/main/no-such-dir",
    );
    expect(await screen.findByText("This branch or path could not be found")).toBeInTheDocument();
    expect(screen.getByText(/does not contain the path "no-such-dir"/)).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load/i)).not.toBeInTheDocument();
  });
});

describe("RepoFilePage (blob) not-found", () => {
  it("renders an in-shell missing-path state for a missing blob", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/contents/")) return notFound();
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/blob/:ref/*", element: <RepoFilePage /> }],
      "/ui/admin/test/blob/main/nope.txt",
    );
    expect(await screen.findByText("This branch or path could not be found")).toBeInTheDocument();
    expect(screen.getByText(/does not contain the path "nope\.txt"/)).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load/i)).not.toBeInTheDocument();
  });

  it("keeps the raw error banner when the blob read answers 500", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/contents/")) return serverError();
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/blob/:ref/*", element: <RepoFilePage /> }],
      "/ui/admin/test/blob/main/nope.txt",
    );
    expect(await screen.findByText(/Failed to load nope\.txt/i)).toBeInTheDocument();
    expect(screen.queryByText("This branch or path could not be found")).not.toBeInTheDocument();
  });
});

// ─── Profile / org / gist (owning resources → full-page 404) ────────────────

describe("ProfilePage not-found", () => {
  it("renders the full-page 404 for an unknown login", async () => {
    mockFetch.mockImplementation(() => notFound());
    renderAt([{ path: "/ui/:login", element: <ProfilePage /> }], "/ui/ghost");
    expect(await screen.findByText("This page does not exist")).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load profile/i)).not.toBeInTheDocument();
  });
});

describe("OrgOverviewPage not-found", () => {
  it("renders the full-page 404 for an unknown organization", async () => {
    mockFetch.mockImplementation(() => notFound());
    renderAt([{ path: "/ui/orgs/:org", element: <OrgOverviewPage /> }], "/ui/orgs/nope");
    expect(await screen.findByText("This page does not exist")).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load organization/i)).not.toBeInTheDocument();
  });
});

describe("GistDetailPage not-found", () => {
  it("renders the full-page 404 for an unknown gist id", async () => {
    mockFetch.mockImplementation(() => notFound());
    renderAt([{ path: "/ui/gists/:id", element: <GistDetailPage /> }], "/ui/gists/deadbeef");
    expect(await screen.findByText("This page does not exist")).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load gist/i)).not.toBeInTheDocument();
  });

  it("keeps the raw error banner when the gist read answers 500", async () => {
    mockFetch.mockImplementation(() => serverError());
    renderAt([{ path: "/ui/gists/:id", element: <GistDetailPage /> }], "/ui/gists/deadbeef");
    expect(await screen.findByText(/Failed to load gist/i)).toBeInTheDocument();
    expect(screen.queryByText("This page does not exist")).not.toBeInTheDocument();
  });
});

// ─── Sub-resources inside an existing repo (in-shell 404) ───────────────────

describe("ReleasesPage detail not-found", () => {
  it("renders an in-shell 404 for a missing release", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/releases/99")) return notFound();
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/releases/:releaseId", element: <ReleasesPage /> }],
      "/ui/admin/test/releases/99",
    );
    expect(await screen.findByText("Release not found")).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load release/i)).not.toBeInTheDocument();
  });

  it("keeps the raw error banner when the release read answers 500", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/releases/99")) return serverError();
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/releases/:releaseId", element: <ReleasesPage /> }],
      "/ui/admin/test/releases/99",
    );
    expect(await screen.findByText(/Failed to load release/i)).toBeInTheDocument();
    expect(screen.queryByText("Release not found")).not.toBeInTheDocument();
  });
});

describe("DiscussionsPage detail not-found", () => {
  it("renders an in-shell 404 for a missing discussion (GraphQL NOT_FOUND)", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/api/graphql") && (init?.method ?? "GET") === "POST") {
        return Promise.resolve(
          jsonResponse({ errors: [{ type: "NOT_FOUND", message: "Could not resolve to a Discussion" }] }),
        );
      }
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/discussions/:number", element: <DiscussionsPage /> }],
      "/ui/admin/test/discussions/7",
    );
    expect(await screen.findByText("Discussion #7 not found")).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load discussion/i)).not.toBeInTheDocument();
  });

  it("keeps the raw error banner when the discussion read answers 500", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/api/graphql") && (init?.method ?? "GET") === "POST") return serverError();
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/discussions/:number", element: <DiscussionsPage /> }],
      "/ui/admin/test/discussions/7",
    );
    expect(await screen.findByText(/Failed to load discussion #7/i)).toBeInTheDocument();
    expect(screen.queryByText("Discussion #7 not found")).not.toBeInTheDocument();
  });
});

describe("RunDetailPage not-found", () => {
  it("renders an in-shell 404 for a missing run", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/actions/runs/999")) return notFound();
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData()));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt(
      [{ path: "/ui/:owner/:repo/actions/runs/:runId", element: <RunDetailPage /> }],
      "/ui/admin/test/actions/runs/999",
    );
    expect(await screen.findByText("Run #999 not found")).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load run/i)).not.toBeInTheDocument();
  });
});

// ─── Wiki: missing slug — create CTA for writers, plain 404 for readers ─────

const wikiHome = {
  slug: "home",
  title: "Home",
  body: "# Welcome",
  author: "admin",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-02-01T00:00:00Z",
};

function mockWiki(push: boolean, pageStatus: 404 | 500) {
  mockFetch.mockImplementation((url: RequestInfo | URL) => {
    const u = url.toString();
    if (u.endsWith("/wiki/pages")) return Promise.resolve(jsonResponse([wikiHome]));
    if (u.includes("/wiki/pages/getting-started")) {
      return pageStatus === 404 ? notFound() : serverError();
    }
    if (u.endsWith("/repos/admin/test")) {
      return Promise.resolve(jsonResponse(repoData({ permissions: { admin: push, push, pull: true } })));
    }
    return Promise.resolve(jsonResponse([]));
  });
}

describe("WikiPage missing page", () => {
  const wikiRoutes = [{ path: "/ui/:owner/:repo/wiki/:slug", element: <WikiPage /> }];

  it("offers writers a title-prefilled create affordance", async () => {
    mockWiki(true, 404);
    renderAt(wikiRoutes, "/ui/admin/test/wiki/getting-started");
    expect(await screen.findByText("New page?")).toBeInTheDocument();
    expect(screen.getByText(/does not exist yet/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Create “getting started”/ })).toBeInTheDocument();
    // The wiki list rail survives the missing page.
    expect(screen.getByRole("link", { name: "Home" })).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load page/i)).not.toBeInTheDocument();
  });

  it("shows readers the plain 404 text with no create affordance", async () => {
    mockWiki(false, 404);
    renderAt(wikiRoutes, "/ui/admin/test/wiki/getting-started");
    expect(await screen.findByText("This page does not exist")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Create/ })).not.toBeInTheDocument();
    expect(screen.queryByText(/Failed to load page/i)).not.toBeInTheDocument();
  });

  it("keeps the raw error banner when the page read answers 500", async () => {
    mockWiki(true, 500);
    renderAt(wikiRoutes, "/ui/admin/test/wiki/getting-started");
    expect(await screen.findByText(/Failed to load page/i)).toBeInTheDocument();
    expect(screen.queryByText("New page?")).not.toBeInTheDocument();
  });
});

describe("catch-all 404 route", () => {
  // App.tsx routes /ui/* to this page so unknown paths get github.com's 404
  // instead of silently landing on the dashboard.
  it("renders the not-found page for unknown routes", () => {
    render(<RepoNotFound />);
    expect(screen.getByText("This page does not exist")).toBeInTheDocument();
  });
});
