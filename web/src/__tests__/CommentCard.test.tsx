import { describe, it, expect, afterEach, vi } from "vitest";
import { render, cleanup, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CommentCard, EditableCommentList } from "../components/CommentCard.js";

afterEach(cleanup);

describe("CommentCard", () => {
  it("renders the body as GitHub-flavored markdown, not raw text", () => {
    render(
      <CommentCard
        login="octocat"
        date="2026-01-01T00:00:00Z"
        body={"### Steps\n\n- [x] done\n\nSome `inline code` here."}
      />,
    );
    // The heading renders as an <h3>, not the literal "### Steps".
    const heading = screen.getByRole("heading", { level: 3, name: "Steps" });
    expect(heading).toBeInTheDocument();
    // Inline code renders as a <code> element.
    expect(screen.getByText("inline code").tagName).toBe("CODE");
    // The raw markdown tokens are not shown verbatim.
    expect(screen.queryByText(/### Steps/)).toBeNull();
  });

  it("shows a placeholder when the body is empty", () => {
    render(<CommentCard login="octocat" date="2026-01-01T00:00:00Z" body="" />);
    expect(screen.getByText("No description provided.")).toBeInTheDocument();
  });
});

describe("EditableCommentList reactions", () => {
  it("reacts on an individual issue comment via its reactions endpoint", async () => {
    const mockFetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/issues/comments/42/reactions") && init?.method === "POST") {
        return Promise.resolve(new Response(JSON.stringify({ id: 9, content: "heart" }), { status: 201, headers: { "Content-Type": "application/json" } }));
      }
      return Promise.resolve(new Response(JSON.stringify([]), { status: 200, headers: { "Content-Type": "application/json" } }));
    });
    globalThis.fetch = mockFetch as typeof fetch;
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <EditableCommentList
          owner="admin"
          repo="r"
          viewerLogin="admin"
          items={[{ event: "commented", id: 42, body: "a comment", created_at: "2026-01-01T00:00:00Z", user: { login: "admin" } } as never]}
          invalidateKeys={[]}
        />
      </QueryClientProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "add reaction" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "react with heart" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(([u, i]) => String(u).endsWith("/issues/comments/42/reactions") && i?.method === "POST");
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toEqual({ content: "heart" });
    });
  });
});
