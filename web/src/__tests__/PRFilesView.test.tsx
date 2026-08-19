import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { PRFilesView } from "../components/PRFilesView.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown) {
  return new Response(JSON.stringify(data), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
  // Viewed-file and review-summary state persist per PR in sessionStorage;
  // keep tests independent.
  sessionStorage.clear();
});

const prFile = {
  sha: "f1",
  filename: "a.txt",
  status: "modified",
  additions: 1,
  deletions: 0,
  changes: 1,
  patch: "@@ -1,1 +1,1 @@\n+hello",
};

describe("PRFilesView review comment", () => {
  it("invalidates the pr-timeline cache (not the stale issue-timeline key) after commenting", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls/7/files")) return Promise.resolve(jsonResponse([prFile]));
      if (init?.method === "POST") return Promise.resolve(jsonResponse({ id: 1 }));
      return Promise.resolve(jsonResponse({}));
    });

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");

    render(
      <QueryClientProvider client={queryClient}>
        <PRFilesView owner="admin" repo="test" number={7} headSha="deadbeef" />
      </QueryClientProvider>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Comment on a.txt line 1" }));
    fireEvent.change(screen.getByLabelText("Review comment"), { target: { value: "nit: typo" } });
    fireEvent.click(screen.getByRole("button", { name: "Add single comment" }));

    await waitFor(() => {
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ["pr-timeline", "admin", "test", 7] });
    });
    const keys = invalidateSpy.mock.calls.map((c) => JSON.stringify(c[0]));
    expect(keys).not.toContain(JSON.stringify({ queryKey: ["issue-timeline", "admin", "test", 7] }));
  });

  it("drafts comments into a server-side PENDING review and submits via /events", async () => {
    // Stateful mock: creating the pending review makes it (and its draft
    // comment) visible to subsequent reads — the reload-safe persistence.
    let pendingReview: Record<string, unknown> | null = null;
    let createBody: unknown = null;
    let submitBody: unknown = null;
    const draftComment = {
      id: 51,
      pull_request_review_id: 9,
      diff_hunk: "@@ -1,1 +1,1 @@\n+hello",
      path: "a.txt",
      line: 1,
      side: "RIGHT",
      body: "please fix",
      user: { login: "admin", avatar_url: "" },
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    };
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls/7/files")) return Promise.resolve(jsonResponse([prFile]));
      if (u.endsWith("/api/v3/user")) {
        return Promise.resolve(jsonResponse({ id: 1, login: "admin", avatar_url: "", type: "User" }));
      }
      if (init?.method === "POST" && u.endsWith("/pulls/7/reviews")) {
        createBody = JSON.parse(String(init.body));
        pendingReview = { id: 9, user: { login: "admin", avatar_url: "" }, body: "", state: "PENDING", commit_id: "deadbeef", submitted_at: null };
        return Promise.resolve(jsonResponse(pendingReview));
      }
      if (init?.method === undefined && u.endsWith("/pulls/7/reviews")) {
        return Promise.resolve(jsonResponse(pendingReview ? [pendingReview] : []));
      }
      if (init?.method === undefined && u.endsWith("/pulls/7/reviews/9/comments")) {
        return Promise.resolve(jsonResponse([draftComment]));
      }
      if (init?.method === "POST" && u.endsWith("/pulls/7/reviews/9/events")) {
        submitBody = JSON.parse(String(init.body));
        return Promise.resolve(jsonResponse({ ...pendingReview, state: "APPROVED" }));
      }
      if (init?.method === "PUT" && u.endsWith("/pulls/7/reviews/9")) {
        return Promise.resolve(jsonResponse({ ...pendingReview, body: "LGTM overall" }));
      }
      if (u.endsWith("/pulls/7/comments")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({}));
    });

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <PRFilesView owner="admin" repo="test" number={7} headSha="deadbeef" />
      </QueryClientProvider>,
    );

    // Accumulate one pending line comment via "Start a review" — it must be
    // created server-side as a PENDING review (POST with no `event`).
    fireEvent.click(await screen.findByRole("button", { name: "Comment on a.txt line 1" }));
    fireEvent.change(screen.getByLabelText("Review comment"), { target: { value: "please fix" } });
    fireEvent.click(screen.getByRole("button", { name: "Start a review" }));

    await waitFor(() => expect(createBody).not.toBeNull());
    expect(createBody).toMatchObject({
      body: "",
      comments: [{ path: "a.txt", line: 1, side: "RIGHT", body: "please fix" }],
    });
    expect((createBody as Record<string, unknown>).event).toBeUndefined();

    // The pending count surfaces on the review button, and the draft renders
    // with its Pending badge under the diff.
    const finishBtn = await screen.findByRole("button", { name: "Finish your review (1)" });
    expect(await screen.findByText("Pending")).toBeInTheDocument();
    expect(screen.getByText("please fix")).toBeInTheDocument();

    // Submit as Approve through the popover — POST /reviews/{id}/events.
    fireEvent.click(finishBtn);
    fireEvent.change(await screen.findByLabelText("Review summary"), { target: { value: "LGTM overall" } });
    fireEvent.click(screen.getByRole("radio", { name: /^approve/i }));
    fireEvent.click(screen.getByRole("button", { name: /^submit review$/i }));

    await waitFor(() => expect(submitBody).not.toBeNull());
    expect(submitBody).toEqual({ event: "APPROVE" });
    // The edited summary was saved onto the pending review before submitting.
    const putCall = mockFetch.mock.calls.find(
      (c) => c[0]!.toString().endsWith("/pulls/7/reviews/9") && (c[1] as RequestInit | undefined)?.method === "PUT",
    );
    expect(putCall).toBeDefined();
    expect(JSON.parse(String((putCall![1] as RequestInit).body))).toEqual({ body: "LGTM overall" });
  });

  it("includes start_line when a multi-line range is set", async () => {
    const multi = { ...prFile, filename: "m.txt", patch: "@@ -0,0 +1,3 @@\n+line1\n+line2\n+line3" };
    let commentBody: unknown = null;
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls/7/files")) return Promise.resolve(jsonResponse([multi]));
      if (init?.method === "POST" && u.endsWith("/pulls/7/comments")) {
        commentBody = JSON.parse(String(init.body));
        return Promise.resolve(jsonResponse({ id: 3 }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <PRFilesView owner="admin" repo="test" number={7} headSha="deadbeef" />
      </QueryClientProvider>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Comment on m.txt line 3" }));
    fireEvent.change(screen.getByLabelText("Start line (optional)"), { target: { value: "1" } });
    fireEvent.change(screen.getByLabelText("Review comment"), { target: { value: "spans lines" } });
    fireEvent.click(screen.getByRole("button", { name: "Add single comment" }));

    await waitFor(() => expect(commentBody).not.toBeNull());
    expect(commentBody).toMatchObject({ path: "m.txt", line: 3, side: "RIGHT", start_line: 1 });
  });
});

function renderFiles(number = 7) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <PRFilesView owner="admin" repo="test" number={number} headSha="deadbeef" />
    </QueryClientProvider>,
  );
}

function reviewComment(id: number, overrides: Record<string, unknown> = {}) {
  return {
    id,
    pull_request_review_id: 3,
    diff_hunk: "@@ -1,1 +1,1 @@\n+hello",
    path: "a.txt",
    line: 1,
    side: "RIGHT",
    body: `comment ${id}`,
    user: { login: "carol", avatar_url: "" },
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

describe("PRFilesView inline threads", () => {
  it("renders existing review threads under their diff row with a reply box and resolve control", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls/7/files")) return Promise.resolve(jsonResponse([prFile]));
      if (u.endsWith("/pulls/7/comments") && init?.method === undefined) {
        return Promise.resolve(jsonResponse([reviewComment(61, { body: "existing thread" })]));
      }
      if (u.endsWith("/api/graphql") && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({
            data: {
              repository: {
                pullRequest: {
                  reviewThreads: {
                    nodes: [
                      { id: "T1", isResolved: false, comments: { nodes: [{ databaseId: 61 }] } },
                    ],
                  },
                },
              },
            },
          }),
        );
      }
      if (u.endsWith("/pulls/7/reviews")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });

    renderFiles();

    expect(await screen.findByText("existing thread")).toBeInTheDocument();
    // The thread sits under its diff row with the resolve control (wired to
    // the GraphQL thread id) and a reply composer.
    expect(await screen.findByRole("button", { name: /^resolve$/i })).toBeInTheDocument();
    expect(screen.getByLabelText("reply to thread on a.txt")).toBeInTheDocument();
  });

  it("renders ```suggestion fences as a mini-diff without a commit action", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls/7/files")) return Promise.resolve(jsonResponse([prFile]));
      if (u.endsWith("/pulls/7/comments") && init?.method === undefined) {
        return Promise.resolve(
          jsonResponse([
            reviewComment(62, { body: "Try this:\n```suggestion\nhello world\n```" }),
          ]),
        );
      }
      if (u.endsWith("/pulls/7/reviews")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });

    renderFiles();

    expect(await screen.findByText("Suggested change")).toBeInTheDocument();
    // Suggested replacement inserted, original line struck through.
    expect(screen.getByText("hello world")).toBeInTheDocument();
    expect(document.querySelector("del")?.textContent).toBe("hello");
    // The server has no apply-suggestion endpoint, so render-only.
    expect(screen.queryByRole("button", { name: /commit suggestion/i })).not.toBeInTheDocument();
  });
});

describe("PRFilesView per-file controls", () => {
  function filesOnlyMock() {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/pulls/7/files")) return Promise.resolve(jsonResponse([prFile]));
      if (u.endsWith("/pulls/7/reviews") || u.endsWith("/pulls/7/comments")) {
        return Promise.resolve(jsonResponse([]));
      }
      return Promise.resolve(jsonResponse({}));
    });
  }

  it("marking a file Viewed collapses it and persists per PR in sessionStorage", async () => {
    filesOnlyMock();
    renderFiles();

    expect(await screen.findByText("+hello")).toBeInTheDocument();
    fireEvent.click(screen.getByLabelText("Viewed a.txt"));
    expect(screen.queryByText("+hello")).not.toBeInTheDocument();
    expect(JSON.parse(sessionStorage.getItem("bleephub:pr-viewed:admin/test#7") ?? "[]")).toEqual([
      "a.txt",
    ]);

    // The chevron re-expands the diff while the file stays viewed.
    fireEvent.click(screen.getByRole("button", { name: "Toggle diff for a.txt" }));
    expect(screen.getByText("+hello")).toBeInTheDocument();
    expect((screen.getByLabelText("Viewed a.txt") as HTMLInputElement).checked).toBe(true);
  });

  it("collapses and re-expands a file diff via the chevron", async () => {
    filesOnlyMock();
    renderFiles();

    expect(await screen.findByText("+hello")).toBeInTheDocument();
    const toggle = screen.getByRole("button", { name: "Toggle diff for a.txt" });
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    fireEvent.click(toggle);
    expect(screen.queryByText("+hello")).not.toBeInTheDocument();
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    expect(screen.getByText("+hello")).toBeInTheDocument();
  });
});
