import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { RunDetailPage } from "../pages/RunDetailPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function textResponse(text: string) {
  return new Response(text, {
    status: 200,
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderPage(runId = 5) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[`/ui/repos/admin/test/actions/runs/${runId}`]}>
        <Routes>
          <Route path="/ui/repos/:owner/:repo/actions/runs/:runId" element={<RunDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function runData(overrides: Record<string, unknown> = {}) {
  return {
    id: 5,
    name: "CI",
    run_number: 12,
    run_attempt: 1,
    event: "push",
    status: "completed",
    conclusion: "success",
    head_branch: "main",
    head_sha: "abc1234567890",
    path: ".github/workflows/ci.yml",
    workflow_id: 10,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:05:00Z",
    actor: { login: "admin" },
    ...overrides,
  };
}

function jobData(overrides: Record<string, unknown> = {}) {
  return {
    id: 99,
    run_id: 5,
    name: "build",
    status: "completed",
    conclusion: "success",
    started_at: "2026-01-01T00:00:10Z",
    completed_at: "2026-01-01T00:01:40Z",
    run_attempt: 1,
    labels: ["ubuntu-latest"],
    steps: [
      {
        name: "Set up job",
        status: "completed",
        conclusion: "success",
        number: 1,
        started_at: "2026-01-01T00:00:10Z",
        completed_at: "2026-01-01T00:00:20Z",
      },
      {
        name: "Run tests",
        status: "completed",
        conclusion: "success",
        number: 2,
        started_at: "2026-01-01T00:00:20Z",
        completed_at: "2026-01-01T00:01:40Z",
      },
    ],
    ...overrides,
  };
}

interface MockState {
  run?: Record<string, unknown>;
  job?: Record<string, unknown>;
  logs?: string;
  artifacts?: unknown[];
  pending?: unknown[];
  summary?: string;
}

function installMocks({
  run = runData(),
  job = jobData(),
  logs = "",
  artifacts = [],
  pending = [],
  summary = "",
}: MockState = {}) {
  mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
    const u = url.toString();
    const method = init?.method ?? "GET";
    if (u.includes("/issues") || u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
    if (method === "POST" && u.endsWith("/cancel")) {
      return Promise.resolve(new Response(null, { status: 202 }));
    }
    if (method === "POST" && u.endsWith("/rerun")) {
      return Promise.resolve(new Response(null, { status: 201 }));
    }
    if (method === "POST" && u.endsWith("/rerun-failed-jobs")) {
      return Promise.resolve(new Response(null, { status: 201 }));
    }
    if (u.endsWith("/pending_deployments") && method === "POST") {
      return Promise.resolve(jsonResponse([]));
    }
    if (u.endsWith("/pending_deployments")) return Promise.resolve(jsonResponse(pending));
    if (u.includes("/attempts/")) return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    if (u.endsWith("/jobs?per_page=100")) {
      return Promise.resolve(jsonResponse({ total_count: 1, jobs: [job] }));
    }
    if (u.includes("/actions/jobs/") && u.endsWith("/logs")) {
      return Promise.resolve(textResponse(logs));
    }
    if (u.includes("/internal/repos/") && u.endsWith("/summary")) {
      return Promise.resolve(jsonResponse({ summary }));
    }
    if (u.endsWith("/artifacts")) {
      return Promise.resolve(jsonResponse({ total_count: artifacts.length, artifacts }));
    }
    if (u.endsWith("/actions/runs/5")) return Promise.resolve(jsonResponse(run));
    return Promise.resolve(jsonResponse([]));
  });
}

describe("RunDetailPage", () => {
  it("renders the run header, job sidebar and step list with durations", async () => {
    installMocks();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /CI/ })).toBeInTheDocument();
    });
    expect(screen.getByText("#12")).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: /build/ })).toBeInTheDocument();
    expect(await screen.findByText("Set up job")).toBeInTheDocument();
    expect(screen.getByText("Run tests")).toBeInTheDocument();
    // Step 2 ran 80s → "1m 20s".
    expect(screen.getByText("1m 20s")).toBeInTheDocument();
  });

  it("shows Cancel while in progress and POSTs the cancel endpoint", async () => {
    installMocks({
      run: runData({ status: "in_progress", conclusion: null }),
      job: jobData({ status: "in_progress", conclusion: null, completed_at: null }),
    });
    renderPage();
    const cancelBtn = await screen.findByRole("button", { name: /cancel workflow/i });
    expect(screen.queryByRole("button", { name: /re-run all jobs/i })).not.toBeInTheDocument();
    fireEvent.click(cancelBtn);
    await waitFor(() => {
      const call = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/actions/runs/5/cancel") && c[1]?.method === "POST",
      );
      expect(call).toBeTruthy();
    });
  });

  it("force-cancels an in-progress run via POST /force-cancel", async () => {
    installMocks({
      run: runData({ status: "in_progress", conclusion: null }),
      job: jobData({ status: "in_progress", conclusion: null, completed_at: null }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Force cancel" }));
    await waitFor(() => {
      const call = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/actions/runs/5/force-cancel") && c[1]?.method === "POST",
      );
      expect(call).toBeTruthy();
    });
  });

  it("approves a waiting run via POST /approve", async () => {
    installMocks({ run: runData({ status: "waiting", conclusion: null }) });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    await waitFor(() => {
      const call = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/actions/runs/5/approve") && c[1]?.method === "POST",
      );
      expect(call).toBeTruthy();
    });
  });

  it("offers re-run buttons for a completed failed run and hits the right endpoints", async () => {
    installMocks({
      run: runData({ conclusion: "failure" }),
      job: jobData({ conclusion: "failure" }),
    });
    renderPage();
    const rerunAll = await screen.findByRole("button", { name: /re-run all jobs/i });
    const rerunFailed = screen.getByRole("button", { name: /re-run failed jobs/i });
    expect(screen.queryByRole("button", { name: /cancel workflow/i })).not.toBeInTheDocument();
    fireEvent.click(rerunAll);
    fireEvent.click(rerunFailed);
    await waitFor(() => {
      const calls = mockFetch.mock.calls
        .filter((c) => c[1]?.method === "POST")
        .map((c) => c[0].toString());
      expect(calls).toContain("/api/v3/repos/admin/test/actions/runs/5/rerun");
      expect(calls).toContain("/api/v3/repos/admin/test/actions/runs/5/rerun-failed-jobs");
    });
  });

  it("re-runs an individual job from the job pane", async () => {
    installMocks({ run: runData({ conclusion: "failure" }), job: jobData({ conclusion: "failure" }) });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Re-run job" }));
    await waitFor(() => {
      const calls = mockFetch.mock.calls.filter((c) => c[1]?.method === "POST").map((c) => c[0].toString());
      expect(calls.some((u) => /\/actions\/jobs\/\d+\/rerun$/.test(u))).toBe(true);
    });
  });

  it("expands a step to its ##[group] log slice when markers segment the log", async () => {
    const logs = [
      "2026-01-01T00:00:10Z ##[group]Set up job",
      "2026-01-01T00:00:11Z runner version 2.320.0",
      "2026-01-01T00:00:12Z ##[endgroup]",
      "2026-01-01T00:00:20Z ##[group]Run tests",
      "2026-01-01T00:00:21Z 1 passed, 0 failed",
      "2026-01-01T00:00:22Z ##[endgroup]",
    ].join("\n");
    installMocks({ logs });
    renderPage();
    // The step row becomes expandable (aria-expanded appears) once the
    // log has loaded and its ##[group] segment matched the step name.
    const stepBtn = await screen.findByRole("button", { name: /run tests/i });
    await waitFor(() => expect(stepBtn).toHaveAttribute("aria-expanded", "false"));
    expect(screen.queryByText(/1 passed, 0 failed/)).not.toBeInTheDocument();
    fireEvent.click(stepBtn);
    expect(await screen.findByText(/1 passed, 0 failed/)).toBeInTheDocument();
    expect(screen.queryByText(/runner version/)).not.toBeInTheDocument();
  });

  it("falls back to the full job log when no group markers are present", async () => {
    installMocks({ logs: "2026-01-01T00:00:10Z hello from the runner\n" });
    renderPage();
    expect(await screen.findByText("Job log")).toBeInTheDocument();
    expect(screen.getByText(/hello from the runner/)).toBeInTheDocument();
  });

  it("renders the persisted GitHub step summary", async () => {
    installMocks({ summary: "## Test report\n\n- **12** passed" });
    renderPage();
    expect(await screen.findByText("Job summary")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Test report" })).toBeInTheDocument();
    expect(screen.getByText("12", { selector: "strong" })).toBeInTheDocument();
  });

  it("lists artifacts with a direct zip download link", async () => {
    installMocks({
      artifacts: [
        { id: 7, name: "dist", size_in_bytes: 2048, expired: false, created_at: "2026-01-01T00:02:00Z" },
      ],
    });
    renderPage();
    expect(await screen.findByText("Artifacts")).toBeInTheDocument();
    expect(screen.getByText("dist")).toBeInTheDocument();
    expect(screen.getByText("2.0 KB")).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "Download" });
    expect(link).toHaveAttribute("href", "/api/v3/repos/admin/test/actions/artifacts/7/zip");
    // Run-level "Download log archive" links the logs zip endpoint.
    expect(screen.getByRole("link", { name: "Download log archive" })).toHaveAttribute(
      "href",
      "/api/v3/repos/admin/test/actions/runs/5/logs",
    );
  });

  it("shows the approval banner for a waiting run and POSTs the review", async () => {
    installMocks({
      run: runData({ status: "waiting", conclusion: null }),
      job: jobData({ status: "waiting", conclusion: null, completed_at: null }),
      pending: [
        {
          environment: { id: 3, name: "production" },
          wait_timer: 0,
          wait_timer_started_at: "2026-01-01T00:00:00Z",
          current_user_can_approve: true,
          reviewers: [],
        },
      ],
    });
    renderPage();
    expect(await screen.findByText(/waiting for review to deploy/i)).toBeInTheDocument();
    expect(screen.getByText("production")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/leave a comment/i), { target: { value: "lgtm" } });
    fireEvent.click(screen.getByRole("button", { name: /approve and deploy/i }));
    await waitFor(() => {
      const call = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/pending_deployments") && c[1]?.method === "POST",
      );
      expect(call).toBeTruthy();
      expect(JSON.parse(call![1]!.body as string)).toEqual({
        environment_ids: [3],
        state: "approved",
        comment: "lgtm",
      });
    });
  });

  it("deletes a completed workflow run after confirming and navigates away", async () => {
    installMocks();
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete workflow run" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => /\/actions\/runs\/\d+$/.test(c[0].toString()) && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("deletes all logs for a completed run after confirming", async () => {
    installMocks();
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete all logs" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => /\/actions\/runs\/\d+\/logs$/.test(c[0].toString()) && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });
});

describe("RunDetailPage — log viewer", () => {
  it("filters log lines via the search box with highlighted matches and a count", async () => {
    installMocks({
      logs: [
        "2026-01-01T00:00:10Z compiling module alpha",
        "2026-01-01T00:00:11Z tests passed for alpha",
        "2026-01-01T00:00:12Z uploading artifact",
      ].join("\n"),
    });
    renderPage();
    await screen.findByText("Job log");
    fireEvent.change(screen.getByLabelText("Search logs"), { target: { value: "alpha" } });
    expect(await screen.findByText(/2 matching lines in this job’s log/)).toBeInTheDocument();
    // Matches are wrapped in <mark>.
    await waitFor(() => {
      expect(document.querySelectorAll("mark").length).toBeGreaterThanOrEqual(2);
    });
    // The sidebar job button shows its match-count badge.
    expect(screen.getByLabelText("2 matching log lines")).toBeInTheDocument();
  });

  it("hides runner timestamps by default and shows them via the toggle", async () => {
    installMocks({ logs: "2026-01-01T00:00:10Z hello from the runner\n" });
    renderPage();
    expect(await screen.findByText("Job log")).toBeInTheDocument();
    // Timestamp prefix stripped by default.
    expect(screen.getByText("hello from the runner")).toBeInTheDocument();
    expect(screen.queryByText(/2026-01-01T00:00:10Z hello/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByLabelText(/show timestamps/i));
    expect(await screen.findByText(/2026-01-01T00:00:10Z hello from the runner/)).toBeInTheDocument();
  });

  it("links the per-job raw log view", async () => {
    installMocks();
    renderPage();
    const raw = await screen.findByRole("link", { name: "View raw logs" });
    expect(raw).toHaveAttribute("href", "/api/v3/repos/admin/test/actions/jobs/99/logs");
  });
});

describe("RunDetailPage — job dependencies", () => {
  it("annotates sidebar jobs with their needs from the workflow file", async () => {
    const yaml = [
      "name: CI",
      "on: push",
      "jobs:",
      "  compile:",
      "    runs-on: ubuntu-latest",
      "    steps: []",
      "  build:",
      "    needs: compile",
      "    runs-on: ubuntu-latest",
      "    steps: []",
    ].join("\n");
    const encoded = btoa(yaml);
    installMocks();
    const base = mockFetch.getMockImplementation()!;
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/contents/")) {
        return Promise.resolve(
          jsonResponse({ name: "ci.yml", path: ".github/workflows/ci.yml", type: "file", encoding: "base64", content: encoded }),
        );
      }
      return base(url, init);
    });
    renderPage();
    // The single job in the fixture is named "build" and needs "compile".
    expect(await screen.findByText("needs: compile")).toBeInTheDocument();
  });
});

describe("RunDetailPage — check-run annotations", () => {
  it("renders annotations derived from the job's check_run_url", async () => {
    const job = jobData({
      check_run_url: "/api/v3/repos/admin/test/check-runs/42",
    });
    const annotation = {
      path: "main.go",
      start_line: 10,
      end_line: 10,
      annotation_level: "failure",
      message: "boom",
      title: "vet",
    };
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/check-runs/42/annotations")) {
        return Promise.resolve(jsonResponse([annotation]));
      }
      if (u.includes("/issues") || u.includes("/pulls")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/pending_deployments")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/attempts/")) return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      if (u.endsWith("/jobs?per_page=100")) {
        return Promise.resolve(jsonResponse({ total_count: 1, jobs: [job] }));
      }
      if (u.includes("/actions/jobs/") && u.endsWith("/logs")) {
        return Promise.resolve(textResponse(""));
      }
      if (u.includes("/internal/repos/") && u.endsWith("/summary")) {
        return Promise.resolve(jsonResponse({ summary: "" }));
      }
      if (u.endsWith("/artifacts")) {
        return Promise.resolve(jsonResponse({ total_count: 0, artifacts: [] }));
      }
      if (u.endsWith("/actions/runs/5")) return Promise.resolve(jsonResponse(runData()));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    expect(await screen.findByText("Annotations")).toBeInTheDocument();
    expect(screen.getByText("boom")).toBeInTheDocument();
    expect(screen.getByText("main.go:10")).toBeInTheDocument();
  });
});
