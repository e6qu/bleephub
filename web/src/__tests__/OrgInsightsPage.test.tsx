import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgInsightsPage } from "../pages/OrgInsightsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/ui/orgs/acme/insights"]}>
        <Routes>
          <Route path="/ui/orgs/:org/insights" element={<OrgInsightsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("OrgInsightsPage", () => {
  it("renders the API-request summary and the top subjects table", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/insights/api/summary-stats"))
        return Promise.resolve(jsonResponse({ total_request_count: 1234, rate_limited_request_count: 5, last_request_timestamp: "2026-02-01T00:00:00Z", last_rate_limited_timestamp: null }));
      if (u.includes("/insights/api/subject-stats"))
        return Promise.resolve(jsonResponse([{ subject_name: "ci-bot", total_request_count: 900, rate_limited_request_count: 3 }, { subject_name: "web", total_request_count: 334, rate_limited_request_count: 0 }]));
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();

    expect(await screen.findByText("1,234")).toBeInTheDocument();
    expect(screen.getByText("Total API requests")).toBeInTheDocument();
    expect(await screen.findByText("ci-bot")).toBeInTheDocument();
    // Highest request-count subject sorts first.
    const rowsText = screen.getAllByRole("row").map((r) => r.textContent);
    expect(rowsText.some((t) => t?.includes("ci-bot") && t?.includes("900"))).toBe(true);
  });

  it("shows an empty state when there is no API activity", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/summary-stats")) return Promise.resolve(jsonResponse({ total_request_count: 0, rate_limited_request_count: 0, last_request_timestamp: "", last_rate_limited_timestamp: null }));
      if (u.includes("/subject-stats")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText(/no api activity/i)).toBeInTheDocument());
  });
});
