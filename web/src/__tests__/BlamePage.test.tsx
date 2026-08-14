import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { BlamePage } from "../pages/BlamePage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderAt(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/repos/:owner/:repo/blame/:ref/*" element={<BlamePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("BlamePage", () => {
  it("renders blame hunks: commit gutter + line numbers + code", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/ui-data/repos/admin/demo/blame/src/app.ts")) {
        return Promise.resolve(
          jsonResponse({
            path: "src/app.ts",
            ref: "main",
            sha: "c".repeat(40),
            hunks: [
              {
                sha: "a".repeat(40),
                short_sha: "aaaaaaa",
                summary: "Initial import",
                author: "admin",
                date: "2026-01-01T00:00:00Z",
                start_line: 1,
                lines: ["line one", "line two"],
              },
              {
                sha: "b".repeat(40),
                short_sha: "bbbbbbb",
                summary: "Add feature",
                author: "octo",
                date: "2026-02-01T00:00:00Z",
                start_line: 3,
                lines: ["line three"],
              },
            ],
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/demo/blame/main/src/app.ts");

    // Commit gutters link to each commit; both summaries render.
    await waitFor(() =>
      expect(screen.getByRole("link", { name: "aaaaaaa" })).toHaveAttribute(
        "href",
        `/ui/repos/admin/demo/commits/${"a".repeat(40)}`,
      ),
    );
    expect(screen.getByText("Initial import")).toBeInTheDocument();
    expect(screen.getByText("Add feature")).toBeInTheDocument();
    // Every source line rendered.
    expect(screen.getByText("line one")).toBeInTheDocument();
    expect(screen.getByText("line three")).toBeInTheDocument();
    // The "View file" link points back at the blob.
    expect(screen.getByRole("link", { name: "View file" })).toHaveAttribute(
      "href",
      "/ui/repos/admin/demo/blob/main/src/app.ts",
    );
  });
});
