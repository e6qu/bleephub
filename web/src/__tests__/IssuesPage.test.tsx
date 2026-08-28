import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { IssuesPage } from "../pages/IssuesPage.js";

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
  // Comment drafts persist in sessionStorage; keep tests independent.
  sessionStorage.clear();
});

function renderAt(path: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/:owner/:repo/issues" element={<IssuesPage />} />
          <Route path="/ui/:owner/:repo/issues/:number" element={<IssuesPage />} />
          <Route path="/ui/:owner/:repo/labels" element={<IssuesPage view="labels" />} />
          <Route path="/ui/:owner/:repo/milestones" element={<IssuesPage view="milestones" />} />
          {/* Marker for navigations out of the issues page (convert-to-discussion). */}
          <Route
            path="/ui/:owner/:repo/discussions/:number"
            element={<div>discussion detail route</div>}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...utils, queryClient };
}

// Admin fixture carries full access so write affordances render; viewer gating reads these permissions.
const adminPerms = { admin: true, push: true, pull: true };
const adminRepo = { id: 1, name: "test", full_name: "admin/test", owner: { login: "admin", type: "User" }, permissions: adminPerms };

function issue(number: number, title: string) {
  return {
    id: number,
    node_id: `I_kwDO${number.toString().padStart(8, "0")}`,
    number,
    title,
    body: "body",
    state: "open",
    user: { login: "admin", avatar_url: "" },
    labels: [],
    assignees: [],
    comments: 0,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    closed_at: null,
  };
}

describe("IssuesPage detail", () => {
  it("shows a not-found state (not a spinner) for a missing issue", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues/999")) {
        return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/999");
    await waitFor(() => {
      expect(screen.getByText(/issue #999 not found/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/loading issue/i)).not.toBeInTheDocument();
  });

  it("shows an error state when the issue fetch fails", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues/7")) {
        return Promise.resolve(jsonResponse({ message: "boom" }, 500));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    await waitFor(() => {
      expect(screen.getByText(/failed to load issue #7/i)).toBeInTheDocument();
    });
  });

  it("renders the issue when found", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    await waitFor(() => {
      expect(screen.getByText("A real issue")).toBeInTheDocument();
    });
  });

  it("shows closing pull requests in the Development section", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (String(body.query).includes("closedByPullRequestsReferences")) {
          return Promise.resolve(jsonResponse({
            data: { repository: { issue: { closedByPullRequestsReferences: { nodes: [{ number: 42, title: "Fix it", state: "OPEN" }] } } } },
          }));
        }
        return Promise.resolve(jsonResponse({ data: {} }));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    const link = await screen.findByRole("link", { name: /#42 Fix it/ });
    expect(link).toHaveAttribute("href", "/ui/admin/test/pulls/42");
  });

  it("interleaves timeline events with comments in the conversation", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues/7/timeline")) {
        return Promise.resolve(jsonResponse([
          { event: "labeled", actor: { login: "admin" }, label: { name: "bug", color: "d73a4a" }, created_at: "2026-01-02T00:00:00Z" },
          { event: "commented", id: 100, body: "on it", user: { login: "admin" }, created_at: "2026-01-03T00:00:00Z" },
          { event: "cross-referenced", actor: { login: "admin" }, created_at: "2026-01-03T12:00:00Z", source: { type: "issue", issue: { number: 12, title: "related work" } } },
          { event: "closed", actor: { login: "admin" }, created_at: "2026-01-04T00:00:00Z" },
        ]));
      }
      if (u.includes("/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    expect(await screen.findByText(/added the/)).toBeInTheDocument();
    expect(screen.getByText("bug")).toBeInTheDocument();
    expect(screen.getByText("on it")).toBeInTheDocument();
    expect(screen.getByText(/mentioned this in/)).toBeInTheDocument();
    expect(screen.getByText(/#12 related work/)).toBeInTheDocument();
    expect(screen.getByText(/closed/)).toBeInTheDocument();
  });

  it("hides a comment via the minimizeComment mutation", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (String(body.query).includes("minimizeComment")) {
          return Promise.resolve(jsonResponse({ data: { minimizeComment: { minimizedComment: { isMinimized: true } } } }));
        }
        // minimization-state query: comment 100 not yet minimized.
        return Promise.resolve(jsonResponse({
          data: { repository: { issue: { comments: { nodes: [{ databaseId: 100, isMinimized: false, minimizedReason: null }] } } } },
        }));
      }
      if (u.includes("/issues/7/timeline")) {
        return Promise.resolve(jsonResponse([
          { event: "commented", id: 100, node_id: "IC_kgDO00000064", body: "spammy", user: { login: "admin" }, created_at: "2026-01-03T00:00:00Z" },
        ]));
      }
      if (u.includes("/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");

    fireEvent.click(await screen.findByRole("button", { name: "Hide comment" }));
    fireEvent.change(screen.getByLabelText("Reason for hiding"), { target: { value: "SPAM" } });
    fireEvent.click(screen.getByRole("button", { name: "Hide" }));

    await waitFor(() => {
      const mut = mockFetch.mock.calls.find(
        ([u2, i2]) => u2.toString().includes("/api/graphql") && String((i2 as RequestInit)?.body).includes("minimizeComment"),
      );
      expect(mut).toBeTruthy();
      const vars = JSON.parse(String((mut![1] as RequestInit).body)).variables;
      expect(vars.input).toMatchObject({ subjectId: "IC_kgDO00000064", classifier: "SPAM" });
    });
  });

  it("adds a sub-issue by number via POST /issues/{n}/sub_issues", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7/sub_issues") && init?.method === "POST") {
        return Promise.resolve(jsonResponse(issue(7, "A real issue"), 201));
      }
      if (u.endsWith("/issues/7/sub_issues")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.endsWith("/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      if (u.endsWith("/issues/9")) return Promise.resolve(jsonResponse(issue(9, "Child issue")));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");

    fireEvent.change(await screen.findByLabelText("sub-issue number"), { target: { value: "9" } });
    fireEvent.click(screen.getByRole("button", { name: "Add sub-issue" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7/sub_issues") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ sub_issue_id: 9 });
    });
  });

  it("posts a comment through the composer", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7/comments") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse(
            { id: 1, body: "looks good", user: { login: "admin" }, created_at: "2026-01-02T00:00:00Z" },
            201,
          ),
        );
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    const box = await screen.findByPlaceholderText(/leave a comment/i);
    fireEvent.change(box, { target: { value: "looks good" } });
    fireEvent.click(screen.getByRole("button", { name: /^comment$/i }));
    await waitFor(() => {
      const posted = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7/comments") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      expect(JSON.parse((posted![1] as RequestInit).body as string)).toEqual({ body: "looks good" });
    });
  });

  it("restores a comment draft after leaving and returning; posting clears it", async () => {
    const impl = (url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7/comments") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse(
            { id: 1, body: "half-typed thought", user: { login: "admin" }, created_at: "2026-01-02T00:00:00Z" },
            201,
          ),
        );
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    };
    mockFetch.mockImplementation(impl);
    const first = renderAt("/ui/admin/test/issues/7");
    const box = await screen.findByPlaceholderText(/leave a comment/i);
    fireEvent.change(box, { target: { value: "half-typed thought" } });
    // Navigating away unmounts the page; the draft stays in sessionStorage.
    first.unmount();
    expect(sessionStorage.getItem("bleephub:draft:issue-comment:admin/test/7")).toBe(
      "half-typed thought",
    );

    mockFetch.mockImplementation(impl);
    renderAt("/ui/admin/test/issues/7");
    const restored = await screen.findByPlaceholderText(/leave a comment/i);
    await waitFor(() => {
      expect((restored as HTMLTextAreaElement).value).toBe("half-typed thought");
    });

    fireEvent.click(screen.getByRole("button", { name: /^comment$/i }));
    await waitFor(() => {
      const posted = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7/comments") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      expect(sessionStorage.getItem("bleephub:draft:issue-comment:admin/test/7")).toBeNull();
    });
  });

  it("closes an open issue via the Close button", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7") && init?.method === "PATCH") {
        return Promise.resolve(jsonResponse({ ...issue(7, "A real issue"), state: "closed" }));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    const closeBtn = await screen.findByRole("button", { name: /close issue/i });
    fireEvent.click(closeBtn);
    await waitFor(() => {
      const patched = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7") && c[1]?.method === "PATCH",
      );
      expect(patched).toBeTruthy();
      expect(JSON.parse((patched![1] as RequestInit).body as string)).toEqual({
        state: "closed",
        state_reason: "completed",
      });
    });
  });

  it("closes an open issue with the selected reason", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7") && init?.method === "PATCH") {
        return Promise.resolve(
          jsonResponse({ ...issue(7, "A real issue"), state: "closed", state_reason: "not_planned" }),
        );
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.change(await screen.findByLabelText("Reason for closing"), { target: { value: "not_planned" } });
    fireEvent.click(screen.getByRole("button", { name: /close issue/i }));
    await waitFor(() => {
      const patched = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7") && c[1]?.method === "PATCH",
      );
      expect(patched).toBeTruthy();
      expect(JSON.parse((patched![1] as RequestInit).body as string)).toEqual({
        state: "closed",
        state_reason: "not_planned",
      });
    });
  });

  it("edits the issue title and body", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7") && init?.method === "PATCH") {
        return Promise.resolve(jsonResponse({ ...issue(7, "Renamed"), body: "New body" }));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
    const title = await screen.findByLabelText(/title/i);
    fireEvent.change(title, { target: { value: "Renamed" } });
    fireEvent.change(screen.getByLabelText(/description/i), { target: { value: "New body" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => {
      const patched = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7") && c[1]?.method === "PATCH",
      );
      expect(patched).toBeTruthy();
      expect(JSON.parse((patched![1] as RequestInit).body as string)).toEqual({
        title: "Renamed",
        body: "New body",
      });
    });
  });

  const withComment = () => ({
    id: 100,
    body: "old comment",
    user: { login: "admin" },
    created_at: "2026-01-02T00:00:00Z",
  });

  it("edits a comment in place", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/comments/100") && init?.method === "PATCH") {
        return Promise.resolve(jsonResponse({ ...withComment(), body: "edited" }));
      }
      if (u.includes("/issues/7/timeline")) return Promise.resolve(jsonResponse([{ ...withComment(), event: "commented" }]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: /edit comment/i }));
    const box = await screen.findByLabelText(/edit comment/i);
    fireEvent.change(box, { target: { value: "edited" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => {
      const patched = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/comments/100") && c[1]?.method === "PATCH",
      );
      expect(patched).toBeTruthy();
      expect(JSON.parse((patched![1] as RequestInit).body as string)).toEqual({ body: "edited" });
    });
  });

  it("deletes a comment after confirmation", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/comments/100") && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("/issues/7/timeline")) return Promise.resolve(jsonResponse([{ ...withComment(), event: "commented" }]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: /delete comment/i }));
    // confirmAction modal renders a "Delete" confirm button
    fireEvent.click(await screen.findByRole("button", { name: /^delete$/i }));
    await waitFor(() => {
      const deleted = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/comments/100") && c[1]?.method === "DELETE",
      );
      expect(deleted).toBeTruthy();
    });
  });

  it("assigns a user through the sidebar", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7/assignees") && init?.method === "POST") {
        return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      }
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      if (u.endsWith("/repos/admin/test/assignees")) {
        return Promise.resolve(jsonResponse([{ login: "bob" }]));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    const select = await screen.findByLabelText(/add assignee/i);
    fireEvent.change(select, { target: { value: "bob" } });
    await waitFor(() => {
      const posted = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7/assignees") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      expect(JSON.parse((posted![1] as RequestInit).body as string)).toEqual({ assignees: ["bob"] });
    });
  });

  it("locks the conversation from the sidebar", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7/lock") && init?.method === "PUT") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: /lock conversation/i }));
    await waitFor(() => {
      const locked = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7/lock") && c[1]?.method === "PUT",
      );
      expect(locked).toBeTruthy();
    });
  });
});

describe("IssuesPage list pagination", () => {
  it("shows Load more when the server advertises a next page, and appends it", async () => {
    const page2Url = "/api/v3/repos/admin/test/issues?state=open&per_page=50&page=2";
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("page=2")) {
        return Promise.resolve(jsonResponse([issue(3, "third issue")]));
      }
      if (u.includes("/issues?")) {
        return Promise.resolve(
          jsonResponse([issue(1, "first issue"), issue(2, "second issue")], 200, {
            Link: `<${page2Url}>; rel="next"`,
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");

    await waitFor(() => {
      expect(screen.getByText("first issue")).toBeInTheDocument();
    });
    const loadMore = screen.getByRole("button", { name: /load more/i });
    fireEvent.click(loadMore);
    await waitFor(() => {
      expect(screen.getByText("third issue")).toBeInTheDocument();
    });
    // page 2 was fetched via the Link rel="next" URL the server advertised
    const calls = mockFetch.mock.calls.map((c) => c[0].toString());
    expect(calls).toContain(page2Url);
  });

  it("renders an honest N+ badge when the open count is truncated by paging", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues?")) {
        return Promise.resolve(
          jsonResponse([issue(1, "first issue"), issue(2, "second issue")], 200, {
            Link: `</api/v3/repos/admin/test/issues?state=open&per_page=50&page=2>; rel="next"`,
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => {
      expect(screen.getByText("2+")).toBeInTheDocument();
    });
  });
});

describe("IssuesPage list filter bar", () => {
  function issueWith(number: number, title: string, overrides: Record<string, unknown>) {
    return { ...issue(number, title), ...overrides };
  }

  it("shows the Open/Closed count header and filters by label via the dropdown", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("state=closed")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues?")) {
        return Promise.resolve(
          jsonResponse([
            issueWith(1, "bug issue", { labels: [{ name: "bug", color: "d73a4a" }] }),
            issueWith(2, "plain issue", { labels: [] }),
          ]),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");

    await waitFor(() => expect(screen.getByText("bug issue")).toBeInTheDocument());
    expect(screen.getByText(/Open$/)).toBeInTheDocument();
    expect(screen.getByText(/Closed$/)).toBeInTheDocument();
    // The visible count is announced to screen readers as a live region.
    const hasStatus = (re: RegExp) =>
      screen.getAllByRole("status").some((el) => re.test(el.textContent ?? ""));
    expect(hasStatus(/2 issues/)).toBe(true);

    // Selecting the Label filter narrows the list client-side.
    fireEvent.change(screen.getByLabelText("Label"), { target: { value: "bug" } });
    await waitFor(() => {
      expect(screen.queryByText("plain issue")).not.toBeInTheDocument();
    });
    expect(screen.getByText("bug issue")).toBeInTheDocument();
    expect(hasStatus(/1 issue/)).toBe(true);
  });

  it("switches to closed via the count header, refetching with state=closed", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("state=closed")) {
        return Promise.resolve(jsonResponse([issueWith(3, "done issue", { state: "closed" })]));
      }
      if (u.includes("/issues?")) return Promise.resolve(jsonResponse([issue(1, "open issue")]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText("open issue")).toBeInTheDocument());

    fireEvent.click(screen.getByText(/Closed$/));
    await waitFor(() => expect(screen.getByText("done issue")).toBeInTheDocument());
    const calls = mockFetch.mock.calls.map((c) => c[0].toString());
    expect(calls.some((u) => u.includes("/issues?state=closed"))).toBe(true);
  });

  it("carries the label + author filters to the server query", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("state=closed")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/milestones")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues?")) return Promise.resolve(jsonResponse([issueWith(1, "bug issue", { labels: [{ name: "bug", color: "d73a4a" }], user: { login: "octo" } })]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText("bug issue")).toBeInTheDocument());

    fireEvent.change(screen.getByLabelText("Label"), { target: { value: "bug" } });
    fireEvent.change(screen.getByLabelText("Author"), { target: { value: "octo" } });
    await waitFor(() => {
      const calls = mockFetch.mock.calls.map((c) => c[0].toString());
      expect(calls.some((u) => u.includes("/issues?") && u.includes("labels=bug") && u.includes("creator=octo"))).toBe(true);
    });
  });

  it("switches to All via the count header, refetching with state=all", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("state=all")) {
        return Promise.resolve(jsonResponse([issue(1, "open issue"), issueWith(3, "done issue", { state: "closed" })]));
      }
      if (u.includes("state=closed")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues?")) return Promise.resolve(jsonResponse([issue(1, "open issue")]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText("open issue")).toBeInTheDocument());

    fireEvent.click(screen.getByText(/All$/));
    await waitFor(() => expect(screen.getByText("done issue")).toBeInTheDocument());
    const calls = mockFetch.mock.calls.map((c) => c[0].toString());
    expect(calls.some((u) => u.includes("/issues?state=all"))).toBe(true);
  });
});

describe("IssuesPage detail sidebar", () => {
  it("renders the two-column layout with the metadata sidebar", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) {
        return Promise.resolve(
          jsonResponse({ ...issue(7, "Sidebar issue"), assignees: [{ login: "carol" }] }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    await waitFor(() => expect(screen.getByText("Sidebar issue")).toBeInTheDocument());
    // Assignees + Development are sidebar-only; Projects/Labels also name tabs, so assert distinctive ones.
    expect(screen.getByText("Assignees")).toBeInTheDocument();
    expect(screen.getByText("Development")).toBeInTheDocument();
    expect(screen.getByText("carol")).toBeInTheDocument();
  });
});

const bugLabel = { id: 1, name: "bug", color: "d73a4a", description: "Broken", default: false };

function milestone(number: number, title: string, state = "open") {
  return {
    id: number,
    number,
    title,
    description: "",
    state,
    creator: { login: "admin", avatar_url: "" },
    open_issues: 1,
    closed_issues: 3,
    due_on: null,
    closed_at: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

describe("IssuesPage labels view", () => {
  it("lists repo labels with descriptions", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/labels")) return Promise.resolve(jsonResponse([bugLabel]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/labels");
    await waitFor(() => {
      expect(screen.getByText("bug")).toBeInTheDocument();
    });
    expect(screen.getByText("Broken")).toBeInTheDocument();
  });

  it("shows an empty state when the repo has no labels", async () => {
    mockFetch.mockImplementation(() => Promise.resolve(jsonResponse([])));
    renderAt("/ui/admin/test/labels");
    await waitFor(() => {
      expect(screen.getByText(/no labels yet/i)).toBeInTheDocument();
    });
  });

  it("creates a label through the dialog", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      if (u.includes("/labels") && init?.method === "POST") {
        return Promise.resolve(jsonResponse(bugLabel, 201));
      }
      if (u.includes("/labels")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/labels");
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /new label/i })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /new label/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "bug" } });
    fireEvent.click(screen.getByRole("button", { name: /create label/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/api/v3/repos/admin/test/labels") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toMatchObject({ name: "bug" });
    });
  });
});

describe("IssuesPage milestones view", () => {
  it("lists milestones with progress and supports closing", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      if (u.includes("/milestones/1") && init?.method === "PATCH") {
        return Promise.resolve(jsonResponse(milestone(1, "v1.0", "closed")));
      }
      if (u.includes("/milestones?")) return Promise.resolve(jsonResponse([milestone(1, "v1.0")]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/milestones");
    await waitFor(() => {
      expect(screen.getByText("v1.0")).toBeInTheDocument();
    });
    expect(screen.getByText(/75% complete · 1 open · 3 closed/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "close" }));
    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/milestones/1") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeTruthy();
      expect(JSON.parse(String(patch![1]!.body))).toMatchObject({ state: "closed" });
    });
  });
});

describe("IssuesPage detail triage", () => {
  const epicType = {
    id: 5,
    node_id: "IT_kwDO00000005",
    name: "Epic",
    description: "Coordinated work",
    color: "purple",
    is_enabled: true,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };

  function mockDetailEndpoints() {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/repos/admin/test")) {
        return Promise.resolve(
          jsonResponse({ owner: { login: "admin", type: "Organization" }, permissions: adminPerms }),
        );
      }
      if (u.includes("/issues/7") && init?.method === "PATCH") {
        return Promise.resolve(
          jsonResponse({ ...issue(7, "Triaged"), milestone: milestone(2, "v2.0"), issue_type: epicType }),
        );
      }
      if (u.includes("/issues/7/labels") && init?.method === "POST") {
        return Promise.resolve(jsonResponse([bugLabel]));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "Triaged")));
      if (u.includes("/milestones?")) {
        return Promise.resolve(jsonResponse([milestone(2, "v2.0")]));
      }
      if (u.includes("/api/v3/repos/admin/test/labels")) {
        return Promise.resolve(jsonResponse([bugLabel]));
      }
      if (u.includes("/api/v3/orgs/admin/issue-types")) {
        return Promise.resolve(jsonResponse([epicType]));
      }
      return Promise.resolve(jsonResponse([]));
    });
  }

  it("adds a label from the repo label list", async () => {
    mockDetailEndpoints();
    renderAt("/ui/admin/test/issues/7");
    await waitFor(() => {
      expect(screen.getByLabelText("Add label")).toBeInTheDocument();
    });
    fireEvent.change(screen.getByLabelText("Add label"), { target: { value: "bug" } });
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/issues/7/labels") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toEqual({ labels: ["bug"] });
    });
  });

  it("sets the milestone via PATCH", async () => {
    mockDetailEndpoints();
    renderAt("/ui/admin/test/issues/7");
    await waitFor(() => {
      expect(screen.getByRole("option", { name: "v2.0" })).toBeInTheDocument();
    });
    fireEvent.change(screen.getByLabelText("Set milestone"), { target: { value: "2" } });
    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeTruthy();
      expect(JSON.parse(String(patch![1]!.body))).toEqual({ milestone: 2 });
    });
  });

  it("sets the organization issue type via PATCH", async () => {
    mockDetailEndpoints();
    renderAt("/ui/admin/test/issues/7");
    await waitFor(() => {
      expect(screen.getByRole("option", { name: "Epic" })).toBeInTheDocument();
    });
    fireEvent.change(screen.getByLabelText("Set issue type"), { target: { value: "5" } });
    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeTruthy();
      expect(JSON.parse(String(patch![1]!.body))).toEqual({ issue_type_id: 5 });
    });
  });

  it("hides issue type controls and skips the org issue-type endpoint for user-owned repositories", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.endsWith("/api/v3/repos/admin/test")) {
        return Promise.resolve(jsonResponse({ owner: { login: "admin", type: "User" } }));
      }
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "Triaged")));
      if (u.includes("/milestones?")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/api/v3/repos/admin/test/labels")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    await waitFor(() => expect(screen.getByText("Triaged")).toBeInTheDocument());
    expect(screen.queryByLabelText("Set issue type")).not.toBeInTheDocument();
    expect(
      mockFetch.mock.calls.some((c) => c[0].toString().includes("/api/v3/orgs/admin/issue-types")),
    ).toBe(false);
  });
});

// ─── PR rows must never leak into the Issues list (BLOCKER regression) ────

describe("IssuesPage PR filtering", () => {
  it("hides pull-request rows from a mixed /issues payload and excludes them from counts", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) return Promise.resolve(jsonResponse({}));
      if (u.includes("state=closed")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues?")) {
        return Promise.resolve(
          jsonResponse([
            issue(1, "a real issue"),
            { ...issue(2, "actually a pull request"), pull_request: { url: "http://x/pulls/2" } },
          ]),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText("a real issue")).toBeInTheDocument());
    expect(screen.queryByText("actually a pull request")).not.toBeInTheDocument();
    expect(screen.getByText(/1 Open/)).toBeInTheDocument();
  });
});

// ─── Exact counts via the search API ──────────────────────────────────────

describe("IssuesPage exact counts", () => {
  it("prefers search total_count over the truncated page count", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) {
        const q = decodeURIComponent(u);
        return Promise.resolve(jsonResponse({ total_count: q.includes("is:open") ? 120 : 45 }));
      }
      if (u.includes("state=closed")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues?")) {
        return Promise.resolve(
          jsonResponse([issue(1, "first issue")], 200, {
            Link: `</api/v3/repos/admin/test/issues?state=open&per_page=50&page=2>; rel="next"`,
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText(/120 Open/)).toBeInTheDocument());
    expect(screen.getByText(/45 Closed/)).toBeInTheDocument();
    // All = exact open + exact closed.
    expect(screen.getByText(/165 All/)).toBeInTheDocument();
  });
});

// ─── List row anatomy ─────────────────────────────────────────────────────

describe("IssuesPage list rows", () => {
  it("shows comment count, milestone chip and the not-planned skip icon", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) return Promise.resolve(jsonResponse({}));
      if (u.includes("/issues?")) {
        return Promise.resolve(
          jsonResponse([
            {
              ...issue(1, "discussed issue"),
              comments: 4,
              milestone: milestone(1, "v1.0"),
            },
            {
              ...issue(2, "skipped issue"),
              state: "closed",
              state_reason: "not_planned",
            },
            {
              ...issue(3, "done issue"),
              state: "closed",
              state_reason: "completed",
            },
          ]),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText("discussed issue")).toBeInTheDocument());
    // Comment count badge with an accessible name.
    expect(screen.getByLabelText("4 comments")).toBeInTheDocument();
    // Scope to the row: the facet dropdown also lists this milestone.
    const row = screen.getByRole("link", { name: /discussed issue/ });
    expect(within(row).getByText("v1.0")).toBeInTheDocument();
    // Closed-as-not-planned gray skip icon vs completed purple check.
    expect(screen.getByLabelText("Closed as not planned")).toBeInTheDocument();
    expect(screen.getByLabelText("Closed as completed")).toBeInTheDocument();
    expect(document.querySelector("time")).not.toBeNull();
  });
});

// ─── Free-text search (server pass-through + no silent drops) ─────────────

describe("IssuesPage free-text search", () => {
  it("passes free text to the server search and renders its results", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) {
        const q = decodeURIComponent(u);
        if (q.includes("per_page=1")) return Promise.resolve(jsonResponse({}));
        if (q.includes("crash")) {
          return Promise.resolve(jsonResponse({ total_count: 1, items: [issue(9, "crash on save")] }));
        }
        return Promise.resolve(jsonResponse({ total_count: 0, items: [] }));
      }
      if (u.includes("/issues?")) {
        return Promise.resolve(jsonResponse([issue(1, "unrelated issue")]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText("unrelated issue")).toBeInTheDocument());

    const box = screen.getByLabelText("Search issues and pull requests");
    fireEvent.change(box, { target: { value: "is:issue is:open crash" } });
    fireEvent.submit(box.closest("form") as HTMLFormElement);

    await waitFor(() => expect(screen.getByText("crash on save")).toBeInTheDocument());
    expect(screen.queryByText("unrelated issue")).not.toBeInTheDocument();
    const calls = mockFetch.mock.calls.map((c) => decodeURIComponent(c[0].toString()));
    expect(calls.some((u) => u.includes("/search/issues") && u.includes("crash"))).toBe(true);
    // The free text stays visible in the box — never silently dropped.
    expect((screen.getByLabelText("Search issues and pull requests") as HTMLInputElement).value).toContain("crash");
  });

  it("supports the no:label qualifier client-side", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) return Promise.resolve(jsonResponse({}));
      if (u.includes("/issues?")) {
        return Promise.resolve(
          jsonResponse([
            { ...issue(1, "labeled issue"), labels: [{ name: "bug", color: "d73a4a" }] },
            issue(2, "bare issue"),
          ]),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText("labeled issue")).toBeInTheDocument());

    const box = screen.getByLabelText("Search issues and pull requests");
    fireEvent.change(box, { target: { value: "is:issue is:open no:label" } });
    fireEvent.submit(box.closest("form") as HTMLFormElement);

    await waitFor(() => expect(screen.queryByText("labeled issue")).not.toBeInTheDocument());
    expect(screen.getByText("bare issue")).toBeInTheDocument();
  });
});

// ─── New-issue template chooser ───────────────────────────────────────────

describe("IssuesPage new-issue templates", () => {
  const templateMd = [
    "---",
    "name: Bug report",
    "about: Report something broken",
    "title: '[Bug]: '",
    "labels: bug, needs-triage",
    "---",
    "**Steps to reproduce**",
    "",
  ].join("\n");

  it("offers the template chooser and pre-fills title/body/labels from front-matter", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) return Promise.resolve(jsonResponse({}));
      if (u.includes("/contents/.github/ISSUE_TEMPLATE/bug_report.md")) {
        return Promise.resolve(
          jsonResponse({
            type: "file",
            name: "bug_report.md",
            path: ".github/ISSUE_TEMPLATE/bug_report.md",
            content: btoa(templateMd),
            encoding: "base64",
          }),
        );
      }
      if (u.includes("/contents/.github/ISSUE_TEMPLATE")) {
        return Promise.resolve(
          jsonResponse([
            { type: "file", name: "bug_report.md", path: ".github/ISSUE_TEMPLATE/bug_report.md", sha: "x" },
            { type: "file", name: "config.yml", path: ".github/ISSUE_TEMPLATE/config.yml", sha: "y" },
          ]),
        );
      }
      if (u.includes("/ui-data/bootstrap/repos/")) {
        return Promise.resolve(
          jsonResponse({
            repo: {},
            branches: { first_page: [], total_count: 0 },
            tags: { first_page: [], total_count: 0 },
            contributors: [],
            root_entries: [{ type: "dir", name: ".github", path: ".github" }],
          }),
        );
      }
      if (u.includes("/contents/")) {
        // Root and .github listings for the top-down template walk.
        return Promise.resolve(
          jsonResponse([
            { type: "dir", name: ".github", path: ".github" },
            { type: "dir", name: "ISSUE_TEMPLATE", path: ".github/ISSUE_TEMPLATE" },
          ]),
        );
      }
      if (u.endsWith("/issues") && init?.method === "POST") {
        return Promise.resolve(jsonResponse(issue(11, "[Bug]: boom"), 201));
      }
      // Post-create navigation lands on the issue detail.
      if (u.includes("/issues/11")) return Promise.resolve(jsonResponse(issue(11, "[Bug]: boom")));
      if (u.includes("/issues?")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByRole("button", { name: "New issue" })).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "New issue" }));

    // Chooser lists the markdown template (config.yml is not a template).
    expect(await screen.findByText("bug_report")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Get started" }));

    // Title comes pre-filled from the template front-matter.
    const title = await screen.findByPlaceholderText("Issue title");
    expect((title as HTMLInputElement).value).toBe("[Bug]: ");
    fireEvent.change(title, { target: { value: "[Bug]: boom" } });
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toMatchObject({
        title: "[Bug]: boom",
        labels: ["bug", "needs-triage"],
      });
      expect(JSON.parse(String(post![1]!.body)).body).toContain("**Steps to reproduce**");
    });
  });

  it("offers 'Open a blank issue' from the chooser", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) return Promise.resolve(jsonResponse({}));
      if (u.includes("/contents/.github/ISSUE_TEMPLATE")) {
        return Promise.resolve(
          jsonResponse([{ type: "file", name: "bug_report.md", path: ".github/ISSUE_TEMPLATE/bug_report.md", sha: "x" }]),
        );
      }
      if (u.includes("/ui-data/bootstrap/repos/")) {
        return Promise.resolve(
          jsonResponse({
            repo: {},
            branches: { first_page: [], total_count: 0 },
            tags: { first_page: [], total_count: 0 },
            contributors: [],
            root_entries: [{ type: "dir", name: ".github", path: ".github" }],
          }),
        );
      }
      if (u.includes("/contents/")) {
        return Promise.resolve(
          jsonResponse([
            { type: "dir", name: ".github", path: ".github" },
            { type: "dir", name: "ISSUE_TEMPLATE", path: ".github/ISSUE_TEMPLATE" },
          ]),
        );
      }
      if (u.includes("/issues?")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByRole("button", { name: "New issue" })).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "New issue" }));
    fireEvent.click(await screen.findByRole("button", { name: "Open a blank issue" }));
    const title = await screen.findByPlaceholderText("Issue title");
    expect((title as HTMLInputElement).value).toBe("");
  });
});

// ─── Pinned issues ────────────────────────────────────────────────────────

describe("IssuesPage pinned issues", () => {
  it("renders the pinned issues section from Repository.pinnedIssues", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (String(body.query).includes("pinnedIssues")) {
          return Promise.resolve(
            jsonResponse({
              data: {
                repository: {
                  pinnedIssues: {
                    nodes: [{ issue: { number: 5, title: "read me first", state: "OPEN", stateReason: null } }],
                  },
                },
              },
            }),
          );
        }
        return Promise.resolve(jsonResponse({ data: {} }));
      }
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) return Promise.resolve(jsonResponse({}));
      if (u.includes("/issues?")) return Promise.resolve(jsonResponse([issue(1, "ordinary issue")]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues");
    await waitFor(() => expect(screen.getByText("read me first")).toBeInTheDocument());
    const section = screen.getByRole("region", { name: "Pinned issues" });
    expect(section).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /read me first/ })).toHaveAttribute(
      "href",
      "/ui/admin/test/issues/5",
    );
  });
});

// ─── Overflow menu: pin / transfer / delete ───────────────────────────────

describe("IssuesPage overflow menu", () => {
  function mockMenuEndpoints(overrides?: (u: string, init?: RequestInit) => Response | undefined) {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      const custom = overrides?.(u, init);
      if (custom) return Promise.resolve(custom);
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        const q = String(body.query);
        if (q.includes("isPinned") && q.includes("pinnedIssues")) {
          return Promise.resolve(
            jsonResponse({
              data: { repository: { issue: { isPinned: false }, pinnedIssues: { totalCount: 1 } } },
            }),
          );
        }
        return Promise.resolve(jsonResponse({ data: {} }));
      }
      if (u.endsWith("/api/v3/repos/admin/test")) {
        return Promise.resolve(
          jsonResponse({ owner: { login: "admin", type: "User" }, permissions: adminPerms }),
        );
      }
      if (u.endsWith("/api/v3/users/admin/repos?per_page=100")) {
        return Promise.resolve(
          jsonResponse([
            { id: 1, node_id: "R_1", name: "test", full_name: "admin/test" },
            { id: 2, node_id: "R_2", name: "other", full_name: "admin/other" },
          ]),
        );
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      // The transfer flow navigates to the moved issue in admin/other.
      if (u.includes("/issues/3")) return Promise.resolve(jsonResponse(issue(3, "Transferred issue")));
      return Promise.resolve(jsonResponse([]));
    });
  }

  const graphqlCall = (needle: string) =>
    mockFetch.mock.calls.find(
      ([u, i]) => u.toString().includes("/api/graphql") && String((i as RequestInit)?.body).includes(needle),
    );

  it("pins the issue via the pinIssue mutation", async () => {
    mockMenuEndpoints((u, init) => {
      if (u.includes("/api/graphql")) {
        const q = String(JSON.parse((init?.body as string) ?? "{}").query);
        if (q.includes("pinIssue(")) {
          return jsonResponse({ data: { pinIssue: { issue: { isPinned: true } } } });
        }
      }
      return undefined;
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: "Issue actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Pin issue/ }));
    await waitFor(() => {
      const mut = graphqlCall("pinIssue(");
      expect(mut).toBeTruthy();
      const vars = JSON.parse(String((mut![1] as RequestInit).body)).variables;
      expect(vars.input.issueId).toBe(issue(7, "x").node_id);
    });
  });

  it("disables Pin when the repo already has 3 pinned issues", async () => {
    mockMenuEndpoints((u, init) => {
      if (u.includes("/api/graphql")) {
        const q = String(JSON.parse((init?.body as string) ?? "{}").query);
        if (q.includes("isPinned") && q.includes("pinnedIssues")) {
          return jsonResponse({
            data: { repository: { issue: { isPinned: false }, pinnedIssues: { totalCount: 3 } } },
          });
        }
      }
      return undefined;
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: "Issue actions" }));
    const pinItem = await screen.findByRole("menuitem", { name: /Pin issue/ });
    expect(pinItem).toBeDisabled();
    expect(pinItem.getAttribute("title")).toMatch(/3 pinned issues/);
  });

  it("transfers the issue to a same-owner repo and navigates to the new URL", async () => {
    mockMenuEndpoints((u, init) => {
      if (u.includes("/api/graphql")) {
        const q = String(JSON.parse((init?.body as string) ?? "{}").query);
        if (q.includes("transferIssue(")) {
          return jsonResponse({
            data: {
              transferIssue: {
                issue: { number: 3, repository: { name: "other", owner: { login: "admin" } } },
              },
            },
          });
        }
      }
      return undefined;
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: "Issue actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Transfer issue" }));

    const select = await screen.findByLabelText("Choose a repository");
    // Wait for the candidate repos to load; the current repo is not a destination.
    await screen.findByRole("option", { name: "admin/other" });
    expect(screen.queryByRole("option", { name: "admin/test" })).not.toBeInTheDocument();
    fireEvent.change(select, { target: { value: "R_2" } });
    fireEvent.click(screen.getByRole("button", { name: "Transfer issue" }));

    await waitFor(() => {
      const mut = graphqlCall("transferIssue(");
      expect(mut).toBeTruthy();
      const vars = JSON.parse(String((mut![1] as RequestInit).body)).variables;
      expect(vars.input).toMatchObject({ issueId: issue(7, "x").node_id, repositoryId: "R_2" });
    });
    await waitFor(() => {
      expect(screen.queryByText("A real issue")).not.toBeInTheDocument();
    });
  });

  it("deletes the issue only after the typed confirmation matches", async () => {
    mockMenuEndpoints((u, init) => {
      if (u.includes("/api/graphql")) {
        const q = String(JSON.parse((init?.body as string) ?? "{}").query);
        if (q.includes("deleteIssue(")) {
          return jsonResponse({ data: { deleteIssue: { repository: { name: "test" } } } });
        }
      }
      return undefined;
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: "Issue actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete issue" }));

    const confirmBtn = await screen.findByRole("button", { name: "Delete this issue" });
    expect(confirmBtn).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/to confirm/), { target: { value: "admin/test#7" } });
    expect(confirmBtn).not.toBeDisabled();
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      const mut = graphqlCall("deleteIssue(");
      expect(mut).toBeTruthy();
      const vars = JSON.parse(String((mut![1] as RequestInit).body)).variables;
      expect(vars.input.issueId).toBe(issue(7, "x").node_id);
    });
  });
});

// ─── Close with comment ───────────────────────────────────────────────────

describe("IssuesPage close with comment", () => {
  it("posts the pending comment first, then the state change, on one click", async () => {
    const order: string[] = [];
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7/comments") && init?.method === "POST") {
        order.push("comment");
        return Promise.resolve(jsonResponse({ id: 1, body: "wrapping up" }, 201));
      }
      if (u.endsWith("/issues/7") && init?.method === "PATCH") {
        order.push("patch");
        return Promise.resolve(jsonResponse({ ...issue(7, "A real issue"), state: "closed" }));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    const box = await screen.findByPlaceholderText(/leave a comment/i);
    fireEvent.change(box, { target: { value: "wrapping up" } });
    // With a draft present the close button reads "Close with comment".
    const closeBtn = await screen.findByRole("button", { name: "Close with comment" });
    fireEvent.click(closeBtn);
    await waitFor(() => {
      expect(order).toEqual(["comment", "patch"]);
    });
    const posted = mockFetch.mock.calls.find(
      (c) => c[0].toString().endsWith("/issues/7/comments") && c[1]?.method === "POST",
    );
    expect(JSON.parse(String(posted![1]!.body))).toEqual({ body: "wrapping up" });
  });
});

// ─── Lock with reason ─────────────────────────────────────────────────────

describe("IssuesPage lock reason", () => {
  it("passes the selected lock_reason on PUT /lock", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/issues/7/lock") && init?.method === "PUT") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.change(await screen.findByLabelText("Lock reason"), { target: { value: "spam" } });
    fireEvent.click(screen.getByRole("button", { name: /lock conversation/i }));
    await waitFor(() => {
      const locked = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/issues/7/lock") && c[1]?.method === "PUT",
      );
      expect(locked).toBeTruthy();
      expect(JSON.parse(String(locked![1]!.body))).toEqual({ lock_reason: "spam" });
    });
  });
});

// ─── Notifications (thread subscription) ──────────────────────────────────

describe("IssuesPage notifications section", () => {
  it("subscribes via the notification-thread subscription endpoint", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/repos/admin/test/notifications")) {
        return Promise.resolve(
          jsonResponse([
            {
              id: "42",
              subject: { title: "A real issue", url: "http://x/api/v3/repos/admin/test/issues/7", type: "Issue" },
            },
          ]),
        );
      }
      if (u.endsWith("/notifications/threads/42/subscription")) {
        if (init?.method === "DELETE") return Promise.resolve(jsonResponse({}, 200));
        return Promise.resolve(jsonResponse({ subscribed: true, ignored: false }));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    // A thread means the viewer is subscribed, so start on Unsubscribe: probing state via GET would
    // 404 without a subscription record and trip the console-error e2e gate.
    const unsub = await screen.findByRole("button", { name: "Unsubscribe" });
    fireEvent.click(unsub);
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/notifications/threads/42/subscription") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
    const btn = await screen.findByRole("button", { name: "Subscribe" });
    fireEvent.click(btn);
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/notifications/threads/42/subscription") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse(String(put![1]!.body))).toEqual({ ignored: false });
    });
  });

  it("says so when there is no notification thread to subscribe to", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/repos/admin/test/notifications")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    await waitFor(() =>
      expect(screen.getByText(/No notification thread for this conversation yet/)).toBeInTheDocument(),
    );
    expect(screen.queryByRole("button", { name: "Subscribe" })).not.toBeInTheDocument();
  });
});

// ─── Sidebar gear buttons ─────────────────────────────────────────────────

describe("IssueSidebar gear buttons", () => {
  it("renders a real gear button that focuses the section's picker", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(adminRepo));
      if (u.endsWith("/repos/admin/test/assignees")) {
        return Promise.resolve(jsonResponse([{ login: "bob" }]));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    const gear = await screen.findByRole("button", { name: "Edit assignees" });
    fireEvent.click(gear);
    await waitFor(() => {
      expect(screen.getByLabelText("Add assignee")).toHaveFocus();
    });
  });
});

// ─── Milestones view: links + progress bars ───────────────────────────────

describe("IssuesPage milestones links", () => {
  it("links each milestone to the pre-filtered issues list and shows a progress bar", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/milestones?")) return Promise.resolve(jsonResponse([milestone(1, "v1.0")]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/milestones");
    const link = await screen.findByRole("link", { name: "v1.0" });
    expect(link).toHaveAttribute("href", "/ui/admin/test/issues?milestone=v1.0");
    const bar = screen.getByRole("progressbar", { name: "v1.0 progress" });
    expect(bar).toHaveAttribute("aria-valuenow", "75");
  });

  it("pre-filters the issues list from the milestone query param", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/search/issues")) return Promise.resolve(jsonResponse({}));
      if (u.includes("/milestones")) return Promise.resolve(jsonResponse([milestone(1, "v1.0")]));
      if (u.includes("/issues?")) {
        return Promise.resolve(
          jsonResponse([
            { ...issue(1, "in milestone"), milestone: milestone(1, "v1.0") },
            issue(2, "not in milestone"),
          ]),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues?milestone=v1.0");
    await waitFor(() => expect(screen.getByText("in milestone")).toBeInTheDocument());
    expect(screen.queryByText("not in milestone")).not.toBeInTheDocument();
  });
});

describe("IssuesPage detail bootstrap", () => {
  it("hydrates the issue detail from the bootstrap with no standalone refetches", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/ui-data/bootstrap/repos/admin/test/issues/7")) {
        return Promise.resolve(
          jsonResponse({
            issue: issue(7, "Bootstrapped issue"),
            comments: [],
            timeline: [
              {
                event: "commented",
                id: 900,
                node_id: "IC_900",
                body: "seeded comment",
                created_at: "2026-01-02T00:00:00Z",
                user: { login: "admin", avatar_url: "" },
              },
            ],
            labels: [{ id: 1, name: "bug", color: "ff0000" }],
            milestones: [],
            assignees_available: [{ login: "admin" }],
          }),
        );
      }
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({
            data: { repository: { issue: { isPinned: false }, pinnedIssues: { totalCount: 0 } } },
          }),
        );
      }
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    expect(await screen.findByText("Bootstrapped issue")).toBeInTheDocument();
    expect(await screen.findByText("seeded comment")).toBeInTheDocument();

    // Every sub-payload the bootstrap carried must be a cache hit — none of
    // the standalone endpoints those hooks call may have been fetched.
    const gets = mockFetch.mock.calls
      .filter((c) => (c[1] as RequestInit | undefined)?.method === undefined)
      .map((c) => c[0]!.toString());
    expect(gets.some((u) => u.includes("/api/v3/") && u.endsWith("/issues/7"))).toBe(false);
    expect(gets.some((u) => u.includes("/issues/7/timeline"))).toBe(false);
    expect(gets.some((u) => u.includes("/api/v3/repos/admin/test/labels"))).toBe(false);
    expect(gets.some((u) => u.includes("/assignees"))).toBe(false);
  });

  it("seeds the all-states milestone key and a client-filtered open key from the bootstrap", async () => {
    const openMs = milestone(1, "v1.0", "open");
    const closedMs = milestone(2, "v0.9", "closed");
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/ui-data/bootstrap/repos/admin/test/issues/7")) {
        return Promise.resolve(
          jsonResponse({
            issue: issue(7, "Bootstrapped issue"),
            comments: [],
            timeline: [],
            labels: [],
            // The aggregate list is state=ALL: it carries open AND closed.
            milestones: [openMs, closedMs],
            assignees_available: [],
          }),
        );
      }
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      return Promise.resolve(jsonResponse([]));
    });
    const { queryClient } = renderAt("/ui/admin/test/issues/7");
    expect(await screen.findByText("Bootstrapped issue")).toBeInTheDocument();

    // The key IssueSidebar reads gets the full state=ALL list…
    expect(queryClient.getQueryData(["milestones", "admin", "test", "all"])).toEqual([openMs, closedMs]);
    // …and the key NewIssue reads gets only the open milestones.
    expect(queryClient.getQueryData(["milestones", "admin", "test", "open"])).toEqual([openMs]);
  });

  it("falls back to the standalone endpoints when the bootstrap answers 500", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/ui-data/bootstrap/")) {
        return Promise.resolve(jsonResponse({ message: "boom" }, 500));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "Fallback issue")));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/issues/7");
    expect(await screen.findByText("Fallback issue")).toBeInTheDocument();
    const gets = mockFetch.mock.calls
      .filter((c) => (c[1] as RequestInit | undefined)?.method === undefined)
      .map((c) => c[0]!.toString());
    expect(gets.some((u) => u.includes("/api/v3/") && u.endsWith("/issues/7"))).toBe(true);
  });
});

// ─── Convert issue to discussion ──────────────────────────────────────────

describe("IssuesPage convert to discussion", () => {
  // The GraphQL category id is the node id; its trailing digits are the
  // numeric store id the /ui-data convert endpoint takes.
  const generalCategory = {
    id: "DGC_kgDO00000005",
    name: "General",
    emoji: "💬",
    description: "",
    isAnswerable: false,
  };

  function mockConvertEndpoints(opts: {
    hasDiscussions: boolean;
    convert?: (init?: RequestInit) => Response;
  }) {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/convert-to-discussion") && init?.method === "POST") {
        return Promise.resolve(
          opts.convert?.(init) ??
            jsonResponse({ id: 1, number: 12, title: "A real issue", category: generalCategory }, 201),
        );
      }
      if (u.includes("/api/graphql")) {
        const q = String(JSON.parse((init?.body as string) ?? "{}").query);
        if (q.includes("discussionCategories")) {
          return Promise.resolve(
            jsonResponse({
              data: { repository: { discussionCategories: { nodes: [generalCategory] } } },
            }),
          );
        }
        return Promise.resolve(
          jsonResponse({
            data: { repository: { issue: { isPinned: false }, pinnedIssues: { totalCount: 0 } } },
          }),
        );
      }
      if (u.endsWith("/api/v3/repos/admin/test")) {
        return Promise.resolve(
          jsonResponse({
            owner: { login: "admin", type: "User" },
            has_discussions: opts.hasDiscussions,
            permissions: adminPerms,
          }),
        );
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "admin" }));
      if (u.includes("/issues/7")) return Promise.resolve(jsonResponse(issue(7, "A real issue")));
      return Promise.resolve(jsonResponse([]));
    });
  }

  it("converts via the actions menu, POSTing the chosen category and navigating to the discussion", async () => {
    mockConvertEndpoints({ hasDiscussions: true });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: "Issue actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Convert to discussion" }));

    const select = await screen.findByLabelText("Category");
    await screen.findByRole("option", { name: /General/ });
    fireEvent.change(select, { target: { value: generalCategory.id } });
    fireEvent.click(screen.getByRole("button", { name: "I understand, convert this issue" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) =>
          c[0].toString().endsWith("/ui-data/repos/admin/test/issues/7/convert-to-discussion") &&
          c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toEqual({ category_id: 5 });
    });
    expect(await screen.findByText("discussion detail route")).toBeInTheDocument();
  });

  it("does not offer Convert to discussion when the repo has discussions disabled", async () => {
    mockConvertEndpoints({ hasDiscussions: false });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: "Issue actions" }));
    // The menu is open (Transfer is always offered) but Convert is absent.
    expect(await screen.findByRole("menuitem", { name: "Transfer issue" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Convert to discussion" })).not.toBeInTheDocument();
  });

  it("surfaces a 422 inline in the dialog", async () => {
    mockConvertEndpoints({
      hasDiscussions: true,
      convert: () =>
        jsonResponse(
          { message: "Validation Failed", errors: [{ resource: "Discussion", field: "category_id", code: "invalid" }] },
          422,
        ),
    });
    renderAt("/ui/admin/test/issues/7");
    fireEvent.click(await screen.findByRole("button", { name: "Issue actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Convert to discussion" }));

    const select = await screen.findByLabelText("Category");
    await screen.findByRole("option", { name: /General/ });
    fireEvent.change(select, { target: { value: generalCategory.id } });
    fireEvent.click(screen.getByRole("button", { name: "I understand, convert this issue" }));

    // The 422 renders inline; the dialog stays open (Category still visible).
    expect(await screen.findByText(/can't be converted/)).toBeInTheDocument();
    expect(screen.getByLabelText("Category")).toBeInTheDocument();
  });
});

// ─── Viewer-role gating (pull-only outsider vs author) ────────────────────

describe("IssuesPage viewer-role gating", () => {
  const readerRepo = { ...adminRepo, permissions: { admin: false, push: false, pull: true } };

  function mockReadOnlyDetail(viewerLogin: string) {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(readerRepo));
      if (u.endsWith("/repos/admin/test/assignees")) return Promise.resolve(jsonResponse([{ login: "bob" }]));
      if (u.includes("/issues/7/timeline")) {
        return Promise.resolve(
          jsonResponse([
            {
              event: "commented",
              id: 100,
              node_id: "IC_100",
              body: "someone else's comment",
              user: { login: "admin" },
              created_at: "2026-01-02T00:00:00Z",
            },
          ]),
        );
      }
      if (u.includes("/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: viewerLogin }));
      if (u.includes("/issues/7")) {
        return Promise.resolve(jsonResponse({ ...issue(7, "A real issue"), assignees: [{ login: "carol" }] }));
      }
      return Promise.resolve(jsonResponse([]));
    });
  }

  it("hides every 403-able control from a pull-only outsider", async () => {
    mockReadOnlyDetail("reader");
    renderAt("/ui/admin/test/issues/7");

    // The conversation, composer and reactions stay for everyone.
    expect(await screen.findByText("someone else's comment")).toBeInTheDocument();
    expect(await screen.findByPlaceholderText(/leave a comment/i)).toBeInTheDocument();
    expect((await screen.findAllByRole("button", { name: "add reaction" })).length).toBeGreaterThan(0);

    // Close/reopen, title edit, and the overflow menu are write-or-author.
    expect(screen.queryByRole("button", { name: /close issue/i })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Reason for closing")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^edit$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Issue actions" })).not.toBeInTheDocument();

    // Sidebar is read-only: values as plain text, no pickers, gears, or Lock conversation section.
    expect(screen.getByText("carol")).toBeInTheDocument();
    expect(screen.queryByLabelText("Add assignee")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Unassign carol")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit assignees" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Add label")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Set milestone")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /lock conversation/i })).not.toBeInTheDocument();

    // Comment moderation is write-or-author; this comment is someone else's.
    expect(screen.queryByRole("button", { name: "Edit comment" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete comment" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Hide comment" })).not.toBeInTheDocument();
  });

  it("lets the issue author close, edit and manage their own comment without push", async () => {
    mockReadOnlyDetail("admin"); // issue #7 and comment 100 are authored by admin
    renderAt("/ui/admin/test/issues/7");

    expect(await screen.findByRole("button", { name: /close issue/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^edit$/i })).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Edit comment" })).toBeInTheDocument();

    // Authorship does not grant triage: the kebab and sidebar stay read-only.
    expect(screen.queryByRole("button", { name: "Issue actions" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Add assignee")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /lock conversation/i })).not.toBeInTheDocument();
  });

  it("renders the labels view read-only without push access", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(readerRepo));
      if (u.includes("/labels")) return Promise.resolve(jsonResponse([bugLabel]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/labels");
    expect(await screen.findByText("bug")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /new label/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "delete" })).not.toBeInTheDocument();
  });

  it("renders the milestones view read-only without push access", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/repos/admin/test")) return Promise.resolve(jsonResponse(readerRepo));
      if (u.includes("/milestones?")) return Promise.resolve(jsonResponse([milestone(1, "v1.0")]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/admin/test/milestones");
    expect(await screen.findByText("v1.0")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /new milestone/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "close" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "delete" })).not.toBeInTheDocument();
  });
});
