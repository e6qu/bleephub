import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";

import {
  RepoComparePage,
  RepoDetailPage,
} from "../pages/RepoDetailPage.js";

vi.mock("../components/Shell.js", () => ({
  RepoHeader: ({ owner, repo }: { owner: string; repo: string }) => <div>{owner}/{repo}</div>,
}));

vi.mock("../hooks/useOpenCounts.js", () => ({
  useOpenCounts: () => ({}),
}));

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

const jsonResponse = (data: unknown, status = 200) =>
  new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const repo = {
  id: 1,
  name: "repo",
  full_name: "admin/repo",
  owner: { login: "admin", type: "User" },
  default_branch: "main",
  private: false,
  visibility: "public",
  pushed_at: "2026-01-03T00:00:00Z",
  ssh_url: "git@bleephub.test:admin/repo.git",
};

const commit = {
  sha: "a".repeat(40),
  commit: {
    message: "Fix repository reads",
    author: {
      name: "Admin",
      email: "admin@example.test",
      date: "2026-01-03T00:00:00Z",
    },
  },
};

function commonFetch(input: RequestInfo | URL) {
  const url = String(input);
  if (url === "/api/v3/repos/admin/repo") return jsonResponse(repo);
  if (url.split("?")[0]!.endsWith("/branches")) {
    return jsonResponse([
      { name: "main", commit: { sha: "a".repeat(40) } },
      { name: "feature", commit: { sha: "b".repeat(40) } },
    ]);
  }
  if (url.endsWith("/languages")) return jsonResponse({});
  if (url.includes("/issues?") || url.includes("/pulls?")) return jsonResponse([]);
  return null;
}

function renderAt(entry: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[entry]}>
        <Routes>
          <Route path="/ui/:owner/:repo" element={<RepoDetailPage />} />
          <Route path="/ui/:owner/:repo/commits" element={<RepoDetailPage initialTab="commits" />} />
          <Route path="/ui/:owner/:repo/compare/:range" element={<RepoComparePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

describe("repository collaboration journeys", () => {
  it("filters commit history with the official selectors and stable UTC date boundaries", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const common = commonFetch(input);
      if (common) return Promise.resolve(common);
      if (String(input).includes("/commits?")) return Promise.resolve(jsonResponse([commit]));
      return Promise.resolve(jsonResponse({ names: [] }));
    });

    renderAt("/ui/admin/repo/commits");
    expect(await screen.findByText("Fix repository reads")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Path"), { target: { value: "internal/server" } });
    fireEvent.change(screen.getByLabelText("Author"), { target: { value: "admin" } });
    fireEvent.change(screen.getByLabelText("Since"), { target: { value: "2026-01-01" } });
    fireEvent.change(screen.getByLabelText("Until"), { target: { value: "2026-01-31" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply filters" }));

    await waitFor(() => {
      const request = mockFetch.mock.calls
        .map(([input]) => String(input))
        .find((url) => url.includes("path=internal%2Fserver"));
      expect(request).toContain("author=admin");
      expect(request).toContain("since=2026-01-01T00%3A00%3A00.000Z");
      expect(request).toContain("until=2026-01-31T23%3A59%3A59.999Z");
    });
  });

  it("offers ref-specific source archives from the Code menu", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const common = commonFetch(input);
      if (common) return Promise.resolve(common);
      const url = String(input);
      if (url.includes("/commits?")) return Promise.resolve(jsonResponse([commit]));
      if (url.includes("/contents/")) return Promise.resolve(jsonResponse([]));
      if (url.includes("/readme")) return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      if (url.endsWith("/topics")) return Promise.resolve(jsonResponse({ names: [] }));
      if (url.split("?")[0]!.endsWith("/releases")) return Promise.resolve(jsonResponse([]));
      if (url.includes("/packages")) return Promise.resolve(jsonResponse([]));
      if (url.endsWith("/stargazers") || url.endsWith("/subscribers") || url.endsWith("/forks")) {
        return Promise.resolve(jsonResponse([]));
      }
      return Promise.resolve(jsonResponse({}));
    });

    renderAt("/ui/admin/repo");
    fireEvent.click(await screen.findByRole("button", { name: /Code/ }));
    expect(screen.getByRole("link", { name: "Download ZIP" })).toHaveAttribute(
      "href",
      "/api/v3/repos/admin/repo/zipball/main",
    );
    expect(screen.getByRole("link", { name: "Download TAR.GZ" })).toHaveAttribute(
      "href",
      "/api/v3/repos/admin/repo/tarball/main",
    );
  });

  it("loads a comparison and renders its commit and file patch", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const common = commonFetch(input);
      if (common) return Promise.resolve(common);
      if (String(input).includes("/compare/main...feature")) {
        return Promise.resolve(jsonResponse({
          status: "ahead",
          ahead_by: 1,
          behind_by: 0,
          total_commits: 1,
          commits: [commit],
          files: [{
            sha: "c".repeat(40),
            filename: "README.md",
            status: "modified",
            additions: 1,
            deletions: 0,
            changes: 1,
            patch: "@@ -1 +1 @@",
          }],
        }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    renderAt("/ui/admin/repo/compare/main...feature");
    expect(await screen.findByText("Fix repository reads")).toBeInTheDocument();
    expect(screen.getByText("README.md")).toBeInTheDocument();
    expect(screen.getByText("@@ -1 +1 @@")).toBeInTheDocument();
  });
});
