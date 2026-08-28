import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { RepoCommitPage, RepoComparePage, RepoDetailPage, RepoFilePage } from "../pages/RepoDetailPage.js";

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

function renderPage(path = "/ui/admin/test") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/:owner/:repo" element={<RepoDetailPage />} />
          <Route path="/ui/:owner/:repo/commits" element={<RepoDetailPage initialTab="commits" />} />
          <Route path="/ui/:owner/:repo/branches" element={<RepoDetailPage initialTab="branches" />} />
          <Route path="/ui/:owner/:repo/tags" element={<RepoDetailPage initialTab="tags" />} />
          <Route path="/ui/:owner/:repo/activity" element={<RepoDetailPage initialTab="activity" />} />
          <Route path="/ui/:owner/:repo/commits/:sha" element={<RepoCommitPage />} />
          <Route path="/ui/:owner/:repo/tree/:ref/*" element={<RepoDetailPage />} />
          <Route path="/ui/:owner/:repo/blob/:ref/*" element={<RepoFilePage />} />
          <Route path="/ui/:owner/:repo/compare/:range" element={<RepoComparePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const repoData = {
  id: 1,
  name: "test",
  full_name: "admin/test",
  description: "a repo",
  homepage: "https://example.com",
  default_branch: "main",
  visibility: "public",
  private: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
  pushed_at: "2026-01-02T00:00:00Z",
  stargazers_count: 5,
  subscribers_count: 2,
  forks_count: 1,
  ssh_url: "git@bleephub.example:admin/test.git",
  owner: { login: "admin", type: "User" },
  permissions: { admin: true, push: true, pull: true },
};

const topicsData = { names: ["cli", "tooling"] };

const releasesData = [
  {
    id: 1,
    tag_name: "v1.0.0",
    name: "First release",
    body: "",
    draft: false,
    prerelease: false,
    created_at: "2026-02-01T00:00:00Z",
    published_at: "2026-02-01T00:00:00Z",
    html_url: "http://x/admin/test/releases/tag/v1.0.0",
  },
  {
    id: 2,
    tag_name: "v1.1.0",
    name: "Draft release",
    body: "",
    draft: true,
    prerelease: false,
    created_at: "2026-03-01T00:00:00Z",
    published_at: null,
    html_url: "http://x/admin/test/releases/tag/v1.1.0",
  },
];

const branchesData = [{ name: "main", commit: { sha: "abc" } }];
const commitsData = [
  {
    sha: "abc123",
    commit: {
      message: "Initial commit",
      author: { name: "Admin", email: "a@b", date: "2026-01-01T00:00:00Z" },
    },
  },
];
const contentsData = [
  { name: "README.md", path: "README.md", sha: "r1", type: "file", size: 14 },
  { name: "src", path: "src", sha: "d1", type: "dir" },
];
const readmeData = {
  name: "README.md",
  path: "README.md",
  sha: "r1",
  type: "file",
  encoding: "base64",
  content: "IyB0ZXN0CgpuZXh0cmEgZGV0YWls",
};

function routedFetch(url: RequestInfo | URL): Promise<Response> {
  const u = url.toString();
  if (u.endsWith("/commits/abc123")) {
    return Promise.resolve(jsonResponse({
      ...commitsData[0],
      stats: { additions: 2, deletions: 0, total: 2 },
      files: [{
        sha: "r1",
        filename: "README.md",
        status: "added",
        additions: 2,
        deletions: 0,
        changes: 2,
        patch: "@@ -0,0 +1,2 @@\n+# test\n+extra detail",
      }],
    }));
  }
  if (u.includes("/releases")) return Promise.resolve(jsonResponse(releasesData));
  if (u.endsWith("/topics")) return Promise.resolve(jsonResponse(topicsData));
  if (u.endsWith("/packages")) return Promise.resolve(jsonResponse([]));
  if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(repoData));
  if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
  if (u.includes("/commits?")) return Promise.resolve(jsonResponse(commitsData));
  if (u.includes("/readme")) return Promise.resolve(jsonResponse(readmeData));
  if (u.includes("/contents/README.md")) return Promise.resolve(jsonResponse(readmeData));
  if (u.includes("/contents/")) return Promise.resolve(jsonResponse(contentsData));
  return Promise.resolve(jsonResponse([]));
}

// G9 removed the in-page Releases sub-tab; RepoDetailPage now only leads to the
// routed ReleasesPage from the About sidebar, the way github.com does.
describe("RepoDetailPage releases entry point", () => {
  it("links the About sidebar's Releases heading at the routed releases page", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    const about = await screen.findByRole("complementary", { name: "About" });
    expect(within(about).getByRole("link", { name: "Releases" })).toHaveAttribute(
      "href",
      "/ui/admin/test/releases",
    );
  });
});

describe("RepoDetailPage code", () => {
  it("renders the file tree and README for a non-empty repo", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findByText("a repo");

    await waitFor(() => {
      // README.md appears both as a file row and the README panel header.
      expect(screen.getAllByText("README.md").length).toBeGreaterThan(0);
      expect(screen.getByText("src")).toBeInTheDocument();
    });
    // "test" appears in the repo breadcrumb and the rendered README <h1>.
    expect(screen.getAllByText("test").length).toBeGreaterThan(0);
  });

  it("hides the top-level README when browsing a subdirectory", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    // "nextra detail" is text unique to the rendered README markdown body.
    expect(await screen.findByText("nextra detail")).toBeInTheDocument();

    // Readme cache is keyed by branch not path, so the panel must gate on path === "".
    fireEvent.click(screen.getByRole("link", { name: "src" }));
    await waitFor(() => {
      expect(screen.queryByText("nextra detail")).not.toBeInTheDocument();
    });
  });

  it("shows only supported empty-repository transport setup", async () => {
    const calls: string[] = [];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      calls.push(u);
      if (u.endsWith("/repos/admin/test")) {
        return Promise.resolve(jsonResponse({ ...repoData, pushed_at: null, ssh_url: "" }));
      }
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await screen.findByText("a repo");

    await waitFor(() => {
      expect(screen.getByText("This repository is empty")).toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: "HTTPS" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "SSH" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "GitHub CLI" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "SSH" }));
    expect(screen.getByRole("note")).toHaveTextContent(/SSH cloning is not enabled/i);
    expect(calls.some((url) => url.includes("/commits?"))).toBe(false);
  });

  it("renders the latest-commit banner above the file table", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/Initial commit/)).toBeInTheDocument();
    });
    expect(screen.getByText("abc123".slice(0, 7))).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /1 commit\b/ })).toHaveAttribute(
      "href",
      "/ui/admin/test/commits",
    );
    expect(screen.getByRole("link", { name: "abc123" })).toHaveAttribute(
      "href",
      "/ui/admin/test/commits/abc123",
    );
  });

  it("exposes a Code clone dropdown with the HTTPS clone URL and a copy button", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findAllByText("README.md");

    // Match the clone dropdown, not the sub-tab "Code" button, via aria-expanded.
    const codeButton = screen.getByRole("button", { name: "Code", expanded: false });
    fireEvent.click(codeButton);

    const field = screen.getByLabelText("HTTPS clone URL") as HTMLInputElement;
    expect(field.value).toMatch(/\/admin\/test\.git$/);
    expect(screen.getByRole("button", { name: "Copy clone URL" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "SSH" }));
    expect(screen.getByLabelText("SSH clone URL")).toHaveValue(
      "git@bleephub.example:admin/test.git",
    );
  });

  it("links files and commits to navigable detail journeys", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findAllByText("README.md");

    expect(screen.getAllByRole("link", { name: "README.md" })[0]).toHaveAttribute(
      "href",
      "/ui/admin/test/blob/main/README.md",
    );
    // G9: reach commit history from the tree header's "N commits" link, not a sub-tab.
    fireEvent.click(screen.getByRole("link", { name: /\d+\+? commits?$/ }));
    expect(await screen.findByRole("link", { name: "Initial commit" })).toHaveAttribute(
      "href",
      "/ui/admin/test/commits/abc123",
    );
  });

  it("loads a shareable repository tree at a commit and path", async () => {
    const calls: string[] = [];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      calls.push(url.toString());
      return routedFetch(url);
    });
    renderPage("/ui/admin/test/tree/abc123/src");

    await screen.findAllByText("src");
    expect(calls).toContain(
      "/api/v3/repos/admin/test/contents/src?ref=abc123",
    );
    expect(calls).toContain(
      "/api/v3/repos/admin/test/commits?per_page=100&sha=abc123",
    );
  });

  it("drives Watch, Fork, and Star through the public GitHub repository APIs", async () => {
    let starred = false;
    let subscribed = false;
    const calls: Array<{ method: string; url: string }> = [];
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      const method = init?.method ?? "GET";
      calls.push({ method, url: u });
      if (u === "/api/v3/user") {
        return Promise.resolve(jsonResponse({ id: 9, login: "octocat", type: "User", site_admin: false }));
      }
      if (u.startsWith("/api/v3/user/orgs")) {
        return Promise.resolve(jsonResponse([{ id: 10, login: "acme" }]));
      }
      if (u.endsWith("/ui-data/repos/admin/test/viewer")) {
        return Promise.resolve(jsonResponse({ starred, subscribed }));
      }
      if (u.endsWith("/user/starred/admin/test")) {
        if (method === "PUT") {
          starred = true;
          return Promise.resolve(new Response(null, { status: 204 }));
        }
        return Promise.resolve(new Response(null, { status: starred ? 204 : 404 }));
      }
      if (u.endsWith("/repos/admin/test/subscription")) {
        if (method === "PUT") subscribed = true;
        return Promise.resolve(jsonResponse({
          subscribed,
          ignored: false,
          reason: null,
          created_at: "2026-01-01T00:00:00Z",
          url: u,
          repository_url: "/api/v3/repos/admin/test",
        }));
      }
      if (u.endsWith("/repos/admin/test/forks") && method === "POST") {
        return Promise.resolve(jsonResponse({ ...repoData, id: 2, full_name: "octocat/test", owner: { login: "octocat" } }, 202));
      }
      return routedFetch(url);
    });
    renderPage();

    const actions = await screen.findByLabelText("Repository actions");
    fireEvent.click(within(actions).getByRole("button", { name: /Watch/ }));
    await waitFor(() => expect(calls).toContainEqual({ method: "PUT", url: "/api/v3/repos/admin/test/subscription" }));

    fireEvent.click(within(actions).getByRole("button", { name: /Star/ }));
    await waitFor(() => expect(calls).toContainEqual({ method: "PUT", url: "/api/v3/user/starred/admin/test" }));
    await waitFor(() => expect(within(actions).getByRole("button", { name: /Unstar/ })).toBeInTheDocument());

    fireEvent.click(within(actions).getByRole("button", { name: /Fork/ }));
    const forkDialog = await screen.findByRole("dialog", { name: "Create a new fork" });
    await within(forkDialog).findByRole("option", { name: "acme" });
    fireEvent.change(within(forkDialog).getByLabelText("Owner"), { target: { value: "acme" } });
    fireEvent.click(within(forkDialog).getByRole("button", { name: "Create fork" }));
    await waitFor(() => expect(calls).toContainEqual({ method: "POST", url: "/api/v3/repos/admin/test/forks" }));
    expect(calls.some(({ url }) => url.startsWith("/internal/"))).toBe(false);
  });

  // G9: pins the old second tab row's absence plus the github.com-shaped entry
  // point that replaced each of its items.
  it("has no second repository tab row, and reaches every destination the way github.com does", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findAllByText("README.md");

    expect(screen.queryByRole("navigation", { name: "Repository content" })).not.toBeInTheDocument();
    expect(screen.queryByText("Repository administration")).not.toBeInTheDocument();
    expect(screen.queryByText("All repository settings")).not.toBeInTheDocument();

    // Code: the one remaining repository tab row.
    const repoTabs = screen.getByRole("navigation", { name: "Repository" });
    expect(within(repoTabs).getByRole("link", { name: "Code" })).toHaveAttribute(
      "href",
      "/ui/admin/test",
    );
    // Settings: still on that row for an admin (webhooks/secrets/environments live behind it).
    expect(within(repoTabs).getByRole("link", { name: "Settings" })).toHaveAttribute(
      "href",
      "/ui/admin/test/settings",
    );

    expect(screen.getByRole("link", { name: /\d+\+? commits?$/ })).toHaveAttribute(
      "href",
      "/ui/admin/test/commits",
    );

    // Branches and Tags: the branch/tag switcher's own footer.
    fireEvent.click(screen.getByRole("button", { name: "Switch branches or tags" }));
    expect(screen.getByRole("link", { name: "View all branches" })).toHaveAttribute(
      "href",
      "/ui/admin/test/branches",
    );
    fireEvent.click(screen.getByRole("tab", { name: "Tags" }));
    expect(screen.getByRole("link", { name: "View all tags" })).toHaveAttribute(
      "href",
      "/ui/admin/test/tags",
    );

    // Activity and Releases: the About sidebar.
    const about = screen.getByRole("complementary", { name: "About" });
    expect(within(about).getByRole("link", { name: "Activity" })).toHaveAttribute(
      "href",
      "/ui/admin/test/activity",
    );
    expect(within(about).getByRole("link", { name: "Releases" })).toHaveAttribute(
      "href",
      "/ui/admin/test/releases",
    );
    // Deployments hung off the removed row's overflow; the sidebar is its only entry point now.
    expect(within(about).getByRole("link", { name: "Deployments" })).toHaveAttribute(
      "href",
      "/ui/admin/test/deployments",
    );
  });
});

describe("RepoDetailPage About sidebar", () => {
  it("renders description, website, topics, releases, packages and social counts", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findAllByText("README.md");

    const about = screen.getByRole("complementary", { name: "About" });
    expect(within(about).getByText("a repo")).toBeInTheDocument();
    expect(within(about).getByText("example.com")).toBeInTheDocument();
    expect(within(about).getByText("cli")).toBeInTheDocument();
    expect(within(about).getByText("tooling")).toBeInTheDocument();
    expect(within(about).getByText("Latest")).toBeInTheDocument();
    expect(within(about).getByText("No packages published")).toBeInTheDocument();
    expect(within(about).getByText(/5 stars/)).toBeInTheDocument();
    expect(within(about).getByText(/2 watchers/)).toBeInTheDocument();
    expect(within(about).getByText(/1 fork/)).toBeInTheDocument();
  });

  it("renders the license name when the repo has one", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/test")) {
        return Promise.resolve(jsonResponse({
          ...repoData,
          license: { key: "mit", name: "MIT License", spdx_id: "MIT", url: "" },
        }));
      }
      return routedFetch(url);
    });
    renderPage();
    const about = await screen.findByRole("complementary", { name: "About" });
    expect(within(about).getByText("MIT License")).toBeInTheDocument();
  });

  it("styles README headings via the markdown-body class", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    const { container } = renderPage();
    await waitFor(() => {
      // README markdown "# test" renders as a real <h1> inside .markdown-body
      const heading = container.querySelector(".markdown-body h1");
      expect(heading).not.toBeNull();
      expect(heading?.textContent).toBe("test");
    });
  });
});

describe("repository detail journeys", () => {
  it("renders a commit with its summary and patch", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/admin/test/commits/abc123");

    expect(await screen.findByRole("heading", { name: "Initial commit" })).toBeInTheDocument();
    expect(screen.getByText("2 changes")).toBeInTheDocument();
    expect(screen.getByText(/extra detail/)).toBeInTheDocument();
  });

  it("renders the commit's parents with links", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/commits/abc123")) {
        return Promise.resolve(jsonResponse({
          ...commitsData[0],
          stats: { additions: 2, deletions: 0, total: 2 },
          files: [],
          parents: [{ sha: "9999999abcdef", url: "", html_url: "" }],
        }));
      }
      return routedFetch(url);
    });
    renderPage("/ui/admin/test/commits/abc123");
    expect(await screen.findByText("1 parent")).toBeInTheDocument();
    expect(screen.getByText("9999999")).toBeInTheDocument();
  });

  it("adds a commit comment via POST /commits/{sha}/comments", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/commits/abc123/comments") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({ id: 1, body: "nice", user: { login: "admin" }, created_at: "2026-01-01T00:00:00Z" }, 201),
        );
      }
      return routedFetch(url);
    });
    renderPage("/ui/admin/test/commits/abc123");

    fireEvent.change(await screen.findByLabelText("commit comment"), { target: { value: "nice" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/commits/abc123/comments") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ body: "nice" });
    });
  });

  it("creates a commit status via POST /statuses/{sha}", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/statuses/abc123") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({ context: "ci/build", state: "success", description: null, target_url: null }, 201),
        );
      }
      if (u.endsWith("/commits/abc123/status")) {
        return Promise.resolve(jsonResponse({ state: "pending", sha: "abc123", total_count: 0, statuses: [] }));
      }
      return routedFetch(url);
    });
    renderPage("/ui/admin/test/commits/abc123");

    fireEvent.change(await screen.findByLabelText("status context"), { target: { value: "ci/build" } });
    fireEvent.click(screen.getByRole("button", { name: "Create status" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/statuses/abc123") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ state: "success", context: "ci/build" });
    });
  });

  it("adds a reaction to a commit comment via POST /comments/{id}/reactions", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/comments/50/reactions") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({ id: 9, content: "heart", user: { login: "admin" }, created_at: "2026-01-01T00:00:00Z" }, 201),
        );
      }
      if (u.endsWith("/comments/50/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/commits/abc123/comments")) {
        return Promise.resolve(
          jsonResponse([{ id: 50, body: "hi", user: { login: "bob" }, created_at: "2026-01-01T00:00:00Z" }]),
        );
      }
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      return routedFetch(url);
    });
    renderPage("/ui/admin/test/commits/abc123");

    fireEvent.click(await screen.findByRole("button", { name: "add reaction" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "react with heart" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/comments/50/reactions") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ content: "heart" });
    });
  });

  it("renders associated pull requests for a commit with links", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/commits/abc123/pulls")) {
        return Promise.resolve(
          jsonResponse([
            {
              number: 7,
              title: "Add feature",
              state: "open",
              merged_at: null,
              html_url: "http://x/admin/test/pull/7",
              user: { login: "admin" },
            },
          ]),
        );
      }
      return routedFetch(url);
    });
    renderPage("/ui/admin/test/commits/abc123");

    const link = await screen.findByRole("link", { name: /#7 Add feature/ });
    expect(link).toHaveAttribute("href", "/ui/admin/test/pulls/7");
  });

  it("renders a repository file at its durable URL with Raw + History controls", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/admin/test/blob/main/README.md");

    const crumbs = await screen.findByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumbs).getByRole("link", { name: "test" })).toHaveAttribute(
      "href",
      "/ui/admin/test/tree/main",
    );
    expect(within(crumbs).getByText("README.md")).toHaveAttribute("aria-current", "page");
    expect(await screen.findByText(/extra detail/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "View raw file" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "History" })).toHaveAttribute(
      "href",
      "/ui/admin/test/commits?path=README.md&sha=main",
    );
  });

  it("syncs a fork via POST /merge-upstream", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/merge-upstream") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({ message: "Successfully fetched and fast-forwarded", merge_type: "fast-forward", base_branch: "main" }),
        );
      }
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse({ ...repoData, fork: true }));
      return routedFetch(url);
    });
    renderPage("/ui/admin/test");

    fireEvent.click(await screen.findByRole("button", { name: "Sync fork" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/merge-upstream") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ branch: "main" });
    });
  });
});

describe("RepoDetailPage activity", () => {
  it("renders the activity feed and fires GET /activity", async () => {
    const calls: string[] = [];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      calls.push(u);
      if (u.endsWith("/repos/admin/test/activity")) {
        return Promise.resolve(jsonResponse([
          {
            id: 7,
            activity_type: "push",
            ref: "refs/heads/main",
            before: "0000000",
            after: "abc1234",
            timestamp: "2026-01-02T00:00:00Z",
            actor: { login: "octocat", avatar_url: "" },
          },
        ]));
      }
      return routedFetch(url);
    });
    renderPage("/ui/admin/test/activity");

    expect(await screen.findByText("octocat")).toBeInTheDocument();
    expect(screen.getByText("pushed to")).toBeInTheDocument();
    expect(screen.getByText("main")).toBeInTheDocument();
    expect(calls.some((u) => u.endsWith("/api/v3/repos/admin/test/activity"))).toBe(true);
  });
});

describe("RepoDetailPage file editing", () => {
  it("edits a file through the contents API", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/admin/test/blob/main/README.md");
    fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
    const editor = await screen.findByLabelText(/edit README\.md/i);
    fireEvent.change(editor, { target: { value: "# changed" } });
    fireEvent.click(screen.getByRole("button", { name: /commit changes/i }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/contents/README.md") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      const body = JSON.parse((put![1] as RequestInit).body as string);
      expect(body.sha).toBe("r1");
      expect(body.branch).toBe("main");
      expect(typeof body.content).toBe("string");
    });
  });

  it("deletes a file after confirmation", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/admin/test/blob/main/README.md");
    fireEvent.click(await screen.findByRole("button", { name: /delete file/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^delete$/i }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/contents/README.md") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("creates a new file from the code view", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/admin/test");
    fireEvent.click(await screen.findByRole("button", { name: /add file/i }));
    fireEvent.change(await screen.findByLabelText(/file path/i), { target: { value: "NEW.md" } });
    fireEvent.change(screen.getByLabelText(/contents/i), { target: { value: "hello" } });
    fireEvent.click(screen.getByRole("button", { name: /commit new file/i }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/contents/NEW.md") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      const body = JSON.parse((put![1] as RequestInit).body as string);
      expect(body.branch).toBe("main");
      expect(body.sha).toBeUndefined();
    });
  });
});

describe("RepoDetailPage refs", () => {
  it("creates a branch from the branches tab", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/admin/test/branches");
    fireEvent.click(await screen.findByRole("button", { name: /new branch/i }));
    fireEvent.change(await screen.findByLabelText(/branch name/i), { target: { value: "feature/x" } });
    fireEvent.click(screen.getByRole("button", { name: /create branch/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/git/refs") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse((post![1] as RequestInit).body as string)).toEqual({
        ref: "refs/heads/feature/x",
        sha: "abc",
      });
    });
  });

  it("deletes a non-default branch via DELETE /git/refs/heads/{branch}", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/git/refs/heads/feature/y") && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.split("?")[0]!.endsWith("/branches")) {
        return Promise.resolve(jsonResponse([
          { name: "main", commit: { sha: "abc" } },
          { name: "feature/y", commit: { sha: "def" } },
        ]));
      }
      return routedFetch(url);
    });
    renderPage("/ui/admin/test/branches");
    fireEvent.click(await screen.findByRole("button", { name: "Delete branch feature/y" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/git/refs/heads/feature/y") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("creates a tag from the tags tab", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/admin/test/tags");
    fireEvent.click(await screen.findByRole("button", { name: /new tag/i }));
    fireEvent.change(await screen.findByLabelText(/tag name/i), { target: { value: "v1.0.0" } });
    fireEvent.click(screen.getByRole("button", { name: /create tag/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/git/refs") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse((post![1] as RequestInit).body as string)).toEqual({
        ref: "refs/tags/v1.0.0",
        sha: "abc",
      });
    });
  });

  it("renders tag rows with commit date, sha link, archives and a matching release link", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.split("?")[0]!.endsWith("/repos/admin/tagrepo/tags")) {
        return Promise.resolve(jsonResponse([
          { name: "v1.0.0", commit: { sha: "tagsha1234567", url: "" }, zipball_url: "/z", tarball_url: "/t" },
        ]));
      }
      if (u.endsWith("/commits/tagsha1234567")) {
        return Promise.resolve(jsonResponse({
          sha: "tagsha1234567",
          commit: { message: "cut release", author: { name: "Admin", date: "2026-08-01T00:00:00Z" } },
        }));
      }
      if (u.endsWith("/repos/admin/tagrepo")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/tagrepo" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.split("?")[0]!.endsWith("/repos/admin/tagrepo/releases")) return Promise.resolve(jsonResponse(releasesData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/tagrepo/tags");

    expect(await screen.findByText("v1.0.0")).toBeInTheDocument();
    // sha column links to the commit page
    expect(await screen.findByRole("link", { name: "tagsha1" })).toHaveAttribute(
      "href",
      "/ui/admin/tagrepo/commits/tagsha1234567",
    );
    // creation date resolved from the tagged commit
    await waitFor(() => {
      expect(screen.getByRole("link", { name: "tagsha1" }).closest("div")!.querySelector("time")).not.toBeNull();
    });
    // a release whose tag_name matches links through
    expect(screen.getByRole("link", { name: "Release" })).toHaveAttribute(
      "href",
      "/ui/admin/tagrepo/releases/1",
    );
    expect(screen.getByRole("link", { name: "zip" })).toHaveAttribute("href", "/z");
  });

  it("generates a repository from a template via POST .../generate", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/test/generate") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/generated" }, 201));
      }
      if (u.endsWith("/repos/admin/test")) {
        return Promise.resolve(jsonResponse({ ...repoData, is_template: true }));
      }
      return routedFetch(url);
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Use this template" }));
    fireEvent.change(await screen.findByLabelText("Repository name"), { target: { value: "generated" } });
    fireEvent.click(screen.getByRole("button", { name: "Create repository" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/repos/admin/test/generate") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse((post![1] as RequestInit).body as string)).toEqual({
        name: "generated",
        description: "",
        include_all_branches: false,
        private: false,
      });
    });
  });
});

describe("RepoDetailPage file table metadata", () => {
  it("shows each file's latest commit message and age, per-path", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/filetab")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/filetab" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.includes("/commits?") && u.includes("path=README.md")) {
        return Promise.resolve(jsonResponse([
          { sha: "aaa1111", commit: { message: "touch readme", author: { name: "Admin", date: "2026-08-01T00:00:00Z" } } },
        ]));
      }
      if (u.includes("/commits?") && u.includes("path=src")) {
        return Promise.resolve(jsonResponse([
          { sha: "bbb2222", commit: { message: "add src", author: { name: "Admin", date: "2026-07-01T00:00:00Z" } } },
        ]));
      }
      if (u.includes("/commits?")) return Promise.resolve(jsonResponse(commitsData));
      if (u.includes("/readme")) return Promise.resolve(jsonResponse(readmeData));
      if (u.includes("/contents/")) return Promise.resolve(jsonResponse(contentsData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/filetab");

    // Each row carries the message of the last commit touching that path.
    const readmeMsg = await screen.findByRole("link", { name: "touch readme" });
    expect(readmeMsg).toHaveAttribute("href", "/ui/admin/filetab/commits/aaa1111");
    expect(await screen.findByRole("link", { name: "add src" })).toHaveAttribute(
      "href",
      "/ui/admin/filetab/commits/bbb2222",
    );
    await waitFor(() => {
      expect(readmeMsg.closest("div[class*='flex']")!.parentElement!.querySelector("time")).not.toBeNull();
    });
  });

  it("shows a path-scoped latest-commit banner with a History link in subdirectories", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/subdir")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/subdir" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.includes("/commits?") && u.includes("path=src")) {
        return Promise.resolve(jsonResponse([
          { sha: "ccc3333", commit: { message: "src only change", author: { name: "Admin", date: "2026-08-02T00:00:00Z" } } },
        ]));
      }
      if (u.includes("/commits?")) return Promise.resolve(jsonResponse(commitsData));
      if (u.includes("/contents/")) return Promise.resolve(jsonResponse(contentsData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/subdir/tree/main/src");

    // The banner surfaces the directory's own latest commit, not the repo's.
    expect(await screen.findByText(/src only change/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /History/ })).toHaveAttribute(
      "href",
      "/ui/admin/subdir/commits?path=src",
    );
  });
});

describe("RepoDetailPage ref switcher", () => {
  it("filters branches and switches to a tag from the Tags tab", async () => {
    const calls: string[] = [];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      calls.push(u);
      if (u.endsWith("/repos/admin/refsw")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/refsw" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.split("?")[0]!.endsWith("/tags")) {
        return Promise.resolve(jsonResponse([
          { name: "v9.9.9", commit: { sha: "abc", url: "" }, zipball_url: "", tarball_url: "" },
        ]));
      }
      if (u.includes("/commits?")) return Promise.resolve(jsonResponse(commitsData));
      if (u.includes("/readme")) return Promise.resolve(jsonResponse(readmeData));
      if (u.includes("/contents/")) return Promise.resolve(jsonResponse(contentsData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/refsw");

    const trigger = await screen.findByRole("button", { name: "Switch branches or tags" });
    expect(trigger).toHaveTextContent("main");
    fireEvent.click(trigger);

    const input = screen.getByRole("combobox", { name: /find a branch/i });
    fireEvent.change(input, { target: { value: "ma" } });
    expect(screen.getByRole("option", { name: /main/ })).toBeInTheDocument();
    // The filter carries across tabs (GitHub keeps it); clear it first.
    fireEvent.change(input, { target: { value: "" } });

    // The Tags tab loads its list lazily and navigates on selection.
    fireEvent.click(screen.getByRole("tab", { name: "Tags" }));
    const tagOption = await screen.findByRole("option", { name: /v9\.9\.9/ });
    fireEvent.click(tagOption);
    await waitFor(() => {
      expect(calls.some((u) => u.includes("/contents/") && u.includes("ref=v9.9.9"))).toBe(true);
    });
  });
});

describe("RepoDetailPage commits grouping", () => {
  it("groups commits under 'Commits on {date}' with copy + browse affordances", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/grouped")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/grouped" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.includes("/commits?")) {
        return Promise.resolve(jsonResponse([
          { sha: "sha1111111", commit: { message: "second", author: { name: "Admin", date: "2026-08-02T10:00:00Z" } }, author: { login: "admin", avatar_url: "" } },
          { sha: "sha2222222", commit: { message: "first", author: { name: "Admin", date: "2026-08-02T08:00:00Z" } }, author: { login: "admin", avatar_url: "" } },
          { sha: "sha3333333", commit: { message: "older", author: { name: "Admin", date: "2026-07-30T08:00:00Z" } }, author: { login: "admin", avatar_url: "" } },
        ]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/grouped/commits");

    await screen.findByRole("link", { name: "second" });
    expect(screen.getAllByText(/^Commits on /)).toHaveLength(2);
    expect(screen.getByRole("button", { name: "Copy full SHA for sha1111" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Browse repository at sha1111" })).toHaveAttribute(
      "href",
      "/ui/admin/grouped/tree/sha1111111",
    );
    expect(screen.getByRole("link", { name: "second" }).closest("div")!.parentElement!.querySelector("time")).not.toBeNull();
  });
});

describe("RepoDetailPage branches sections", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("buckets branches into Default/Active/Stale with ahead-behind and a New PR link", async () => {
    // Pin the clock: stale-branch bucketing compares against "now", so fixture dates
    // must be fixed relative to it (shouldAdvanceTime keeps waitFor's real timers working).
    vi.useFakeTimers({ now: new Date("2026-03-01T12:00:00Z"), shouldAdvanceTime: true });
    const now = new Date("2026-03-01T12:00:00Z").getTime();
    const recent = new Date(now - 5 * 24 * 3600 * 1000).toISOString();
    const ancient = new Date(now - 200 * 24 * 3600 * 1000).toISOString();
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/brepo")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/brepo" }));
      if (u.split("?")[0]!.endsWith("/branches")) {
        return Promise.resolve(jsonResponse([
          { name: "main", commit: { sha: "mmm" } },
          { name: "fresh", commit: { sha: "fff" } },
          { name: "old", commit: { sha: "ooo" } },
        ]));
      }
      if (u.endsWith("/commits/mmm") || u.endsWith("/commits/fff")) {
        return Promise.resolve(jsonResponse({
          sha: "x", commit: { message: "m", author: { name: "Admin", date: recent } }, author: { login: "admin", avatar_url: "" },
        }));
      }
      if (u.endsWith("/commits/ooo")) {
        return Promise.resolve(jsonResponse({
          sha: "y", commit: { message: "m", author: { name: "Bob", date: ancient } }, author: { login: "bob", avatar_url: "" },
        }));
      }
      if (u.includes("/compare/main...fresh")) {
        return Promise.resolve(jsonResponse({ status: "ahead", ahead_by: 3, behind_by: 1, total_commits: 3, commits: [] }));
      }
      if (u.includes("/compare/main...old")) {
        return Promise.resolve(jsonResponse({ status: "behind", ahead_by: 0, behind_by: 9, total_commits: 0, commits: [] }));
      }
      if (u.includes("/commits?")) return Promise.resolve(jsonResponse(commitsData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/brepo/branches");

    // stale = no commit in 3 months
    await screen.findByText("Stale branches");
    expect(screen.getByText("Default")).toBeInTheDocument();
    expect(screen.getByText("Active branches")).toBeInTheDocument();
    const stale = screen.getByRole("region", { name: "Stale branches" });
    expect(within(stale).getByText("old")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText("3 ahead · 1 behind")).toBeInTheDocument();
    });
    expect(screen.getAllByRole("link", { name: "New pull request" })[0]).toHaveAttribute(
      "href",
      "/ui/admin/brepo/compare/main...fresh",
    );
    expect(within(stale).getByText("bob")).toBeInTheDocument();
  });
});

describe("RepoComparePage", () => {
  it("offers Create pull request and renders a colored diff", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/ctest")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/ctest" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.includes("/compare/main...feature")) {
        return Promise.resolve(jsonResponse({
          status: "ahead",
          ahead_by: 1,
          behind_by: 0,
          total_commits: 1,
          commits: [{ sha: "cmp1111111", commit: { message: "compared", author: { name: "Admin", date: "2026-08-01T00:00:00Z" } } }],
          files: [{ filename: "a.txt", additions: 1, deletions: 0, changes: 1, status: "modified", patch: "@@ -1 +1,2 @@\n line\n+added line" }],
        }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/ctest/compare/main...feature");

    const createPr = await screen.findByRole("link", { name: "Create pull request" });
    // Contract for the pulls page create flow: ?compare={base}...{head}.
    expect(createPr).toHaveAttribute("href", "/ui/admin/ctest/pulls?compare=main...feature");
    // Diff rows are colored (color-mix), not a monochrome <pre>.
    const hunkRow = screen.getByText("@@ -1 +1,2 @@").parentElement!;
    expect(hunkRow.getAttribute("style")).toMatch(/color-mix/);
    const addedRow = screen.getByText("+added line").parentElement!;
    expect(addedRow.getAttribute("style")).toMatch(/color-mix/);
  });
});

describe("RepoFilePage content types", () => {
  it("renders code blobs with linkable line-number anchors", async () => {
    const code = "const a = 1;\nconst b = 2;\nexport { a, b };";
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/blobber")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/blobber" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.includes("/contents/src/app.ts")) {
        return Promise.resolve(jsonResponse({
          name: "app.ts", path: "src/app.ts", sha: "blob1", type: "file", size: code.length,
          encoding: "base64", content: btoa(code),
        }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    const { container } = renderPage("/ui/admin/blobber/blob/main/src/app.ts");

    const line2 = await screen.findByRole("link", { name: "Line 2" });
    expect(line2).toHaveAttribute("href", "#L2");
    expect(container.querySelector("#L1")).not.toBeNull();
    expect(container.querySelector("#L3")).not.toBeNull();
    fireEvent.click(line2);
    await waitFor(() => {
      expect((container.querySelector("#L2") as HTMLElement).getAttribute("style")).toMatch(/color-mix/);
    });
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumbs).getByRole("link", { name: "src" })).toHaveAttribute(
      "href",
      "/ui/admin/blobber/tree/main/src",
    );
  });

  it("renders markdown blobs with a Preview default and a Code tab", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/admin/test/blob/main/README.md");

    // Preview is the default: markdown renders as HTML.
    expect(await screen.findByRole("heading", { name: "test" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Code" }));
    expect(await screen.findByRole("link", { name: "Line 1" })).toBeInTheDocument();
  });

  it("renders image blobs as an <img> from a data: URI", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/imager")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/imager" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.includes("/contents/logo.png")) {
        return Promise.resolve(jsonResponse({
          name: "logo.png", path: "logo.png", sha: "img1", type: "file", size: 8,
          encoding: "base64", content: "iVBORw0KGgo=",
        }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/imager/blob/main/logo.png");

    const img = await screen.findByRole("img", { name: "logo.png" });
    expect(img.getAttribute("src")).toBe("data:image/png;base64,iVBORw0KGgo=");
  });

  it("shows a binary fallback instead of mojibake, hiding text-only controls", async () => {
    // 0xFF 0xFE 0x00 is not valid UTF-8.
    const binary = btoa(String.fromCharCode(0xff, 0xfe, 0x00));
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/binrepo")) return Promise.resolve(jsonResponse({ ...repoData, full_name: "admin/binrepo" }));
      if (u.split("?")[0]!.endsWith("/branches")) return Promise.resolve(jsonResponse(branchesData));
      if (u.includes("/contents/data.bin")) {
        return Promise.resolve(jsonResponse({
          name: "data.bin", path: "data.bin", sha: "bin1", type: "file", size: 3,
          encoding: "base64", content: binary,
        }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderPage("/ui/admin/binrepo/blob/main/data.bin");

    expect(await screen.findByText(/Binary file not shown/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View raw" })).toHaveAttribute("download", "data.bin");
    // Edit and Raw-text affordances hide for binary content.
    expect(screen.queryByRole("button", { name: "View raw file" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
  });
});

describe("RepoDetailPage contributors sidebar", () => {
  it("lists contributor avatars linking to their profiles and to insights", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/repos/admin/test/contributors")) {
        return Promise.resolve(jsonResponse([
          { login: "alice", avatar_url: "", type: "User", contributions: 9 },
          { login: "bob", avatar_url: "", type: "User", contributions: 3 },
        ]));
      }
      return routedFetch(url);
    });
    renderPage();

    const about = await screen.findByRole("complementary", { name: "About" });
    expect(await within(about).findByRole("link", { name: "alice" })).toHaveAttribute("href", "/ui/alice");
    expect(within(about).getByRole("link", { name: "Contributors 2" })).toHaveAttribute(
      "href",
      "/ui/admin/test/insights",
    );
  });
});

describe("RepoDetailPage bootstrap", () => {
  const bootstrapPayload = {
    repo: { ...repoData, topics: ["cli", "tooling"] },
    readme: readmeData,
    root_entries: contentsData,
    branches: { first_page: branchesData, total_count: 1 },
    tags: { first_page: [], total_count: 0 },
    languages: { Go: 100 },
    contributors: [{ login: "admin", avatar_url: "", type: "User", contributions: 3 }],
    latest_release: null,
    latest_commit: commitsData[0],
    pulls_open_count: 2,
    issues_open_count: 4,
    discussions_enabled: false,
  };
  const treeMetaPayload = {
    ref: "main",
    path: "",
    latest_commit: commitsData[0],
    entries: [
      {
        name: "README.md",
        path: "README.md",
        type: "file",
        size: 14,
        latest: {
          sha: "abc123",
          message_headline: "Initial commit",
          author_login: "admin",
          author_date: "2026-01-01T00:00:00Z",
        },
      },
      { name: "src", path: "src", type: "dir", size: 0, latest: null },
    ],
  };

  it("hydrates the repo home from the bootstrap + tree-meta with no standalone refetches", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/ui-data/bootstrap/repos/admin/test")) {
        return Promise.resolve(jsonResponse(bootstrapPayload));
      }
      if (u.includes("/tree-meta")) return Promise.resolve(jsonResponse(treeMetaPayload));
      if (u.includes("/commits?")) return Promise.resolve(jsonResponse(commitsData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await screen.findByText("a repo");
    // File-table columns come from tree-meta; the src row (latest: null) degrades
    // to the per-path commits fallback, also mocked.
    expect((await screen.findAllByText("Initial commit")).length).toBeGreaterThanOrEqual(1);
    expect(await screen.findByText("tooling")).toBeInTheDocument();

    const gets = mockFetch.mock.calls.map((c) => c[0]!.toString());
    // Seeded keys must be cache hits — their standalone endpoints untouched.
    expect(gets.some((u) => u.endsWith("/api/v3/repos/admin/test"))).toBe(false);
    expect(gets.some((u) => u.includes("/api/v3/repos/admin/test/branches"))).toBe(false);
    expect(gets.some((u) => u.includes("/readme"))).toBe(false);
    expect(gets.some((u) => u.includes("/contents/"))).toBe(false);
    expect(gets.some((u) => u.includes("/languages"))).toBe(false);
    expect(gets.some((u) => u.includes("/contributors"))).toBe(false);
    expect(gets.some((u) => u.includes("/topics"))).toBe(false);
    // Tab badges come from the bootstrap's counts, not the list endpoints.
    expect(gets.some((u) => u.includes("/issues?state=open"))).toBe(false);
    expect(gets.some((u) => u.includes("/pulls?state=open"))).toBe(false);
  });

  it("falls back to the standalone endpoints when the bootstrap answers 500", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/ui-data/")) return Promise.resolve(jsonResponse({ message: "boom" }, 500));
      return routedFetch(url);
    });
    renderPage();
    await screen.findByText("a repo");
    // Per-row latest-commit columns still fill in via the per-path fallback.
    expect((await screen.findAllByText("Initial commit")).length).toBeGreaterThanOrEqual(1);
    const gets = mockFetch.mock.calls.map((c) => c[0]!.toString());
    expect(gets.some((u) => u.endsWith("/api/v3/repos/admin/test"))).toBe(true);
    expect(gets.some((u) => u.split("?")[0]!.endsWith("/branches"))).toBe(true);
  });
});

describe("RepoDetailPage read-only viewer gating", () => {
  // A pull-only outsider: github.com hides every write affordance rather than
  // render buttons that would 403.
  const viewerRepo = { ...repoData, permissions: { admin: false, push: false, pull: true } };
  const viewerFetch = (url: RequestInfo | URL): Promise<Response> => {
    const u = url.toString();
    if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(viewerRepo));
    return routedFetch(url);
  };

  it("hides Add file/Upload files, the Settings tab and the admin menu from a viewer", async () => {
    mockFetch.mockImplementation(viewerFetch);
    renderPage();
    await screen.findAllByText("README.md");

    expect(screen.getByRole("button", { name: "Go to file" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Code", expanded: false })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add file" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Upload files" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
    expect(screen.queryByText("All repository settings")).not.toBeInTheDocument();
    expect(screen.queryByText("Repository administration")).not.toBeInTheDocument();
  });

  it("hides branch create/delete and downgrades the protected badge for a viewer", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.split("?")[0]!.endsWith("/branches")) {
        return Promise.resolve(jsonResponse([
          { name: "main", commit: { sha: "abc" } },
          { name: "feature/y", commit: { sha: "def" } },
          { name: "release", protected: true, commit: { sha: "eee" } },
        ]));
      }
      return viewerFetch(url);
    });
    renderPage("/ui/admin/test/branches");
    await screen.findByText("feature/y");

    expect(screen.queryByRole("button", { name: /new branch/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete branch feature/y" })).not.toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: "Compare" }).length).toBeGreaterThan(0);
    // The protected badge stays informational but is no longer a settings link.
    expect(screen.getByText("protected")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "protected" })).not.toBeInTheDocument();
  });

  it("hides tag create/delete from a viewer", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.split("?")[0]!.endsWith("/repos/admin/test/tags")) {
        return Promise.resolve(jsonResponse([
          { name: "v1.0.0", commit: { sha: "abc", url: "" }, zipball_url: "/z", tarball_url: "/t" },
        ]));
      }
      return viewerFetch(url);
    });
    renderPage("/ui/admin/test/tags");
    await screen.findByText("v1.0.0");

    expect(screen.queryByRole("button", { name: /new tag/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete tag v1.0.0" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "zip" })).toBeInTheDocument();
  });

  it("hides blob Edit/Delete from a viewer but keeps Raw/History/Blame/permalink", async () => {
    mockFetch.mockImplementation(viewerFetch);
    renderPage("/ui/admin/test/blob/main/README.md");
    await screen.findByRole("button", { name: "Copy permalink" });

    expect(screen.getByRole("button", { name: "View raw file" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "History" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Blame" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete file" })).not.toBeInTheDocument();
  });
});
