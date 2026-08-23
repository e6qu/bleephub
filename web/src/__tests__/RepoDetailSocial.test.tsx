import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { RepoDetailPage } from "../pages/RepoDetailPage.js";

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

const repo = {
  id: 1,
  name: "social-repo",
  full_name: "admin/social-repo",
  description: "socialised",
  default_branch: "main",
  visibility: "public",
  private: false,
  owner: { login: "admin", type: "User" },
  stargazers_count: 3,
  subscribers_count: 2,
  forks_count: 1,
  permissions: { admin: true, push: true, pull: true },
};

function installFetchRoutes() {
  mockFetch.mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    if (url === "/api/v3/repos/admin/social-repo") return Promise.resolve(jsonResponse(repo));
    if (url.startsWith("/api/v3/repos/admin/social-repo/branches"))
      return Promise.resolve(
        jsonResponse([
          { name: "main", commit: { sha: "a".repeat(40) }, protected: true },
          { name: "feature", commit: { sha: "b".repeat(40) } },
        ]),
      );
    if (url.startsWith("/api/v3/repos/admin/social-repo/commits?"))
      return Promise.resolve(jsonResponse([]));
    if (url.startsWith("/api/v3/repos/admin/social-repo/languages"))
      return Promise.resolve(jsonResponse({ Go: 3000, Shell: 1000 }));
    if (url.startsWith("/api/v3/repos/admin/social-repo/tags"))
      return Promise.resolve(
        jsonResponse([
          {
            name: "v1.0.0",
            zipball_url: "http://x/admin/social-repo/legacy.zip/refs/tags/v1.0.0",
            tarball_url: "http://x/admin/social-repo/legacy.tar.gz/refs/tags/v1.0.0",
            commit: { sha: "c".repeat(40), url: "http://x/api" },
          },
        ]),
      );
    if (url.includes("/issues?") || url.includes("/pulls?"))
      return Promise.resolve(jsonResponse([]));
    return Promise.resolve(jsonResponse({ message: `unexpected ${url}` }, 500));
  });
}

function renderPage(initialEntry = "/ui/admin/social-repo") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route path="/ui/:owner/:repo" element={<RepoDetailPage />} />
          <Route
            path="/ui/:owner/:repo/branches"
            element={<RepoDetailPage initialTab="branches" />}
          />
          <Route
            path="/ui/:owner/:repo/tags"
            element={<RepoDetailPage initialTab="tags" />}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RepoDetailPage social reads", () => {
  it("shows star/watcher/fork counts linking to their list views", async () => {
    installFetchRoutes();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/3 stars/)).toBeInTheDocument();
    });
    expect(screen.getByText(/2 watchers/).closest("a")).toHaveAttribute(
      "href",
      "/ui/admin/social-repo/watchers",
    );
    expect(screen.getByText(/1 fork/).closest("a")).toHaveAttribute(
      "href",
      "/ui/admin/social-repo/forks",
    );
  });

  it("renders the languages bar with percentages", async () => {
    installFetchRoutes();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Go")).toBeInTheDocument();
    });
    expect(screen.getByText("75.0%")).toBeInTheDocument();
    expect(screen.getByText("Shell")).toBeInTheDocument();
    expect(screen.getByText("25.0%")).toBeInTheDocument();
  });

  // G9 removed the repo sub-tab row, so these listings are reached by URL (the
  // branch/tag switcher's "View all …" footer links there) rather than by a
  // second row of tabs; the switcher footer itself is covered in
  // RepoDetailPage.test.tsx, where the fixture repository has commits.
  it("lists branches with default and protection state", async () => {
    installFetchRoutes();
    renderPage("/ui/admin/social-repo/branches");
    await waitFor(() => {
      expect(screen.getByText("feature")).toBeInTheDocument();
    });
    expect(screen.getByText("default")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "protected" })).toHaveAttribute(
      "href",
      "/ui/admin/social-repo/settings/branch-protection",
    );
  });

  it("lists tags with tarball and zipball download links", async () => {
    installFetchRoutes();
    renderPage("/ui/admin/social-repo/tags");
    await waitFor(() => {
      expect(screen.getByText("v1.0.0")).toBeInTheDocument();
    });
    expect(screen.getByText("zip")).toHaveAttribute(
      "href",
      "http://x/admin/social-repo/legacy.zip/refs/tags/v1.0.0",
    );
    expect(screen.getByText("tar.gz")).toHaveAttribute(
      "href",
      "http://x/admin/social-repo/legacy.tar.gz/refs/tags/v1.0.0",
    );
  });
});
