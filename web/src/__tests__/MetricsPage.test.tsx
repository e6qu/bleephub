import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { MetricsPage } from "../pages/MetricsPage.js";

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

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <MetricsPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const internalMetrics = {
  workflow_submissions: 2,
  job_dispatches: 2,
  job_completions: { success: 1 },
  active_workflows: 1,
  uptime_seconds: 3661,
  goroutines: 42,
  heap_alloc_mb: 12.5,
  job_duration_p50_seconds: 1.2,
  job_duration_p95_seconds: 3.4,
  job_duration_p99_seconds: 5.6,
};
const internalStatus = {
  active_workflows: 1,
  jobs_by_status: { completed: 1, in_progress: 1 },
  connected_runners: 1,
};

function mockEndpoints(internalStatusCode = 200) {
  mockFetch.mockImplementation((url: string) => {
    if (url.includes("/internal/")) {
      if (internalStatusCode !== 200) {
        return Promise.resolve(
          new Response("denied", { status: internalStatusCode, statusText: "Forbidden" }),
        );
      }
      return Promise.resolve(
        jsonResponse(url.includes("/internal/metrics") ? internalMetrics : internalStatus),
      );
    }
    return Promise.resolve(jsonResponse({}));
  });
}

describe("MetricsPage", () => {
  it("renders the metrics heading", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /actions throughput/i })).toBeInTheDocument();
    });
  });

  it("renders metrics cards", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getAllByText(/workflow submissions/i).length).toBeGreaterThan(0);
      expect(screen.getAllByText("2").length).toBeGreaterThan(0);
      expect(screen.getByText(/job dispatches/i)).toBeInTheDocument();
      expect(screen.getAllByText(/connected runners/i).length).toBeGreaterThan(0);
      expect(screen.getByText(/2 workflow submissions/i)).toBeInTheDocument();
      // No card may be labelled with a total the server does not report.
      expect(screen.queryByText("Workflow runs")).not.toBeInTheDocument();
    });
  });

  it("renders job completions breakdown", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/job completions/i)).toBeInTheDocument();
    });
  });

  it("renders jobs by status section", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/jobs by status/i)).toBeInTheDocument();
    });
  });

  it("renders job latency percentiles and runtime health from the same two requests", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/job latency/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/p95 duration/i)).toBeInTheDocument();
    expect(screen.getByText("3.40 s")).toBeInTheDocument();
    // Uptime 3661s formats to "1h 1m".
    expect(screen.getByText(/runtime/i)).toBeInTheDocument();
    expect(screen.getByText("1h 1m")).toBeInTheDocument();
    expect(screen.getByText(/goroutines/i)).toBeInTheDocument();
    expect(screen.getByText("12.5 MB")).toBeInTheDocument();
    const urls = mockFetch.mock.calls.map((call) => String(call[0]));
    expect(urls.filter((url) => url.includes("/internal/"))).toHaveLength(2);
  });

  it("costs exactly two requests instead of walking every repository", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /actions throughput/i })).toBeInTheDocument();
    });
    const urls = mockFetch.mock.calls.map((call) => String(call[0]));
    expect(urls.filter((url) => url.includes("/internal/metrics"))).toHaveLength(1);
    expect(urls.filter((url) => url.includes("/internal/status"))).toHaveLength(1);
    expect(urls.some((url) => url.includes("/api/v3/"))).toBe(false);
    expect(urls.some((url) => url.includes("/internal/storage"))).toBe(false);
  });

  it("shows the figures to a site admin", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getAllByText(/workflow submissions/i).length).toBeGreaterThan(0);
    });
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("explains the figures are operator-only to a non-admin, without raising an error", async () => {
    mockEndpoints(403);
    renderPage();

    const note = await screen.findByRole("note");
    expect(note).toHaveTextContent(/instance diagnostics and require site admin/i);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByText(/Failed to load metrics/i)).not.toBeInTheDocument();
    // Card titles stay so the page still names what is unavailable.
    expect(screen.getByText("Connected runners")).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: /actions throughput/i }),
    ).toBeInTheDocument();
  });

  it("raises a real error surface when the metrics fetch faults", async () => {
    mockEndpoints(500);
    renderPage();

    // A fault (unlike a refusal) is retried once before it settles.
    expect(
      await screen.findByText(/Failed to load metrics/i, undefined, { timeout: 5000 }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
  });
});
