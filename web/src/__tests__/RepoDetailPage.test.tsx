import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { RepoCommitPage, RepoDetailPage, RepoFilePage } from "../pages/RepoDetailPage.js";

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

function renderPage(path = "/ui/repos/admin/test") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/repos/:owner/:repo" element={<RepoDetailPage />} />
          <Route path="/ui/repos/:owner/:repo/commits" element={<RepoDetailPage initialTab="commits" />} />
          <Route path="/ui/repos/:owner/:repo/branches" element={<RepoDetailPage initialTab="branches" />} />
          <Route path="/ui/repos/:owner/:repo/tags" element={<RepoDetailPage initialTab="tags" />} />
          <Route path="/ui/repos/:owner/:repo/activity" element={<RepoDetailPage initialTab="activity" />} />
          <Route path="/ui/repos/:owner/:repo/commits/:sha" element={<RepoCommitPage />} />
          <Route path="/ui/repos/:owner/:repo/tree/:ref/*" element={<RepoDetailPage />} />
          <Route path="/ui/repos/:owner/:repo/blob/:ref/*" element={<RepoFilePage />} />
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

describe("RepoDetailPage releases", () => {
  it("renders a draft release as 'draft', not a 1970 date", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findByText("a repo");
    fireEvent.click(screen.getByRole("button", { name: "Releases" }));
    await waitFor(() => {
      expect(screen.getByText("Draft release")).toBeInTheDocument();
    });
    expect(screen.getByText("draft")).toBeInTheDocument();
    // the published release still shows its real date
    expect(
      screen.getByText(`published ${new Date("2026-02-01T00:00:00Z").toLocaleDateString()}`),
    ).toBeInTheDocument();
    // no zero-time rendering anywhere
    expect(screen.queryByText(/1970/)).not.toBeInTheDocument();
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

    // Navigate into a subdirectory. The readme query cache (keyed by branch,
    // not path) is retained, so the panel must be gated on path === "".
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
    // short sha + commit count
    expect(screen.getByText("abc123".slice(0, 7))).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /1 commit\b/ })).toHaveAttribute(
      "href",
      "/ui/repos/admin/test/commits",
    );
    expect(screen.getByRole("link", { name: "abc123" })).toHaveAttribute(
      "href",
      "/ui/repos/admin/test/commits/abc123",
    );
  });

  it("exposes a Code clone dropdown with the HTTPS clone URL and a copy button", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findAllByText("README.md");

    // The repo sub-tab strip also has a "Code" button; the clone dropdown is
    // the one carrying aria-expanded (matched via the `expanded` filter).
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
      "/ui/repos/admin/test/blob/main/README.md",
    );
    fireEvent.click(screen.getByRole("link", { name: "Commits" }));
    expect(await screen.findByRole("link", { name: "Initial commit" })).toHaveAttribute(
      "href",
      "/ui/repos/admin/test/commits/abc123",
    );
  });

  it("loads a shareable repository tree at a commit and path", async () => {
    const calls: string[] = [];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      calls.push(url.toString());
      return routedFetch(url);
    });
    renderPage("/ui/repos/admin/test/tree/abc123/src");

    await screen.findAllByText("src");
    expect(calls).toContain(
      "/api/v3/repos/admin/test/contents/src?ref=abc123",
    );
    expect(calls).toContain(
      "/api/v3/repos/admin/test/commits?per_page=100&sha=abc123",
    );
  });

  it("routes an organization repository owner to the organization profile", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      if (url.toString().endsWith("/repos/acme/test")) {
        return Promise.resolve(jsonResponse({
          ...repoData,
          full_name: "acme/test",
          owner: { login: "acme", type: "Organization" },
        }));
      }
      return routedFetch(url);
    });
    renderPage("/ui/repos/acme/test");

    expect(await screen.findByRole("link", { name: "acme" })).toHaveAttribute(
      "href",
      "/ui/orgs/acme",
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

  it("groups administrative resources under the repository More menu", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findAllByText("README.md");

    expect(screen.getByRole("navigation", { name: "Repository content" })).toBeInTheDocument();
    expect(screen.getByText("Repository administration")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "All repository settings" })).toHaveAttribute(
      "href",
      "/ui/repos/admin/test/settings",
    );
  });
});

describe("RepoDetailPage About sidebar", () => {
  it("renders description, website, topics, releases, packages and social counts", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await screen.findAllByText("README.md");

    // description + website live in the sidebar
    const about = screen.getByRole("complementary", { name: "About" });
    expect(within(about).getByText("a repo")).toBeInTheDocument();
    expect(within(about).getByText("example.com")).toBeInTheDocument();
    // topics as pill chips
    expect(within(about).getByText("cli")).toBeInTheDocument();
    expect(within(about).getByText("tooling")).toBeInTheDocument();
    // latest release + Latest badge
    expect(within(about).getByText("Latest")).toBeInTheDocument();
    expect(within(about).getByText("No packages published")).toBeInTheDocument();
    // social counts moved into the sidebar
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
    renderPage("/ui/repos/admin/test/commits/abc123");

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
    renderPage("/ui/repos/admin/test/commits/abc123");
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
    renderPage("/ui/repos/admin/test/commits/abc123");

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
    renderPage("/ui/repos/admin/test/commits/abc123");

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
    renderPage("/ui/repos/admin/test/commits/abc123");

    fireEvent.click(await screen.findByRole("button", { name: "add reaction" }));
    fireEvent.click(screen.getByRole("button", { name: "react with heart" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/comments/50/reactions") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ content: "heart" });
    });
  });

  it("renders a repository file at its durable URL with Raw + History controls", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/repos/admin/test/blob/main/README.md");

    expect(await screen.findByText("admin/test")).toBeInTheDocument();
    expect(screen.getByText(/extra detail/)).toBeInTheDocument();
    // github.com's per-file Raw and History affordances.
    expect(screen.getByRole("button", { name: "View raw file" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "History" })).toHaveAttribute(
      "href",
      "/ui/repos/admin/test/commits?path=README.md&sha=main",
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
    renderPage("/ui/repos/admin/test");

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
    renderPage("/ui/repos/admin/test/activity");

    // the activity row: actor login, "pushed to" label, and short ref
    expect(await screen.findByText("octocat")).toBeInTheDocument();
    expect(screen.getByText("pushed to")).toBeInTheDocument();
    expect(screen.getByText("main")).toBeInTheDocument();
    expect(calls.some((u) => u.endsWith("/api/v3/repos/admin/test/activity"))).toBe(true);
  });
});

describe("RepoDetailPage file editing", () => {
  it("edits a file through the contents API", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage("/ui/repos/admin/test/blob/main/README.md");
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
    renderPage("/ui/repos/admin/test/blob/main/README.md");
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
    renderPage("/ui/repos/admin/test");
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
    renderPage("/ui/repos/admin/test/branches");
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
    renderPage("/ui/repos/admin/test/branches");
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
    renderPage("/ui/repos/admin/test/tags");
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
});
