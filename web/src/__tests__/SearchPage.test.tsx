import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import {
  SearchPage,
  buildAdvancedQuery,
  blobRefFromHtmlUrl,
  splitTextMatchFragment,
} from "../pages/SearchPage.js";

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

function renderPage(initialEntry = "/ui/search") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route path="/ui/search" element={<SearchPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const repoItem = {
  id: 1,
  full_name: "admin/hit-repo",
  description: "matching repo",
  visibility: "public",
};

describe("SearchPage", () => {
  it("prompts for a query before searching", () => {
    renderPage();
    expect(screen.getByText("Search bleephub")).toBeInTheDocument();
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("guides instead of 422ing a qualifier-only code search", async () => {
    const calls: string[] = [];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      calls.push(url.toString());
      return Promise.resolve(jsonResponse({ total_count: 0, incomplete_results: false, items: [] }));
    });
    renderPage("/ui/search?type=code&q=language%3Ago");
    expect(await screen.findByText(/Enter a search term/i)).toBeInTheDocument();
    expect(calls.some((u) => u.includes("/search/code"))).toBe(false);
  });

  it("searches repositories with the q parameter and shows the honest count", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({ total_count: 1, incomplete_results: false, items: [repoItem] }),
    );
    renderPage("/ui/search?q=hit&type=repositories");
    await waitFor(() => {
      expect(screen.getByText("admin/hit-repo")).toBeInTheDocument();
    });
    // The count is a live region so a screen reader hears the result on arrival.
    const count = screen.getByRole("status");
    expect(count).toHaveTextContent(/1 repository/);
    const url = String(mockFetch.mock.calls[0]![0]);
    expect(url).toContain("/api/v3/search/repositories?");
    expect(url).toContain("q=hit");
    expect(url).toContain("per_page=30");
  });

  it("builds official repository qualifiers from the filter UI and preserves sort options", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({ total_count: 1, incomplete_results: false, items: [repoItem] }),
    );
    renderPage(
      "/ui/search?q=bank&type=repositories&exclude_topic=web&archived=false&fork=true&sort=stars&order=asc",
    );
    await waitFor(() => {
      expect(screen.getByText("admin/hit-repo")).toBeInTheDocument();
    });

    const firstURL = new URL(String(mockFetch.mock.calls[0]![0]), "http://bleephub.test");
    expect(firstURL.searchParams.get("q")).toBe(
      "bank archived:false fork:true -topic:web",
    );
    expect(firstURL.searchParams.get("sort")).toBe("stars");
    expect(firstURL.searchParams.get("order")).toBe("asc");

    fireEvent.change(screen.getByLabelText("Excluded repository topic"), {
      target: { value: "legacy systems" },
    });
    await waitFor(() => {
      const lastURL = new URL(
        String(mockFetch.mock.calls[mockFetch.mock.calls.length - 1]![0]),
        "http://bleephub.test",
      );
      expect(lastURL.searchParams.get("q")).toContain('-topic:"legacy systems"');
    });
  });

  it("can run a qualifier-only repository search", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({ total_count: 1, incomplete_results: false, items: [repoItem] }),
    );
    renderPage("/ui/search?type=repositories&archived=true");
    await waitFor(() => {
      expect(screen.getByText("admin/hit-repo")).toBeInTheDocument();
    });
    const requestURL = new URL(String(mockFetch.mock.calls[0]![0]), "http://bleephub.test");
    expect(requestURL.searchParams.get("q")).toBe("archived:true");
  });

  it("switches result types in the sidebar and hits the matching search endpoint", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({ total_count: 0, incomplete_results: false, items: [] }),
    );
    renderPage("/ui/search?q=fix&type=issues");
    await waitFor(() => {
      expect(screen.getByText("No matching issues and pull requests")).toBeInTheDocument();
    });
    expect(String(mockFetch.mock.calls[0]![0])).toContain("/api/v3/search/issues?");

    fireEvent.click(screen.getByRole("button", { name: /Commits/ }));
    await waitFor(() => {
      const urls = mockFetch.mock.calls.map((c) => String(c[0]));
      // A full-page commits search (per_page=30), not just the sidebar's
      // per_page=1 count probe.
      expect(urls.some((u) => u.includes("/api/v3/search/commits?") && u.includes("per_page=30"))).toBe(true);
    });
  });

  it("probes the other result types' counts lazily and shows them in the sidebar", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("per_page=1")) {
        return Promise.resolve(jsonResponse({ total_count: 42, incomplete_results: false, items: [] }));
      }
      return Promise.resolve(jsonResponse({ total_count: 1, incomplete_results: false, items: [repoItem] }));
    });
    renderPage("/ui/search?q=hit&type=repositories");
    await waitFor(() => expect(screen.getByText("admin/hit-repo")).toBeInTheDocument());

    // The active type's count comes from its own result envelope…
    expect(screen.getByRole("button", { name: /Repositories/ })).toHaveTextContent("1");
    // …the other types are probed with per_page=1 after the active one lands.
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /Commits/ })).toHaveTextContent("42");
    });
    const probes = mockFetch.mock.calls.map((c) => String(c[0])).filter((u) => u.includes("per_page=1"));
    expect(probes.some((u) => u.includes("/api/v3/search/issues?"))).toBe(true);
    // Labels need a repository_id, so they are never probed blind.
    expect(probes.some((u) => u.includes("/api/v3/search/labels"))).toBe(false);
    expect(screen.getByRole("button", { name: /Labels/ })).toHaveTextContent("—");
  });

  it("links code results to the blob view and highlights text-match fragments", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({
        total_count: 1,
        incomplete_results: false,
        items: [
          {
            name: "main.go",
            path: "cmd/main.go",
            sha: "abc",
            html_url: "http://bleephub.test/admin/tool/blob/trunk/cmd/main.go",
            language: "Go",
            repository: { full_name: "admin/tool", default_branch: "trunk" },
            text_matches: [
              {
                object_url: "http://bleephub.test/api/v3/repos/admin/tool/contents/cmd/main.go",
                object_type: "FileContent",
                property: "content",
                fragment: "func retry() {}",
                matches: [{ text: "retry", indices: [5, 10] }],
              },
            ],
          },
        ],
      }),
    );
    renderPage("/ui/search?q=retry&type=code");
    const link = await screen.findByRole("link", { name: /admin\/tool/ });
    expect(link).toHaveAttribute("href", "/ui/admin/tool/blob/trunk/cmd/main.go");
    const mark = screen.getByText("retry", { selector: "mark" });
    expect(mark).toBeInTheDocument();
    // The request opted into the text-match media type.
    const init = mockFetch.mock.calls[0]![1] as RequestInit;
    expect((init.headers as Record<string, string>).Accept).toContain("text-match+json");
  });

  it("links user results to their account route and commit results to the commit page", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/search/users")) {
        return Promise.resolve(
          jsonResponse({
            total_count: 1,
            incomplete_results: false,
            items: [{ id: 5, login: "acme", type: "Organization", name: "Acme" }],
          }),
        );
      }
      if (url.includes("/search/commits")) {
        return Promise.resolve(
          jsonResponse({
            total_count: 1,
            incomplete_results: false,
            items: [
              {
                sha: "deadbeefcafe",
                commit: { message: "Fix the flake\n\nbody", author: { name: "A", email: "a@x", date: "2026-01-01T00:00:00Z" } },
                author: { login: "acme" },
                repository: { full_name: "acme/api" },
              },
            ],
          }),
        );
      }
      return Promise.resolve(jsonResponse({ total_count: 0, incomplete_results: false, items: [] }));
    });
    renderPage("/ui/search?q=acme&type=users");
    const userLink = await screen.findByRole("link", { name: "acme" });
    expect(userLink).toHaveAttribute("href", "/ui/orgs/acme");

    fireEvent.click(screen.getByRole("button", { name: /Commits/ }));
    const commitLink = await screen.findByRole("link", { name: "Fix the flake" });
    expect(commitLink).toHaveAttribute("href", "/ui/acme/api/commits/deadbeefcafe");
  });

  it("shows a friendly countdown and auto-retries once when search is throttled", async () => {
    let calls = 0;
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("per_page=1")) {
        return Promise.resolve(jsonResponse({ total_count: 0, incomplete_results: false, items: [] }));
      }
      calls += 1;
      if (calls === 1) {
        return Promise.resolve(
          new Response(JSON.stringify({ message: "API rate limit exceeded" }), {
            status: 403,
            headers: { "Content-Type": "application/json", "Retry-After": "1" },
          }),
        );
      }
      return Promise.resolve(jsonResponse({ total_count: 1, incomplete_results: false, items: [repoItem] }));
    });
    renderPage("/ui/search?q=hit&type=repositories");
    expect(await screen.findByText("You're searching too fast")).toBeInTheDocument();
    expect(screen.getByText(/retrying in 1s/i)).toBeInTheDocument();
    // After the Retry-After window, one automatic retry succeeds.
    await waitFor(() => expect(screen.getByText("admin/hit-repo")).toBeInTheDocument(), { timeout: 3000 });
  });

  it("marks pull requests distinctly in issue results", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({
        total_count: 2,
        incomplete_results: false,
        items: [
          {
            id: 10,
            number: 4,
            title: "A plain issue",
            state: "open",
            user: { login: "admin" },
            comments: 0,
            created_at: "2026-01-01T00:00:00Z",
            updated_at: "2026-01-01T00:00:00Z",
            pull_request: null,
            repository: { full_name: "admin/a" },
          },
          {
            id: 11,
            number: 5,
            title: "A pull request",
            state: "open",
            user: { login: "admin" },
            comments: 2,
            created_at: "2026-01-02T00:00:00Z",
            updated_at: "2026-01-02T00:00:00Z",
            pull_request: { url: "http://x/pulls/5" },
            repository: { full_name: "admin/a" },
          },
        ],
      }),
    );
    renderPage("/ui/search?q=a&type=issues");
    await waitFor(() => {
      expect(screen.getByText("A plain issue")).toBeInTheDocument();
    });
    expect(screen.getByText(/issue · admin\/a#4/)).toBeInTheDocument();
    expect(screen.getByText(/pull request · admin\/a#5/)).toBeInTheDocument();
  });

  it("requires a repository for label search and resolves its id", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/v3/repos/admin/labelled") {
        return Promise.resolve(jsonResponse({ id: 77, full_name: "admin/labelled" }));
      }
      if (url.startsWith("/api/v3/search/labels?")) {
        return Promise.resolve(
          jsonResponse({
            total_count: 1,
            incomplete_results: false,
            items: [{ id: 1, name: "bug", color: "ff0000", default: true, description: "Bugs" }],
          }),
        );
      }
      return Promise.resolve(jsonResponse({ message: "unexpected" }, 500));
    });

    renderPage("/ui/search?q=bug&type=labels");
    expect(screen.getByText("Pick a repository")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Repository for label search"), {
      target: { value: "admin/labelled" },
    });
    fireEvent.click(screen.getByRole("button", { name: /search/i }));

    await waitFor(() => {
      expect(screen.getByText("bug")).toBeInTheDocument();
    });
    const labelCall = mockFetch.mock.calls
      .map((c) => String(c[0]))
      .find((u) => u.startsWith("/api/v3/search/labels?"));
    expect(labelCall).toContain("repository_id=77");
  });

  it("surfaces search failures", async () => {
    mockFetch.mockResolvedValue(jsonResponse({ message: "boom" }, 500));
    renderPage("/ui/search?q=zzz&type=users");
    // The list retries a plain failure once (app-default parity) before erroring.
    await waitFor(
      () => {
        expect(screen.getByText("Search failed")).toBeInTheDocument();
      },
      { timeout: 4000 },
    );
  });

  it("builds a qualifier query from the advanced search form and runs it", async () => {
    mockFetch.mockResolvedValue(jsonResponse({ total_count: 0, items: [] }));
    renderPage("/ui/search?type=repositories");
    fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    fireEvent.change(await screen.findByLabelText("With these words"), { target: { value: "http client" } });
    fireEvent.change(screen.getByLabelText("Written in language"), { target: { value: "go" } });
    fireEvent.change(screen.getByLabelText("In organization"), { target: { value: "e6qu" } });
    fireEvent.change(screen.getByLabelText("With at least this many stars"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Build query" }));
    await waitFor(() => {
      const hit = mockFetch.mock.calls.map(([u]) => String(u)).find((u) => u.includes("/search/repositories"));
      expect(hit).toBeDefined();
      // The `q` param encodes spaces as `+`; normalise before comparing.
      expect(decodeURIComponent(hit!.replace(/\+/g, " "))).toContain("http client language:go org:e6qu stars:>=5");
    });
  });
});

describe("SearchPage sort controls", () => {
  it("re-issues the Issues search with the chosen sort and order", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({ total_count: 0, incomplete_results: false, items: [] }),
    );
    renderPage("/ui/search?q=fix&type=issues");
    await waitFor(() => {
      expect(String(mockFetch.mock.calls[0]![0])).toContain("/api/v3/search/issues?");
    });
    // The initial best-match request carries no sort/order.
    const firstURL = new URL(String(mockFetch.mock.calls[0]![0]), "http://bleephub.test");
    expect(firstURL.searchParams.get("sort")).toBeNull();

    fireEvent.change(screen.getByLabelText("Issue search sort"), {
      target: { value: "created" },
    });
    await waitFor(() => {
      const hit = mockFetch.mock.calls
        .map((c) => String(c[0]))
        .find((u) => u.includes("/api/v3/search/issues?") && u.includes("sort=created"));
      expect(hit).toBeDefined();
    });
    // Order defaults to desc once a sort key is selected.
    const issueURLs = mockFetch.mock.calls
      .map((c) => String(c[0]))
      .filter((u) => u.includes("/api/v3/search/issues?"));
    const sorted = new URL(issueURLs[issueURLs.length - 1]!, "http://bleephub.test");
    expect(sorted.searchParams.get("sort")).toBe("created");
    expect(sorted.searchParams.get("order")).toBe("desc");
  });

  it("re-issues the Users search with the chosen sort", async () => {
    mockFetch.mockResolvedValue(
      jsonResponse({ total_count: 0, incomplete_results: false, items: [] }),
    );
    renderPage("/ui/search?q=octo&type=users");
    await waitFor(() => {
      expect(String(mockFetch.mock.calls[0]![0])).toContain("/api/v3/search/users?");
    });

    fireEvent.change(screen.getByLabelText("User search sort"), {
      target: { value: "followers" },
    });
    await waitFor(() => {
      const hit = mockFetch.mock.calls
        .map((c) => String(c[0]))
        .find((u) => u.includes("/api/v3/search/users?") && u.includes("sort=followers"));
      expect(hit).toBeDefined();
    });
  });
});

describe("splitTextMatchFragment", () => {
  it("splits on byte indices, merging overlaps and surviving multi-byte text", () => {
    expect(
      splitTextMatchFragment("func retry() {}", [{ text: "retry", indices: [5, 10] }]),
    ).toEqual([
      { text: "func ", matched: false },
      { text: "retry", matched: true },
      { text: "() {}", matched: false },
    ]);
    // "é" is two UTF-8 bytes: the match after it lands correctly only when the
    // indices are treated as byte offsets.
    expect(
      splitTextMatchFragment("é retry", [{ text: "retry", indices: [3, 8] }]),
    ).toEqual([
      { text: "é ", matched: false },
      { text: "retry", matched: true },
    ]);
    // Overlapping spans merge; out-of-range spans are dropped.
    expect(
      splitTextMatchFragment("abcdef", [
        { text: "abc", indices: [0, 3] },
        { text: "bcd", indices: [1, 4] },
        { text: "zz", indices: [4, 99] },
      ]),
    ).toEqual([
      { text: "abcd", matched: true },
      { text: "ef", matched: false },
    ]);
  });
});

describe("blobRefFromHtmlUrl", () => {
  it("extracts the ref segment of a blob html_url", () => {
    expect(blobRefFromHtmlUrl("http://x/o/r/blob/main/a/b.go")).toBe("main");
    expect(blobRefFromHtmlUrl("http://x/o/r/commit/abc")).toBeNull();
  });
});

describe("buildAdvancedQuery", () => {
  it("assembles, quotes multi-word values, and drops empty fields", () => {
    expect(
      buildAdvancedQuery({ keywords: "rate limiter", language: "go", repo: "e6qu/bleephub", user: "", org: "", topic: "web server", stars: "10" }),
    ).toBe('rate limiter language:go repo:e6qu/bleephub topic:"web server" stars:>=10');
    expect(buildAdvancedQuery({ keywords: "", language: "", repo: "", user: "", org: "", topic: "", stars: "" })).toBe("");
    expect(buildAdvancedQuery({ keywords: "", language: "", repo: "", user: "", org: "", topic: "", stars: "abc" })).toBe("");
  });
});
