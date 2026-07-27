import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { OverviewPage } from "../pages/OverviewPage.js";

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
        <OverviewPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const healthData = { status: "ok", service: "bleephub", enterprise_slug: "bleephub" };
const workflowsData = [
  {
    id: 1,
    name: "CI Build",
    run_number: 1,
    run_attempt: 1,
    event: "push",
    status: "completed",
    conclusion: "success",
    head_branch: "main",
    head_sha: "abc",
    path: ".github/workflows/ci.yml",
    workflow_id: 1234,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    actor: { login: "admin" },
  },
];
const reposData = [{ id: 1, name: "test", full_name: "admin/test", default_branch: "main", owner: { login: "admin", type: "User" } }];
const jobsData = [{
  id: 101,
  run_id: 1,
  name: "build",
  status: "completed",
  conclusion: "success",
  started_at: "2026-01-01T00:00:01Z",
  completed_at: "2026-01-01T00:00:02Z",
  steps: [],
  labels: ["self-hosted"],
  run_attempt: 1,
}];
const internalMetrics = {
  workflow_submissions: 4,
  job_dispatches: 9,
  job_completions: { success: 7, failure: 2 },
  active_workflows: 0,
};
const internalStatus = {
  active_workflows: 0,
  jobs_by_status: { completed: 9 },
  connected_runners: 1,
};

function mockAllEndpoints(internalStatusCode = 200) {
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
    if (url.includes("/health")) return Promise.resolve(jsonResponse(healthData));
    if (url.includes("/api/v3/user/repos")) return Promise.resolve(jsonResponse(reposData));
    if (url.includes("/actions/runs/1/jobs")) return Promise.resolve(jsonResponse({ total_count: 1, jobs: jobsData }));
    if (url.includes("/actions/runs")) return Promise.resolve(jsonResponse({ total_count: 1, workflow_runs: workflowsData }));
    return Promise.resolve(jsonResponse({}));
  });
}

describe("OverviewPage", () => {
  it("renders the overview heading", async () => {
    mockAllEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /system status/i })).toBeInTheDocument();
    });
  });

  it("renders health badge", async () => {
    mockAllEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("ok")).toBeInTheDocument();
    });
  });

  it("renders metrics cards", async () => {
    mockAllEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/active workflows/i)).toBeInTheDocument();
      expect(screen.getAllByText("0").length).toBeGreaterThan(0);
      expect(screen.getByText(/connected runners/i)).toBeInTheDocument();
      expect(screen.getAllByText("1").length).toBeGreaterThan(0);
      // The server counts submissions; it exposes no total of stored runs, so
      // the card must not claim to show one.
      expect(screen.getByText("Workflow submissions")).toBeInTheDocument();
      expect(screen.queryByText("Workflow runs")).not.toBeInTheDocument();
      expect(screen.getByText("4")).toBeInTheDocument();
    });
  });

  it("renders recent workflows table", async () => {
    mockAllEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/recent workflows/i)).toBeInTheDocument();
      expect(screen.getByText("CI Build")).toBeInTheDocument();
    });
  });

  it("reads the counters from the server instead of walking every repository", async () => {
    mockAllEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/system status/i)).toBeInTheDocument();
    });
    const urls = mockFetch.mock.calls.map((call) => String(call[0]));
    expect(urls.filter((url) => url.includes("/internal/metrics"))).toHaveLength(1);
    expect(urls.filter((url) => url.includes("/internal/status"))).toHaveLength(1);
    // The old client-side aggregate paged every repo's runs and then every
    // run's jobs, and asked each repo for its runners.
    expect(urls.some((url) => url.includes("/actions/runners"))).toBe(false);
  });

  it("shows the figures to a site admin", async () => {
    mockAllEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Workflow submissions")).toBeInTheDocument();
    });
    expect(screen.getByText("4")).toBeInTheDocument();
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("explains the figures are operator-only to a non-admin, without raising an error", async () => {
    mockAllEndpoints(403);
    renderPage();

    const note = await screen.findByRole("note");
    expect(note).toHaveTextContent(/instance diagnostics and require site admin/i);
    // A refusal is an answer, not a failure: no alert, no error banner.
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByText(/Failed to load overview/i)).not.toBeInTheDocument();
    // The cards keep their shape and the rest of the console still works.
    expect(screen.getByText("Connected runners")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /system status/i })).toBeInTheDocument();
    expect(screen.getByText("CI Build")).toBeInTheDocument();
  });

  it("raises a real error surface when the metrics fetch faults", async () => {
    mockAllEndpoints(500);
    renderPage();

    // A fault (unlike a refusal) is retried once before it settles.
    expect(
      await screen.findByText(/Failed to load overview/i, undefined, { timeout: 5000 }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
  });

  it("stops polling once the server has refused", async () => {
    mockAllEndpoints(403);
    renderPage();
    await screen.findByRole("note");

    const before = mockFetch.mock.calls.filter((c) =>
      String(c[0]).includes("/internal/metrics"),
    ).length;
    await new Promise((resolve) => setTimeout(resolve, 150));
    const after = mockFetch.mock.calls.filter((c) =>
      String(c[0]).includes("/internal/metrics"),
    ).length;
    // A 403 will not change on retry; re-asking every 5s just makes noise.
    expect(after).toBe(before);
  });
});
