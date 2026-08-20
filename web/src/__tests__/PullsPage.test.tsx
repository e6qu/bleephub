import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { PullsPage } from "../pages/PullsPage.js";

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
  // Viewed-file and review-summary state persist per PR in sessionStorage;
  // keep tests independent.
  sessionStorage.clear();
});

function renderAt(path: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/repos/:owner/:repo/pulls" element={<PullsPage />} />
          <Route path="/ui/repos/:owner/:repo/pulls/:number" element={<PullsPage />} />
          <Route path="/ui/repos/:owner/:repo/pulls/:number/commits" element={<PullsPage />} />
          <Route path="/ui/repos/:owner/:repo/pulls/:number/files" element={<PullsPage />} />
          <Route path="/ui/repos/:owner/:repo/pulls/:number/checks" element={<PullsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function pr(number: number, title: string, overrides: Record<string, unknown> = {}) {
  return {
    id: number,
    number,
    title,
    body: "body",
    state: "open",
    draft: false,
    user: { login: "admin", avatar_url: "" },
    head: { ref: "feature", sha: "abc" },
    base: { ref: "main", sha: "def" },
    labels: [],
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    merged_at: null,
    merged: false,
    ...overrides,
  };
}

const noChecks = { total_count: 0, check_runs: [] };
const emptyStatus = { state: "pending", sha: "abc", total_count: 0, statuses: [] };
const emptyReviewers = { users: [], teams: [] };
const viewer = { id: 1, login: "admin", avatar_url: "", type: "User", site_admin: true };
// Viewer-role gating reads the repo payload's permissions; the admin fixture
// carries full access so the write affordances render as they always did.
const adminPerms = { admin: true, push: true, pull: true };
const adminRepo = {
  id: 1,
  name: "test",
  full_name: "admin/test",
  default_branch: "main",
  owner: { login: "admin", type: "User" },
  permissions: adminPerms,
};

/**
 * Detail-view mock: overrides() is consulted first; everything else gets an
 * honest empty response of the right shape for PR #9 on admin/test.
 */
function mockPRApis(overrides: (u: string, init?: RequestInit) => Response | undefined = () => undefined) {
  mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
    const u = url.toString();
    const o = overrides(u, init);
    if (o) return Promise.resolve(o);
    if (u.includes("/check-runs")) return Promise.resolve(jsonResponse(noChecks));
    if (u.includes("/commits/abc/status")) return Promise.resolve(jsonResponse(emptyStatus));
    if (u.includes("/requested_reviewers")) return Promise.resolve(jsonResponse(emptyReviewers));
    if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse(viewer));
    if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
    if (u.endsWith("/pulls/9")) return Promise.resolve(jsonResponse(pr(9, "Feature PR")));
    if (u.endsWith("/api/graphql")) {
      return Promise.resolve(
        jsonResponse({
          data: {
            repository: {
              pullRequest: {
                reviewThreads: { nodes: [] },
              },
            },
          },
        }),
      );
    }
    return Promise.resolve(jsonResponse([]));
  });
}

function findCall(pathSuffix: string, method?: string): RequestInit | undefined {
  const call = mockFetch.mock.calls.find((c) => {
    const init = c[1] as RequestInit | undefined;
    return c[0]!.toString().endsWith(pathSuffix) && init?.method === method;
  });
  if (!call) return undefined;
  return (call[1] as RequestInit | undefined) ?? {};
}

function checkRun(id: number, name: string, overrides: Record<string, unknown> = {}) {
  return {
    id,
    name,
    status: "completed",
    conclusion: "success",
    started_at: "2026-01-01T00:00:00Z",
    completed_at: "2026-01-01T00:00:42Z",
    details_url: "",
    app: { id: 1 },
    ...overrides,
  };
}

describe("PullsPage detail", () => {
  it("loads the PR via the single-PR endpoint, not by scanning the list", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/check-runs")) return Promise.resolve(jsonResponse(noChecks));
      if (u.includes("/pulls/77/") || u.includes("/issues/77/comments")) {
        return Promise.resolve(jsonResponse([]));
      }
      if (u.endsWith("/pulls/77")) return Promise.resolve(jsonResponse(pr(77, "Single fetch PR")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls/77");
    await waitFor(() => {
      expect(screen.getByText("Single fetch PR")).toBeInTheDocument();
    });
    const calls = mockFetch.mock.calls.map((c) => c[0]!.toString());
    expect(calls).toContain("/api/v3/repos/admin/test/pulls/77");
  });

  it("shows a not-found state (not a spinner) for a missing PR", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/pulls/999")) {
        return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls/999");
    await waitFor(() => {
      expect(screen.getByText(/pull request #999 not found/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/loading pr/i)).not.toBeInTheDocument();
  });

  it("shows an error state when the PR fetch fails", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/pulls/5")) {
        return Promise.resolve(jsonResponse({ message: "boom" }, 500));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls/5");
    await waitFor(() => {
      expect(screen.getByText(/failed to load pr #5/i)).toBeInTheDocument();
    });
  });
});

describe("PullsPage list pagination", () => {
  it("pages through via the Link header's rel=next URL", async () => {
    const page2Url = "/api/v3/repos/admin/test/pulls?state=open&per_page=50&page=2";
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues")) return Promise.resolve(jsonResponse([]));
      if (u.includes("page=2")) return Promise.resolve(jsonResponse([pr(3, "third pr")]));
      if (u.includes("/pulls?")) {
        return Promise.resolve(
          jsonResponse([pr(1, "first pr"), pr(2, "second pr")], 200, {
            Link: `<${page2Url}>; rel="next"`,
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls");
    await waitFor(() => {
      expect(screen.getByText("first pr")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await waitFor(() => {
      expect(screen.getByText("third pr")).toBeInTheDocument();
    });
    const calls = mockFetch.mock.calls.map((c) => c[0]!.toString());
    expect(calls).toContain(page2Url);
  });

  it("carries the base-branch filter to the server query", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/pulls?")) return Promise.resolve(jsonResponse([pr(1, "first pr")]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls");
    await waitFor(() => expect(screen.getByText("first pr")).toBeInTheDocument());
    fireEvent.change(screen.getByLabelText("Filter by base branch"), { target: { value: "main" } });
    await waitFor(() => {
      const calls = mockFetch.mock.calls.map((c) => c[0]!.toString());
      expect(calls.some((u) => u.includes("/pulls?") && u.includes("base=main"))).toBe(true);
    });
  });
});

describe("PullsPage checks section", () => {
  function mockDetail(prData: unknown, checks: unknown, combinedStatus: unknown = emptyStatus) {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/commits/abc/check-runs")) return Promise.resolve(jsonResponse(checks));
      if (u.includes("/commits/abc/status")) return Promise.resolve(jsonResponse(combinedStatus));
      if (u.includes("/requested_reviewers")) return Promise.resolve(jsonResponse(emptyReviewers));
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      if (u.endsWith("/pulls/9")) return Promise.resolve(jsonResponse(prData));
      return Promise.resolve(jsonResponse([]));
    });
  }

  it("shows the green all-passed summary with per-check rows", async () => {
    mockDetail(pr(9, "Checked PR"), {
      total_count: 2,
      check_runs: [checkRun(1, "build"), checkRun(2, "lint")],
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    // The Conversation merge box shows the aggregate summary…
    expect(await screen.findByText(/all checks have passed/i)).toBeInTheDocument();
    // …and the Checks tab (labeled with the reported-check count) lists the
    // per-check rows.
    fireEvent.click(screen.getByRole("tab", { name: /^Checks/ }));
    expect(await screen.findByText("build")).toBeInTheDocument();
    expect(screen.getByText("lint")).toBeInTheDocument();
    // 42s duration from started/completed timestamps.
    expect(screen.getAllByText("42s").length).toBe(2);
  });

  it("shows the pending summary while a check is in progress", async () => {
    mockDetail(pr(9, "Checked PR"), {
      total_count: 2,
      check_runs: [
        checkRun(1, "build"),
        checkRun(2, "e2e", { status: "in_progress", conclusion: null, completed_at: null }),
      ],
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    expect(await screen.findByText(/some checks haven't completed yet/i)).toBeInTheDocument();
  });

  it("shows the failure summary when a check concluded unsuccessfully", async () => {
    mockDetail(pr(9, "Checked PR"), {
      total_count: 2,
      check_runs: [checkRun(1, "build"), checkRun(2, "test", { conclusion: "failure" })],
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    expect(await screen.findByText(/some checks were not successful/i)).toBeInTheDocument();
  });

  it("hides the checks box when the commit has no check runs", async () => {
    mockDetail(pr(9, "Checked PR"), noChecks);
    renderAt("/ui/repos/admin/test/pulls/9");
    await screen.findByText("Checked PR");
    expect(screen.queryByText(/all checks have passed/i)).not.toBeInTheDocument();
  });

  it("links a check to the run detail page when details_url points at a run", async () => {
    mockDetail(pr(9, "Checked PR"), {
      total_count: 1,
      check_runs: [
        checkRun(1, "build", {
          details_url: "http://bleephub.localhost/admin/test/actions/runs/42",
        }),
      ],
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    fireEvent.click(await screen.findByRole("tab", { name: /^Checks/ }));
    const link = await screen.findByRole("link", { name: /build/i });
    expect(link).toHaveAttribute("href", "/ui/repos/admin/test/actions/runs/42");
  });

  it("disables merging and explains when mergeable_state is blocked", async () => {
    mockDetail(pr(9, "Blocked PR", { mergeable_state: "blocked" }), {
      total_count: 1,
      check_runs: [checkRun(1, "required-check", { status: "queued", conclusion: null })],
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    expect(await screen.findByText(/merging is blocked — required checks must pass/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /merge pull request/i })).toBeDisabled();
  });
});

function review(id: number, login: string, state: string, overrides: Record<string, unknown> = {}) {
  return {
    id,
    user: { login, avatar_url: "" },
    body: "",
    state,
    commit_id: "abc",
    submitted_at: "2026-01-02T00:00:00Z",
    ...overrides,
  };
}

describe("PullsPage reviews", () => {
  it("lists reviews with state badges and dismisses via the dismissals endpoint", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/reviews") && init?.method === undefined) {
        return jsonResponse([
          review(1, "alice", "APPROVED", { body: "ship it" }),
          review(2, "bob", "CHANGES_REQUESTED"),
          review(3, "carol", "COMMENTED"),
        ]);
      }
      if (u.endsWith("/pulls/9/reviews/1/dismissals") && init?.method === "PUT") {
        return jsonResponse(review(1, "alice", "DISMISSED"));
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    expect(await screen.findByText("Approved")).toBeInTheDocument();
    expect(screen.getByText("Changes requested")).toBeInTheDocument();
    expect(screen.getByText("Commented")).toBeInTheDocument();
    expect(screen.getByText("ship it")).toBeInTheDocument();

    // Only APPROVED / CHANGES_REQUESTED reviews are dismissable.
    const dismissButtons = screen.getAllByRole("button", { name: /^dismiss$/i });
    expect(dismissButtons.length).toBe(2);

    fireEvent.click(dismissButtons[0]!);
    fireEvent.change(screen.getByLabelText("dismissal message for review 1"), {
      target: { value: "stale approval" },
    });
    fireEvent.click(screen.getByRole("button", { name: /confirm dismiss/i }));

    await waitFor(() => {
      expect(findCall("/pulls/9/reviews/1/dismissals", "PUT")).toBeDefined();
    });
    const init = findCall("/pulls/9/reviews/1/dismissals", "PUT");
    expect(JSON.parse(String(init?.body))).toEqual({ message: "stale approval" });
  });

  it("submits a review with the chosen event from the Files-changed popover", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/reviews") && init?.method === "POST") {
        return jsonResponse(review(9, "admin", "CHANGES_REQUESTED"));
      }
      if (u.endsWith("/pulls/9/files")) {
        return jsonResponse([
          {
            sha: "abc",
            filename: "main.go",
            status: "modified",
            additions: 1,
            deletions: 0,
            changes: 1,
            patch: "@@ -1,1 +1,2 @@\n old\n+new",
          },
        ]);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9/files");
    await screen.findByText("main.go");

    // The review popover lives behind the "Review changes" button.
    fireEvent.click(screen.getByRole("button", { name: /review changes/i }));
    const summary = await screen.findByLabelText("Review summary");

    // REQUEST_CHANGES / COMMENT need a body; APPROVE does not.
    fireEvent.click(screen.getByRole("radio", { name: /request changes/i }));
    expect(screen.getByRole("button", { name: /^submit review$/i })).toBeDisabled();
    fireEvent.change(summary, { target: { value: "needs tests" } });
    expect(screen.getByRole("button", { name: /^submit review$/i })).toBeEnabled();

    fireEvent.click(screen.getByRole("button", { name: /^submit review$/i }));
    await waitFor(() => {
      expect(findCall("/pulls/9/reviews", "POST")).toBeDefined();
    });
    const init = findCall("/pulls/9/reviews", "POST");
    expect(JSON.parse(String(init?.body))).toEqual({
      body: "needs tests",
      event: "REQUEST_CHANGES",
    });
  });
});

describe("PullsPage review comment threads", () => {
  function reviewComment(id: number, overrides: Record<string, unknown> = {}) {
    return {
      id,
      pull_request_review_id: 1,
      diff_hunk: "@@ -1,2 +1,2 @@\n-old line\n+new line",
      path: "main.go",
      line: 3,
      side: "RIGHT",
      body: `comment ${id}`,
      user: { login: "carol", avatar_url: "" },
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      ...overrides,
    };
  }

  function mockThreads() {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/comments") && init?.method === undefined) {
        return jsonResponse([
          reviewComment(11),
          reviewComment(12, { in_reply_to_id: 11, created_at: "2026-01-01T01:00:00Z" }),
        ]);
      }
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        const body = JSON.parse(String(init.body ?? "{}")) as { query?: string };
        if (body.query?.includes("resolveReviewThread")) {
          return jsonResponse({
            data: {
              resolveReviewThread: {
                thread: {
                  id: "PRT_kgDO00000011",
                  isResolved: true,
                  comments: { nodes: [{ databaseId: 11 }, { databaseId: 12 }] },
                },
              },
            },
          });
        }
        return jsonResponse({
          data: {
            repository: {
              pullRequest: {
                reviewThreads: {
                  nodes: [
                    {
                      id: "PRT_kgDO00000011",
                      isResolved: false,
                      comments: { nodes: [{ databaseId: 11 }, { databaseId: 12 }] },
                    },
                  ],
                },
              },
            },
          },
        });
      }
      if (u.endsWith("/pulls/9/comments") && init?.method === "POST") {
        return jsonResponse(reviewComment(13, { in_reply_to_id: 11 }), 201);
      }
      return undefined;
    });
  }

  it("groups comments by file/line, renders the diff hunk, and resolves the thread", async () => {
    mockThreads();
    renderAt("/ui/repos/admin/test/pulls/9");

    expect(await screen.findByText("main.go:3")).toBeInTheDocument();
    expect(screen.getByText(/@@ -1,2 \+1,2 @@/)).toBeInTheDocument();
    expect(screen.getByText("comment 11")).toBeInTheDocument();
    expect(screen.getByText("comment 12")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^resolve$/i }));
    await waitFor(() => {
      const graphQLCalls = mockFetch.mock.calls.filter(
        ([url, init]) => url.toString().endsWith("/api/graphql") && (init as RequestInit | undefined)?.method === "POST",
      );
      expect(
        graphQLCalls.some(([, init]) => {
          const body = JSON.parse(String((init as RequestInit).body ?? "{}")) as {
            query?: string;
            variables?: { input?: { threadId?: string } };
          };
          return (
            body.query?.includes("resolveReviewThread") &&
            body.variables?.input?.threadId === "PRT_kgDO00000011"
          );
        }),
      ).toBe(true);
    });
  });

  it("replies to a thread with in_reply_to", async () => {
    mockThreads();
    renderAt("/ui/repos/admin/test/pulls/9");

    const input = await screen.findByLabelText("reply to thread on main.go");
    fireEvent.change(input, { target: { value: "done in latest push" } });
    fireEvent.click(screen.getByRole("button", { name: /^reply$/i }));

    await waitFor(() => {
      expect(findCall("/pulls/9/comments", "POST")).toBeDefined();
    });
    const init = findCall("/pulls/9/comments", "POST");
    expect(JSON.parse(String(init?.body))).toEqual({
      body: "done in latest push",
      in_reply_to: 11,
    });
  });

  it("adds a reaction to a review comment via POST /pulls/comments/{id}/reactions", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/comments") && init?.method === undefined) {
        return jsonResponse([reviewComment(11)]);
      }
      if (u.endsWith("/pulls/comments/11/reactions") && init?.method === "POST") {
        return jsonResponse(
          { id: 8, content: "heart", user: { login: "admin" }, created_at: "2026-01-01T00:00:00Z" },
          201,
        );
      }
      if (u.endsWith("/pulls/comments/11/reactions") && init?.method === undefined) {
        return jsonResponse([]);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    // The page has several ReactionBars (PR body + each review comment); the
    // review comment's is the last to render.
    expect(await screen.findByText("comment 11")).toBeInTheDocument();
    // The review comment's ReactionBar renders only after its reactions query
    // resolves (ReactionBar returns null while loading), so wait for both the
    // PR-body bar and the review-comment bar, then use the review comment's.
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: "add reaction" }).length).toBeGreaterThanOrEqual(2),
    );
    const addButtons = screen.getAllByRole("button", { name: "add reaction" });
    fireEvent.click(addButtons[addButtons.length - 1]!);
    fireEvent.click(screen.getByRole("menuitem", { name: "react with heart" }));
    await waitFor(() => {
      expect(findCall("/pulls/comments/11/reactions", "POST")).toBeDefined();
    });
    expect(JSON.parse(String(findCall("/pulls/comments/11/reactions", "POST")?.body))).toEqual({
      content: "heart",
    });
  });
});

describe("PullsPage requested reviewers", () => {
  it("shows, adds, and removes requested reviewers", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/requested_reviewers") && init?.method === undefined) {
        return jsonResponse({
          users: [{ id: 3, login: "carol", avatar_url: "", type: "User", site_admin: false }],
          teams: [],
        });
      }
      if (u.endsWith("/pulls/9/requested_reviewers") && init?.method === "POST") {
        return jsonResponse(pr(9, "Feature PR"), 201);
      }
      if (u.endsWith("/pulls/9/requested_reviewers") && init?.method === "DELETE") {
        return jsonResponse(pr(9, "Feature PR"));
      }
      // Reviewers are chosen from the assignable users; carol is already
      // requested (filtered out), dave is offered.
      if (u.endsWith("/assignees")) {
        return jsonResponse([{ login: "carol" }, { login: "dave" }]);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    // The requested-reviewer chip (carol also appears as an assignable option,
    // so target the chip's remove control to disambiguate).
    expect(await screen.findByLabelText("remove reviewer carol")).toBeInTheDocument();

    const reviewerSelect = await screen.findByLabelText("reviewer login");
    fireEvent.change(reviewerSelect, { target: { value: "dave" } });
    await waitFor(() => {
      expect(findCall("/pulls/9/requested_reviewers", "POST")).toBeDefined();
    });
    const post = findCall("/pulls/9/requested_reviewers", "POST");
    expect(JSON.parse(String(post?.body))).toEqual({ reviewers: ["dave"], team_reviewers: [] });

    fireEvent.click(screen.getByLabelText("remove reviewer carol"));
    await waitFor(() => {
      expect(findCall("/pulls/9/requested_reviewers", "DELETE")).toBeDefined();
    });
    const del = findCall("/pulls/9/requested_reviewers", "DELETE");
    expect(JSON.parse(String(del?.body))).toEqual({ reviewers: ["carol"], team_reviewers: [] });
  });

  it("requests and removes a team reviewer", async () => {
    mockPRApis((u, init) => {
      // Team reviewers exist only on org-owned repos; the picker gates on the
      // repo detail's owner type, so serve an Organization owner here.
      if (u.endsWith("/repos/admin/test") && init?.method === undefined) {
        return jsonResponse({
          id: 1,
          name: "test",
          full_name: "admin/test",
          default_branch: "main",
          owner: { login: "admin", type: "Organization" },
          permissions: adminPerms,
        });
      }
      if (u.endsWith("/pulls/9/requested_reviewers") && init?.method === undefined) {
        return jsonResponse({
          users: [],
          teams: [{ id: 5, slug: "platform", name: "Platform" }],
        });
      }
      if (u.endsWith("/pulls/9/requested_reviewers")) {
        return jsonResponse(pr(9, "Feature PR"), init?.method === "POST" ? 201 : 200);
      }
      // The owning org exposes two teams; platform is already requested.
      if (u.endsWith("/orgs/admin/teams")) {
        return jsonResponse([
          { id: 5, slug: "platform", name: "Platform", description: "", privacy: "closed" },
          { id: 6, slug: "security", name: "Security", description: "", privacy: "closed" },
        ]);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    // The requested team chip is removable, and only the not-yet-requested team
    // is offered in the picker.
    expect(await screen.findByLabelText("remove team platform")).toBeInTheDocument();
    const teamSelect = await screen.findByLabelText("reviewer team");
    fireEvent.change(teamSelect, { target: { value: "security" } });
    await waitFor(() => {
      expect(findCall("/pulls/9/requested_reviewers", "POST")).toBeDefined();
    });
    const post = findCall("/pulls/9/requested_reviewers", "POST");
    expect(JSON.parse(String(post?.body))).toEqual({ reviewers: [], team_reviewers: ["security"] });

    fireEvent.click(screen.getByLabelText("remove team platform"));
    await waitFor(() => {
      expect(findCall("/pulls/9/requested_reviewers", "DELETE")).toBeDefined();
    });
    const del = findCall("/pulls/9/requested_reviewers", "DELETE");
    expect(JSON.parse(String(del?.body))).toEqual({ reviewers: [], team_reviewers: ["platform"] });
  });
});

describe("PullsPage combined status merge box", () => {
  it("renders commit-status contexts alongside check runs with a shared summary", async () => {
    mockPRApis((u) => {
      if (u.includes("/commits/abc/status")) {
        return jsonResponse({
          state: "failure",
          sha: "abc",
          total_count: 1,
          statuses: [
            { context: "ci/lint", state: "failure", description: "lint failed", target_url: null },
          ],
        });
      }
      if (u.includes("/commits/abc/check-runs")) {
        return jsonResponse({ total_count: 1, check_runs: [checkRun(1, "build")] });
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    // The merge box shows the shared failure summary on Conversation…
    expect(await screen.findByText(/some checks were not successful/i)).toBeInTheDocument();
    // …and the Checks tab lists the commit-status contexts + check runs.
    fireEvent.click(screen.getByRole("tab", { name: /^Checks/ }));
    expect(await screen.findByText(/ci\/lint/)).toBeInTheDocument();
    expect(screen.getByText(/lint failed/)).toBeInTheDocument();
    expect(screen.getByText("build")).toBeInTheDocument();
  });

  it("updates the PR branch via PUT /pulls/{n}/update-branch", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/update-branch") && init?.method === "PUT") {
        return jsonResponse({ message: "Updating pull request branch.", url: "" }, 202);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    fireEvent.click(await screen.findByRole("button", { name: "Update branch" }));
    await waitFor(() => {
      expect(findCall("/pulls/9/update-branch", "PUT")).toBeDefined();
    });
  });

  it("marks a draft PR ready for review via the GraphQL mutation", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9") && init?.method === undefined) {
        return jsonResponse(pr(9, "Draft PR", { draft: true, node_id: "PR_kwDO123" }));
      }
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        const body = JSON.parse(String(init.body ?? "{}")) as { query?: string };
        if (body.query?.includes("markPullRequestReadyForReview")) {
          return jsonResponse({ data: { markPullRequestReadyForReview: { clientMutationId: null } } });
        }
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    fireEvent.click(await screen.findByRole("button", { name: "Ready for review" }));
    await waitFor(() => {
      const readyCall = mockFetch.mock.calls.find(
        (c) =>
          c[0].toString().endsWith("/api/graphql") &&
          (c[1] as RequestInit | undefined)?.method === "POST" &&
          JSON.parse(String((c[1] as RequestInit).body)).query.includes("markPullRequestReadyForReview"),
      );
      expect(readyCall).toBeDefined();
      expect(JSON.parse(String((readyCall![1] as RequestInit).body)).variables.input).toEqual({
        pullRequestId: "PR_kwDO123",
      });
    });
  });
});

describe("PullsPage reactions", () => {
  it("toggles off my existing reaction via DELETE", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/issues/9/reactions") && init?.method === undefined) {
        return jsonResponse([
          { id: 5, content: "+1", user: { login: "admin" }, created_at: "2026-01-01T00:00:00Z" },
          { id: 6, content: "+1", user: { login: "bob" }, created_at: "2026-01-01T00:00:00Z" },
        ]);
      }
      if (u.endsWith("/issues/9/reactions/5") && init?.method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    const pill = await screen.findByRole("button", { name: "toggle +1 reaction" });
    expect(pill.textContent).toContain("2");
    fireEvent.click(pill);
    await waitFor(() => {
      expect(findCall("/issues/9/reactions/5", "DELETE")).toBeDefined();
    });
  });

  it("adds a reaction from the picker via POST", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/issues/9/reactions") && init?.method === "POST") {
        return jsonResponse(
          { id: 7, content: "heart", user: { login: "admin" }, created_at: "2026-01-01T00:00:00Z" },
          201,
        );
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    fireEvent.click(await screen.findByRole("button", { name: "add reaction" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "react with heart" }));
    await waitFor(() => {
      expect(findCall("/issues/9/reactions", "POST")).toBeDefined();
    });
    const init = findCall("/issues/9/reactions", "POST");
    expect(JSON.parse(String(init?.body))).toEqual({ content: "heart" });
  });
});

describe("PullsPage detail sub-tabs", () => {
  it("renders the Conversation/Commits/Files changed/Checks tabs and a Reviewers sidebar", async () => {
    mockPRApis();
    renderAt("/ui/repos/admin/test/pulls/9");
    await screen.findByText("Feature PR");
    expect(screen.getByRole("tab", { name: "Conversation" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Commits" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Files changed" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Checks" })).toBeInTheDocument();
    // The Reviewers section lives in the sidebar on the Conversation tab.
    expect(screen.getByText("Reviewers")).toBeInTheDocument();
  });

  it("loads the PR's changed files as a diff on the Files changed tab", async () => {
    mockPRApis((u) => {
      if (u.endsWith("/pulls/9/files")) {
        return jsonResponse([
          {
            sha: "abc",
            filename: "main.go",
            status: "modified",
            additions: 2,
            deletions: 1,
            changes: 3,
            patch: "@@ -1,2 +1,2 @@\n-old\n+new",
          },
        ]);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    await screen.findByText("Feature PR");
    fireEvent.click(screen.getByRole("tab", { name: "Files changed" }));
    expect(await screen.findByText("main.go")).toBeInTheDocument();
    expect(screen.getByText(/@@ -1,2 \+1,2 @@/)).toBeInTheDocument();
  });

  it("deep-links to Files changed and adds a correctly addressed inline comment", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/files")) {
        return jsonResponse([
          {
            sha: "abc",
            filename: "main.go",
            status: "modified",
            additions: 2,
            deletions: 1,
            changes: 3,
            patch: "@@ -7,2 +8,2 @@\n-old\n+new",
          },
        ]);
      }
      if (u.endsWith("/pulls/9/comments") && init?.method === "POST") {
        return jsonResponse({ id: 41, body: "Please explain this.", path: "main.go", line: 8 }, 201);
      }
      return undefined;
    });

    renderAt("/ui/repos/admin/test/pulls/9/files");
    await screen.findByText("main.go");
    expect(screen.getByRole("tab", { name: "Files changed" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Comment on main.go line 8" }));
    fireEvent.change(screen.getByLabelText("Review comment"), {
      target: { value: "Please explain this." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add single comment" }));

    await waitFor(() => expect(findCall("/pulls/9/comments", "POST")).toBeDefined());
    const init = findCall("/pulls/9/comments", "POST");
    expect(JSON.parse(String(init?.body))).toEqual({
      body: "Please explain this.",
      commit_id: "abc",
      path: "main.go",
      line: 8,
      side: "RIGHT",
    });
  });
});

describe("PullsPage merge box", () => {
  it("merges with the selected method (squash), sending the editable commit title", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/merge") && init?.method === "PUT") {
        return jsonResponse({ merged: true });
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    await screen.findByRole("heading", { name: /Feature PR/ });

    // GitHub's two-step flow: the merge button opens a confirmation panel
    // with the method-specific default commit title, then Confirm merges.
    fireEvent.change(screen.getByLabelText("Merge method"), { target: { value: "squash" } });
    fireEvent.click(screen.getByRole("button", { name: /^squash and merge$/i }));
    expect(screen.getByLabelText("Commit title")).toHaveValue("Feature PR (#9)");
    fireEvent.click(screen.getByRole("button", { name: /confirm squash and merge/i }));
    await waitFor(() => {
      expect(findCall("/pulls/9/merge", "PUT")).toBeDefined();
    });
    const init = findCall("/pulls/9/merge", "PUT");
    expect(JSON.parse(String(init?.body))).toEqual({
      merge_method: "squash",
      commit_title: "Feature PR (#9)",
    });
  });

  it("stays on the PR after merging and offers Delete branch in the merged box", async () => {
    let merged = false;
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9/merge") && init?.method === "PUT") {
        merged = true;
        return jsonResponse({ merged: true });
      }
      if (u.endsWith("/pulls/9") && init?.method === undefined) {
        return jsonResponse(pr(9, "Feature PR", { merged, merged_at: merged ? "2026-01-03T00:00:00Z" : null }));
      }
      if (u.includes("/branches?")) {
        return jsonResponse([{ name: "main" }, { name: "feature" }]);
      }
      if (u.endsWith("/git/refs/heads/feature") && init?.method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    await screen.findByRole("heading", { name: /Feature PR/ });

    fireEvent.click(screen.getByRole("button", { name: /^merge pull request$/i }));
    fireEvent.click(screen.getByRole("button", { name: /^confirm merge$/i }));

    // Refetch flips the page into the merged state without navigating away.
    expect(await screen.findByText(/successfully merged and closed/i)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Feature PR/ })).toBeInTheDocument();

    // The merged box offers deleting the (still existing) head branch.
    fireEvent.click(await screen.findByRole("button", { name: /^delete branch$/i }));
    await waitFor(() => {
      expect(findCall("/git/refs/heads/feature", "DELETE")).toBeDefined();
    });
  });

  it("offers Restore branch when the merged PR's head branch is gone", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9") && init?.method === undefined) {
        return jsonResponse(pr(9, "Feature PR", { merged: true, merged_at: "2026-01-03T00:00:00Z" }));
      }
      if (u.includes("/branches?")) {
        return jsonResponse([{ name: "main" }]);
      }
      if (u.endsWith("/git/refs") && init?.method === "POST") {
        return jsonResponse({ ref: "refs/heads/feature", object: { sha: "abc" } }, 201);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    fireEvent.click(await screen.findByRole("button", { name: /^restore branch$/i }));
    await waitFor(() => {
      expect(findCall("/git/refs", "POST")).toBeDefined();
    });
    expect(JSON.parse(String(findCall("/git/refs", "POST")?.body))).toEqual({
      ref: "refs/heads/feature",
      sha: "abc",
    });
  });
});

describe("PullsPage conversation timeline", () => {
  it("interleaves comments with label, review, and assignment events", async () => {
    mockPRApis((u) => {
      if (u.endsWith("/issues/9/timeline")) {
        return jsonResponse([
          {
            event: "commented",
            id: 21,
            user: { login: "admin", avatar_url: "" },
            body: "conversation comment",
            created_at: "2026-01-01T00:00:00Z",
          },
          {
            event: "labeled",
            id: 22,
            actor: { login: "admin", avatar_url: "" },
            label: { name: "bug", color: "ff0000" },
            created_at: "2026-01-01T01:00:00Z",
          },
          {
            event: "assigned",
            id: 23,
            actor: { login: "admin", avatar_url: "" },
            assignee: { login: "bob" },
            created_at: "2026-01-01T02:00:00Z",
          },
          {
            event: "reviewed",
            id: 24,
            user: { login: "alice", avatar_url: "" },
            state: "APPROVED",
            body: "",
            submitted_at: "2026-01-01T03:00:00Z",
          },
        ]);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    expect(await screen.findByText("conversation comment")).toBeInTheDocument();
    expect(screen.getByText(/added the/)).toBeInTheDocument();
    expect(screen.getByText("bug")).toBeInTheDocument();
    expect(screen.getByText(/assigned/)).toBeInTheDocument();
    expect(screen.getByText("bob")).toBeInTheDocument();
    expect(screen.getByText(/approved these changes/)).toBeInTheDocument();
    // alice appears in the review row and again in the sidebar participants
    // (reviewers count as participants).
    expect(screen.getAllByText("alice").length).toBeGreaterThanOrEqual(1);
  });
});

describe("PullsPage create", () => {
  it("opens a pull request through the New pull request modal", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls") && init?.method === "POST") {
        return Promise.resolve(jsonResponse(pr(42, "My PR")));
      }
      if (u.includes("/branches")) {
        return Promise.resolve(jsonResponse([{ name: "main" }, { name: "feature" }]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls");
    fireEvent.click(await screen.findByRole("button", { name: /new pull request/i }));
    // Wait for the branch options to load before selecting them (a controlled
    // <select> in jsdom drops a value with no matching option).
    await screen.findAllByRole("option", { name: "feature" });
    fireEvent.change(screen.getByLabelText(/^base$/i), { target: { value: "main" } });
    fireEvent.change(screen.getByLabelText(/^compare$/i), { target: { value: "feature" } });
    fireEvent.change(screen.getByLabelText(/^title$/i), { target: { value: "My PR" } });
    fireEvent.click(screen.getByRole("button", { name: /create pull request/i }));
    await waitFor(() => {
      const posted = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/pulls") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      expect(JSON.parse((posted![1] as RequestInit).body as string)).toEqual({
        title: "My PR",
        head: "feature",
        base: "main",
        body: "",
        draft: false,
      });
    });
  });

  it("creates a draft pull request when the draft checkbox is ticked", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls") && init?.method === "POST") {
        return Promise.resolve(jsonResponse(pr(43, "Draft PR", { draft: true })));
      }
      if (u.includes("/branches")) {
        return Promise.resolve(jsonResponse([{ name: "main" }, { name: "feature" }]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls");
    fireEvent.click(await screen.findByRole("button", { name: /new pull request/i }));
    await screen.findAllByRole("option", { name: "feature" });
    fireEvent.change(screen.getByLabelText(/^base$/i), { target: { value: "main" } });
    fireEvent.change(screen.getByLabelText(/^compare$/i), { target: { value: "feature" } });
    fireEvent.change(screen.getByLabelText(/^title$/i), { target: { value: "Draft PR" } });
    fireEvent.click(screen.getByLabelText(/create as a draft/i));
    fireEvent.click(screen.getByRole("button", { name: /create draft pull request/i }));
    await waitFor(() => {
      const posted = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/pulls") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      expect(JSON.parse((posted![1] as RequestInit).body as string).draft).toBe(true);
    });
  });
});

describe("PullsPage write actions", () => {
  it("closes a pull request", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9") && init?.method === "PATCH") {
        return jsonResponse(pr(9, "Feature PR", { state: "closed" }));
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    fireEvent.click(await screen.findByRole("button", { name: /close pull request/i }));
    await waitFor(() => expect(findCall("/pulls/9", "PATCH")).toBeDefined());
    expect(JSON.parse(String(findCall("/pulls/9", "PATCH")?.body))).toEqual({ state: "closed" });
  });

  it("edits a pull request title and body", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9") && init?.method === "PATCH") {
        return jsonResponse(pr(9, "Renamed", { body: "new" }));
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
    fireEvent.change(await screen.findByLabelText(/title/i), { target: { value: "Renamed" } });
    fireEvent.change(screen.getByLabelText(/description/i), { target: { value: "new" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(findCall("/pulls/9", "PATCH")).toBeDefined());
    expect(JSON.parse(String(findCall("/pulls/9", "PATCH")?.body))).toEqual({
      title: "Renamed",
      body: "new",
    });
  });

  it("comments on the pull request conversation", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/issues/9/comments") && init?.method === "POST") {
        return jsonResponse({ id: 1, body: "nice", user: { login: "admin" }, created_at: "2026-01-02T00:00:00Z" });
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    const box = await screen.findByPlaceholderText(/leave a comment/i);
    fireEvent.change(box, { target: { value: "nice" } });
    // The composer is now a MarkdownComposer (Write/Preview + toolbar), so the
    // textarea's nearest div is an inner tabpanel — find the submit button by
    // its exact accessible name instead.
    fireEvent.click(screen.getByRole("button", { name: /^comment$/i }));
    await waitFor(() => expect(findCall("/issues/9/comments", "POST")).toBeDefined());
    expect(JSON.parse(String(findCall("/issues/9/comments", "POST")?.body))).toEqual({ body: "nice" });
  });
});

describe("PullsPage detail field completeness", () => {
  it("renders the assignee, milestone, and files/commits counts from the PR response", async () => {
    mockPRApis((u) => {
      if (u.endsWith("/pulls/9")) {
        return jsonResponse(
          pr(9, "Rich PR", {
            assignees: [{ login: "octocat" }],
            milestone: { number: 4, title: "v1.0", state: "open" },
            commits: 3,
            changed_files: 5,
            additions: 42,
            deletions: 7,
          }),
        );
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    // Assignee login surfaces in the sidebar.
    expect(await screen.findByText("octocat")).toBeInTheDocument();
    // Milestone title surfaces in the sidebar.
    expect(screen.getByText("v1.0")).toBeInTheDocument();
    // Counts surface in the tab labels.
    expect(screen.getByRole("tab", { name: /Commits 3/ })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: /Files changed 5/ })).toBeInTheDocument();
    // Diffstat summary near the header.
    expect(screen.getByText(/5 changed files/)).toBeInTheDocument();
    expect(screen.getByText("+42")).toBeInTheDocument();
    expect(screen.getByText("−7")).toBeInTheDocument();
  });
});

describe("PullsPage development / closing issues", () => {
  it("renders closing issue references as links in the Development section", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        const body = JSON.parse(String(init.body ?? "{}")) as { query?: string };
        if (body.query?.includes("closingIssuesReferences")) {
          return jsonResponse({
            data: {
              repository: {
                pullRequest: {
                  closingIssuesReferences: {
                    nodes: [
                      {
                        number: 5,
                        title: "Fix the bug",
                        state: "OPEN",
                        url: "http://x/issues/5",
                      },
                    ],
                  },
                },
              },
            },
          });
        }
        return undefined;
      }
      return undefined;
    });

    renderAt("/ui/repos/admin/test/pulls/9");

    const link = await screen.findByRole("link", { name: /#5\s+Fix the bug/ });
    expect(link).toHaveAttribute("href", "/ui/repos/admin/test/issues/5");
  });
});

describe("PullsPage commits tab", () => {
  it("links each commit SHA to the commit page with author avatar context", async () => {
    mockPRApis((u) => {
      if (u.endsWith("/pulls/9/commits")) {
        return jsonResponse([
          {
            sha: "abcdef1234567890",
            commit: {
              message: "Add feature\n\nlong body",
              author: { name: "Ada", date: "2026-01-05T00:00:00Z" },
            },
            author: { login: "ada", avatar_url: "" },
          },
        ]);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9/commits");

    expect(await screen.findByText("Add feature")).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "abcdef1" });
    expect(link).toHaveAttribute("href", "/ui/repos/admin/test/commits/abcdef1234567890");
    expect(screen.getByText(/ada committed/)).toBeInTheDocument();
  });
});

describe("PullsPage unified conversation stream", () => {
  it("renders each review once, chronologically, with inline threads nested in its card", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/issues/9/timeline")) {
        return jsonResponse([
          {
            event: "commented",
            id: 21,
            user: { login: "admin", avatar_url: "" },
            body: "later comment",
            created_at: "2026-01-02T00:00:00Z",
          },
          {
            event: "reviewed",
            id: 7,
            user: { login: "alice", avatar_url: "" },
            state: "APPROVED",
            body: "ship it",
            submitted_at: "2026-01-01T00:00:00Z",
          },
        ]);
      }
      if (u.endsWith("/pulls/9/reviews") && init?.method === undefined) {
        return jsonResponse([
          review(7, "alice", "APPROVED", { body: "ship it", submitted_at: "2026-01-01T00:00:00Z" }),
        ]);
      }
      if (u.endsWith("/pulls/9/comments") && init?.method === undefined) {
        return jsonResponse([
          {
            id: 31,
            pull_request_review_id: 7,
            diff_hunk: "@@ -1 +1 @@\n+x",
            path: "main.go",
            line: 1,
            side: "RIGHT",
            body: "inline note",
            user: { login: "alice", avatar_url: "" },
            created_at: "2026-01-01T00:00:00Z",
            updated_at: "2026-01-01T00:00:00Z",
          },
        ]);
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    expect(await screen.findByText("ship it")).toBeInTheDocument();
    // The timeline "reviewed" row and the reviews-endpoint copy dedupe into
    // exactly one review card…
    expect(screen.getAllByText("Approved").length).toBe(1);
    // …with the review's inline thread nested inside it — no separate stacked
    // "Review comments" / "Reviews" sections anymore.
    expect(screen.getByText("inline note")).toBeInTheDocument();
    expect(screen.queryByText("Review comments")).not.toBeInTheDocument();
    expect(screen.queryByText(/^Reviews$/)).not.toBeInTheDocument();
    // Chronology: the Jan-1 review precedes the Jan-2 conversation comment.
    const pos = screen
      .getByText("ship it")
      .compareDocumentPosition(screen.getByText("later comment"));
    expect(pos & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

describe("PullsPage list filters", () => {
  function listMock(prs: unknown[]) {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/pulls?")) return Promise.resolve(jsonResponse(prs));
      return Promise.resolve(jsonResponse([]));
    });
  }

  it("filters by assignee and milestone from the list payload", async () => {
    listMock([
      pr(1, "assigned pr", {
        assignees: [{ login: "carol" }],
        milestone: { number: 1, title: "v1", state: "open" },
      }),
      pr(2, "other pr"),
    ]);
    renderAt("/ui/repos/admin/test/pulls");
    await screen.findByText("assigned pr");

    fireEvent.change(screen.getByLabelText("Assignee"), { target: { value: "carol" } });
    await waitFor(() => expect(screen.queryByText("other pr")).not.toBeInTheDocument());
    expect(screen.getByText("assigned pr")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Assignee"), { target: { value: "" } });
    fireEvent.change(screen.getByLabelText("Milestones"), { target: { value: "v1" } });
    await waitFor(() => expect(screen.queryByText("other pr")).not.toBeInTheDocument());
    expect(screen.getByText("assigned pr")).toBeInTheDocument();
  });

  it('maps the "Most commented" sort to the pulls endpoint\'s popularity sort', async () => {
    listMock([pr(1, "first pr")]);
    renderAt("/ui/repos/admin/test/pulls");
    await screen.findByText("first pr");

    fireEvent.change(screen.getByLabelText("Sort"), { target: { value: "comments" } });
    await waitFor(() => {
      const calls = mockFetch.mock.calls.map((c) => c[0]!.toString());
      expect(calls.some((u) => u.includes("/pulls?") && u.includes("sort=popularity"))).toBe(true);
    });
    // GitHub's /pulls endpoint rejects sort=comments (that's the issues
    // dialect) — it must never be sent.
    const calls = mockFetch.mock.calls.map((c) => c[0]!.toString());
    expect(calls.some((u) => u.includes("sort=comments"))).toBe(false);
  });
});

describe("PullsPage compare deep-link", () => {
  it("opens the create-PR modal prefilled from ?compare=base...head", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls") && init?.method === "POST") {
        return Promise.resolve(jsonResponse(pr(50, "From compare")));
      }
      if (u.includes("/branches")) {
        return Promise.resolve(jsonResponse([{ name: "main" }, { name: "feature" }]));
      }
      if (u.includes("/issues")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls?compare=main...feature");

    // The modal opens on its own with base/head prefilled once the branch
    // options load.
    await screen.findAllByRole("option", { name: "feature" });
    expect(screen.getByLabelText(/^base$/i)).toHaveValue("main");
    expect(screen.getByLabelText(/^compare$/i)).toHaveValue("feature");

    fireEvent.change(screen.getByLabelText(/^title$/i), { target: { value: "From compare" } });
    fireEvent.click(screen.getByRole("button", { name: /^create pull request$/i }));
    await waitFor(() => {
      const posted = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/pulls") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      expect(JSON.parse((posted![1] as RequestInit).body as string)).toMatchObject({
        head: "feature",
        base: "main",
      });
    });
  });
});

describe("PullsPage detail bootstrap", () => {
  it("hydrates the conversation from the pull bootstrap with no standalone refetches", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/ui-data/bootstrap/repos/admin/test/pulls/9")) {
        return Promise.resolve(
          jsonResponse({
            pull: pr(9, "Bootstrapped PR"),
            timeline: [],
            comments: [],
            reviews: [],
            review_comments: [],
            requested_reviewers: emptyReviewers,
            check_runs: noChecks,
            combined_status: emptyStatus,
            files_summary: { changed_files: 1, additions: 2, deletions: 0 },
            labels: [{ id: 1, name: "bug", color: "ff0000", description: "" }],
            milestones: [{ number: 4, title: "v1.0", state: "open" }],
            assignees_available: [{ login: "admin" }, { login: "carol" }],
          }),
        );
      }
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({ data: { repository: { pullRequest: { reviewThreads: { nodes: [] } } } } }),
        );
      }
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse(viewer));
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    expect(await screen.findByText("Bootstrapped PR")).toBeInTheDocument();

    // Every sub-payload the bootstrap carried must be a cache hit — none of
    // the standalone endpoints those hooks call may have been fetched.
    const gets = mockFetch.mock.calls
      .filter((c) => (c[1] as RequestInit | undefined)?.method === undefined)
      .map((c) => c[0]!.toString());
    expect(gets.some((u) => u.includes("/api/v3/") && u.endsWith("/pulls/9"))).toBe(false);
    expect(gets.some((u) => u.includes("/pulls/9/reviews"))).toBe(false);
    expect(gets.some((u) => u.includes("/pulls/9/comments"))).toBe(false);
    expect(gets.some((u) => u.includes("/issues/9/timeline"))).toBe(false);
    expect(gets.some((u) => u.includes("/pulls/9/requested_reviewers"))).toBe(false);
    expect(gets.some((u) => u.includes("/check-runs"))).toBe(false);
    expect(gets.some((u) => u.includes("/commits/abc/status"))).toBe(false);
  });

  it("seeds the sidebar labels/milestones/assignees keys from the bootstrap", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/ui-data/bootstrap/repos/admin/test/pulls/9")) {
        return Promise.resolve(
          jsonResponse({
            pull: pr(9, "Bootstrapped PR"),
            timeline: [],
            comments: [],
            reviews: [],
            review_comments: [],
            requested_reviewers: emptyReviewers,
            check_runs: noChecks,
            combined_status: emptyStatus,
            files_summary: { changed_files: 1, additions: 2, deletions: 0 },
            labels: [{ id: 1, name: "bug", color: "ff0000", description: "" }],
            milestones: [{ number: 4, title: "v1.0", state: "open" }],
            assignees_available: [{ login: "admin" }, { login: "carol" }],
          }),
        );
      }
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({ data: { repository: { pullRequest: { reviewThreads: { nodes: [] } } } } }),
        );
      }
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse(viewer));
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    expect(await screen.findByText("Bootstrapped PR")).toBeInTheDocument();

    // The sidebar consumes the seeded entries: the assignee picker offers the
    // bootstrap logins and the label picker the bootstrap labels (the PR
    // milestone field is read-only, so its seed is only assertable via the
    // absence of the standalone fetch below).
    // (carol appears in both the assignee picker and the reviewers picker —
    // both read the seeded ["assignable-users"] key.)
    expect((await screen.findAllByRole("option", { name: "carol" })).length).toBeGreaterThanOrEqual(1);
    expect(screen.getByRole("option", { name: "bug" })).toBeInTheDocument();
    // …without any standalone sidebar fetches.
    const gets = mockFetch.mock.calls
      .filter((c) => (c[1] as RequestInit | undefined)?.method === undefined)
      .map((c) => c[0]!.toString());
    expect(gets.some((u) => u.includes("/api/v3/repos/admin/test/labels"))).toBe(false);
    expect(gets.some((u) => u.includes("/api/v3/repos/admin/test/milestones"))).toBe(false);
    expect(gets.some((u) => u.endsWith("/api/v3/repos/admin/test/assignees"))).toBe(false);
  });

  it("falls back to the standalone endpoints when the bootstrap answers 500", async () => {
    mockPRApis((u) => {
      if (u.includes("/ui-data/bootstrap/")) return jsonResponse({ message: "boom" }, 500);
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    expect(await screen.findByText("Feature PR")).toBeInTheDocument();
    const gets = mockFetch.mock.calls
      .filter((c) => (c[1] as RequestInit | undefined)?.method === undefined)
      .map((c) => c[0]!.toString());
    expect(gets.some((u) => u.includes("/api/v3/") && u.endsWith("/pulls/9"))).toBe(true);
  });
});

describe("PullsPage auto-merge", () => {
  const autoMergeRepo = {
    id: 1,
    name: "test",
    full_name: "admin/test",
    default_branch: "main",
    owner: { login: "admin", type: "User" },
    allow_auto_merge: true,
    permissions: adminPerms,
  };

  function findGraphQL(mutationName: string) {
    return mockFetch.mock.calls.find(
      (c) =>
        c[0].toString().endsWith("/api/graphql") &&
        (c[1] as RequestInit | undefined)?.method === "POST" &&
        String((c[1] as RequestInit).body ?? "").includes(mutationName),
    );
  }

  function mockBlockedAutoMergePR(
    graphqlAnswer: (query: string) => Response | undefined,
  ) {
    mockPRApis((u, init) => {
      if (u.endsWith("/repos/admin/test") && init?.method === undefined) {
        return jsonResponse(autoMergeRepo);
      }
      if (u.endsWith("/pulls/9") && init?.method === undefined) {
        return jsonResponse(
          pr(9, "Blocked PR", { mergeable_state: "blocked", node_id: "PR_kwDO123" }),
        );
      }
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        const body = JSON.parse(String(init.body ?? "{}")) as { query?: string };
        return graphqlAnswer(body.query ?? "");
      }
      return undefined;
    });
  }

  it("enables auto-merge on a blocked PR with the chosen method", async () => {
    mockBlockedAutoMergePR((query) =>
      query.includes("enablePullRequestAutoMerge")
        ? jsonResponse({ data: { enablePullRequestAutoMerge: { clientMutationId: null } } })
        : undefined,
    );
    renderAt("/ui/repos/admin/test/pulls/9");

    fireEvent.change(await screen.findByLabelText("Merge method"), {
      target: { value: "squash" },
    });
    fireEvent.click(await screen.findByRole("button", { name: /^enable auto-merge$/i }));
    // The confirmation panel reuses the merge box's commit title/message
    // fields, prefilled with the method-specific defaults.
    expect(screen.getByLabelText("Commit title")).toHaveValue("Blocked PR (#9)");
    fireEvent.click(screen.getByRole("button", { name: /^confirm auto-merge$/i }));

    await waitFor(() => expect(findGraphQL("enablePullRequestAutoMerge")).toBeDefined());
    const body = JSON.parse(
      String((findGraphQL("enablePullRequestAutoMerge")![1] as RequestInit).body),
    ) as { variables: { input: unknown } };
    expect(body.variables.input).toEqual({
      pullRequestId: "PR_kwDO123",
      mergeMethod: "SQUASH",
      commitHeadline: "Blocked PR (#9)",
    });
  });

  it("renders the clean-status refusal inline instead of crashing", async () => {
    mockBlockedAutoMergePR((query) =>
      query.includes("enablePullRequestAutoMerge")
        ? jsonResponse({ errors: [{ message: "Pull request is in clean status" }] })
        : undefined,
    );
    renderAt("/ui/repos/admin/test/pulls/9");

    fireEvent.click(await screen.findByRole("button", { name: /^enable auto-merge$/i }));
    fireEvent.click(screen.getByRole("button", { name: /^confirm auto-merge$/i }));

    expect(await screen.findByText(/pull request is in clean status/i)).toBeInTheDocument();
  });

  it("shows the armed state and disables auto-merge via GraphQL", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/pulls/9") && init?.method === undefined) {
        return jsonResponse(
          pr(9, "Armed PR", {
            node_id: "PR_kwDO123",
            auto_merge: {
              enabled_by: { login: "alice" },
              merge_method: "squash",
              commit_title: "Armed PR (#9)",
              commit_message: "",
            },
          }),
        );
      }
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        const body = JSON.parse(String(init.body ?? "{}")) as { query?: string };
        if (body.query?.includes("disablePullRequestAutoMerge")) {
          return jsonResponse({ data: { disablePullRequestAutoMerge: { clientMutationId: null } } });
        }
        return undefined;
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    expect(
      await screen.findByText(/auto-merge enabled by alice \(squash\)/i),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^disable auto-merge$/i }));
    await waitFor(() => expect(findGraphQL("disablePullRequestAutoMerge")).toBeDefined());
    const body = JSON.parse(
      String((findGraphQL("disablePullRequestAutoMerge")![1] as RequestInit).body),
    ) as { variables: { input: unknown } };
    expect(body.variables.input).toEqual({ pullRequestId: "PR_kwDO123" });
  });

  it("hides the enable affordance when the repo does not allow auto-merge", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/repos/admin/test") && init?.method === undefined) {
        return jsonResponse({ ...autoMergeRepo, allow_auto_merge: false });
      }
      if (u.endsWith("/pulls/9") && init?.method === undefined) {
        return jsonResponse(
          pr(9, "Blocked PR", { mergeable_state: "blocked", node_id: "PR_kwDO123" }),
        );
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");
    expect(
      await screen.findByText(/merging is blocked — required checks must pass/i),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /enable auto-merge/i })).not.toBeInTheDocument();
  });
});

// ─── Viewer-role gating (pull-only outsider vs PR author) ─────────────────

describe("PullsPage viewer-role gating", () => {
  const readerRepo = { ...adminRepo, permissions: { admin: false, push: false, pull: true } };
  const reader = { id: 2, login: "reader", avatar_url: "", type: "User", site_admin: false };

  it("replaces the merge actions with the write-access notice for a pull-only viewer", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/api/v3/repos/admin/test") && init?.method === undefined) {
        return jsonResponse(readerRepo);
      }
      if (u.endsWith("/api/v3/user")) return jsonResponse(reader);
      if (u.endsWith("/pulls/9/requested_reviewers") && init?.method === undefined) {
        return jsonResponse({
          users: [{ id: 3, login: "carol", avatar_url: "", type: "User", site_admin: false }],
          teams: [],
        });
      }
      if (u.endsWith("/assignees")) return jsonResponse([{ login: "bob" }]);
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    // github.com's exact read-only merge-box notice…
    expect(
      await screen.findByText("Only those with write access to this repository can merge pull requests."),
    ).toBeInTheDocument();
    // …while the conflict/checks status display stays visible.
    expect(screen.getByText(/no conflicts with the base branch/i)).toBeInTheDocument();

    // The merge action row is gone.
    expect(screen.queryByRole("button", { name: /merge pull request/i })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Merge method")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update branch" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Convert to draft" })).not.toBeInTheDocument();

    // Close/edit follow author-or-write; the reader is neither.
    expect(screen.queryByRole("button", { name: /close pull request/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^edit$/i })).not.toBeInTheDocument();

    // Reviewers render as read-only chips: no request/re-request/remove.
    expect(await screen.findByText("carol")).toBeInTheDocument();
    expect(screen.queryByLabelText("remove reviewer carol")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("reviewer login")).not.toBeInTheDocument();

    // Sidebar pickers hidden; commenting stays for everyone.
    expect(screen.queryByLabelText("Add assignee")).not.toBeInTheDocument();
    expect(screen.getByPlaceholderText(/leave a comment/i)).toBeInTheDocument();
  });

  it("keeps Ready for review, Close and Edit for the PR author without push", async () => {
    mockPRApis((u, init) => {
      if (u.endsWith("/api/v3/repos/admin/test") && init?.method === undefined) {
        return jsonResponse(readerRepo);
      }
      // The default /api/v3/user viewer is "admin" — the PR author.
      if (u.endsWith("/pulls/9") && init?.method === undefined) {
        return jsonResponse(pr(9, "Draft PR", { draft: true, node_id: "PR_kwDO123" }));
      }
      return undefined;
    });
    renderAt("/ui/repos/admin/test/pulls/9");

    expect(await screen.findByRole("button", { name: "Ready for review" })).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: /close pull request/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^edit$/i })).toBeInTheDocument();
    // Authorship still does not unlock merging.
    expect(
      screen.getByText("Only those with write access to this repository can merge pull requests."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /merge pull request/i })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Merge method")).not.toBeInTheDocument();
  });
});
