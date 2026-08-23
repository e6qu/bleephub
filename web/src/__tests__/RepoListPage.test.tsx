import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { ReposPage } from "../pages/ReposPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200, headers?: Record<string, string>) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
  window.history.pushState({}, "", "/");
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <ReposPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const reposData = [
  {
    id: 1,
    name: "test",
    full_name: "admin/test",
    description: "a repo",
    homepage: null,
    default_branch: "main",
    visibility: "public",
    private: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-02T00:00:00Z",
    pushed_at: "2026-01-02T00:00:00Z",
    size: 0,
    owner: { login: "admin", type: "User" },
    license: null,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
    has_pull_requests: true,
    is_template: false,
    archived: false,
    web_commit_signoff_required: false,
    allow_squash_merge: true,
    allow_merge_commit: true,
    allow_rebase_merge: true,
    allow_auto_merge: false,
    allow_update_branch: false,
    delete_branch_on_merge: false,
    use_squash_pr_title_as_default: false,
    squash_merge_commit_title: "COMMIT_OR_PR_TITLE",
    squash_merge_commit_message: "COMMIT_MESSAGES",
    merge_commit_title: "PR_TITLE",
    merge_commit_message: "PR_BODY",
    pull_request_creation_policy: "open",
  },
];

describe("RepoListPage type filter", () => {
  it("re-fetches with type= when a Type value is selected", async () => {
    mockFetch.mockResolvedValue(jsonResponse(reposData));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("admin/test")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByLabelText("Type filter"), {
      target: { value: "sources" },
    });

    await waitFor(() => {
      const urls = mockFetch.mock.calls.map((c) => String(c[0]));
      expect(urls.some((u) => u.includes("type=sources"))).toBe(true);
    });
  });
});

describe("RepoListPage search across pages", () => {
  it("finds a match on a later page by walking the Link headers", async () => {
    const page2Repo = { ...reposData[0]!, id: 2, name: "zebra", full_name: "admin/zebra", description: "page two repo" };
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("page=2")) return Promise.resolve(jsonResponse([page2Repo]));
      return Promise.resolve(
        jsonResponse(reposData, 200, {
          Link: '</api/v3/user/repos?per_page=30&page=2>; rel="next"',
        }),
      );
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("admin/test")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByLabelText("Find a repository"), { target: { value: "zebra" } });
    expect(await screen.findByText("admin/zebra")).toBeInTheDocument();
  });

  it("renders language/stars/forks meta on repo rows", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse([{ ...reposData[0]!, language: "Go", stargazers_count: 42, forks_count: 7 }]),
    );
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("admin/test")).toBeInTheDocument();
    });
    expect(screen.getByText("Go")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "42 stars" })).toHaveAttribute(
      "href",
      "/ui/admin/test/stargazers",
    );
    expect(screen.getByLabelText("7 forks")).toBeInTheDocument();
  });
});
