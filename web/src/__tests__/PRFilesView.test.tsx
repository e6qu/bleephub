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

  it("batches pending line comments and submits them with the review verdict", async () => {
    let reviewBody: unknown = null;
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/pulls/7/files")) return Promise.resolve(jsonResponse([prFile]));
      if (init?.method === "POST" && u.endsWith("/pulls/7/reviews")) {
        reviewBody = JSON.parse(String(init.body));
        return Promise.resolve(jsonResponse({ id: 9 }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <PRFilesView owner="admin" repo="test" number={7} headSha="deadbeef" />
      </QueryClientProvider>,
    );

    // Accumulate one pending line comment via "Start a review".
    fireEvent.click(await screen.findByRole("button", { name: "Comment on a.txt line 1" }));
    fireEvent.change(screen.getByLabelText("Review comment"), { target: { value: "please fix" } });
    fireEvent.click(screen.getByRole("button", { name: "Start a review" }));

    // The finish-review panel appears; submit as Approve.
    fireEvent.change(await screen.findByLabelText("Review summary"), { target: { value: "LGTM overall" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));

    await waitFor(() => expect(reviewBody).not.toBeNull());
    expect(reviewBody).toMatchObject({
      event: "APPROVE",
      body: "LGTM overall",
      comments: [{ path: "a.txt", line: 1, side: "RIGHT", body: "please fix" }],
    });
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
