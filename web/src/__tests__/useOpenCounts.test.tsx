import type { ReactNode } from "react";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, test, vi } from "vitest";

vi.mock("../api.js", () => ({
  fetchRepoIssuesPage: vi.fn(),
  fetchRepoPRsPage: vi.fn(),
}));

import { fetchRepoIssuesPage, fetchRepoPRsPage } from "../api.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";

function makeWrapper() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
}

describe("useOpenCounts", () => {
  test("issue badge excludes pull requests returned by /issues", async () => {
    // The /issues endpoint returns issues AND pull requests; the Issues badge
    // must count only real issues, while the /pulls badge counts its page.
    vi.mocked(fetchRepoIssuesPage).mockResolvedValue({
      items: [
        { id: 1, pull_request: null },
        { id: 2, pull_request: null },
        { id: 3, pull_request: { url: "http://x/pulls/3" } }, // a PR listed under /issues
      ],
      nextUrl: null,
    } as never);
    vi.mocked(fetchRepoPRsPage).mockResolvedValue({
      items: [{ id: 3 }],
      nextUrl: null,
    } as never);

    const { result } = renderHook(() => useOpenCounts("octo", "repo"), { wrapper: makeWrapper() });

    await waitFor(() => expect(result.current.issueCount).toBe(2));
    expect(result.current.prCount).toBe(1);
  });
});
