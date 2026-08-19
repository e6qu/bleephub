import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { WorkflowsPage } from "../pages/WorkflowsPage.js";

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
        <WorkflowsPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const reposData = [
  {
    id: 1,
    name: "test",
    full_name: "admin/test",
    default_branch: "main",
    owner: { login: "admin", type: "User" },
  },
];

const workflowFilesData = [
  {
    id: 1234,
    name: "CI Build",
    path: ".github/workflows/ci.yml",
    state: "active",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    badge_url: "http://localhost/admin/test/actions/workflows/ci.yml/badge.svg",
  },
];

const workflowRunsData = [
  {
    id: 1,
    name: "CI Build",
    run_number: 1,
    run_attempt: 1,
    event: "push",
    status: "in_progress",
    conclusion: null,
    head_branch: "main",
    head_sha: "abc",
    path: ".github/workflows/ci.yml",
    workflow_id: 1234,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    actor: { login: "admin" },
  },
  {
    id: 2,
    name: "Deploy",
    run_number: 2,
    run_attempt: 1,
    event: "workflow_dispatch",
    status: "waiting",
    conclusion: null,
    head_branch: "main",
    head_sha: "def",
    path: ".github/workflows/deploy.yml",
    workflow_id: 5678,
    created_at: "2026-01-01T01:00:00Z",
    updated_at: "2026-01-01T01:00:00Z",
    actor: { login: "admin" },
  },
];

const jobsData = [
  {
    id: 101,
    run_id: 1,
    name: "build",
    status: "in_progress",
    conclusion: null,
    started_at: "2026-01-01T00:00:01Z",
    completed_at: null,
    steps: [],
    labels: ["self-hosted"],
    run_attempt: 1,
  },
  {
    id: 102,
    run_id: 2,
    name: "deploy",
    status: "waiting",
    conclusion: null,
    started_at: null,
    completed_at: null,
    steps: [],
    labels: ["self-hosted"],
    run_attempt: 1,
  },
];

function routedFetch(url: RequestInfo | URL): Promise<Response> {
  const u = typeof url === "string" ? url : url.toString();
  if (u.includes("/api/v3/user/repos")) return Promise.resolve(jsonResponse(reposData));
  if (u.includes("/actions/workflows")) return Promise.resolve(jsonResponse({ total_count: 1, workflows: workflowFilesData }));
  if (u.includes("/actions/runs/1/jobs")) return Promise.resolve(jsonResponse({ total_count: 1, jobs: [jobsData[0]] }));
  if (u.includes("/actions/runs/2/jobs")) return Promise.resolve(jsonResponse({ total_count: 1, jobs: [jobsData[1]] }));
  if (u.includes("/actions/runs")) return Promise.resolve(jsonResponse({ total_count: 2, workflow_runs: workflowRunsData }));
  return Promise.resolve(jsonResponse([]));
}

describe("WorkflowsPage", () => {
  it("renders both tabs", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Workflows")).toBeInTheDocument();
      expect(screen.getByText("Runs")).toBeInTheDocument();
    });
  });

  it("Workflows tab renders the file listing with a Run-workflow action", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(".github/workflows/ci.yml")).toBeInTheDocument();
      // Per-row dispatch button — at least one Run button must render.
      // ("Runners" tab button also matches /run/i so use a count check.)
      expect(screen.getAllByRole("button", { name: /run/i }).length).toBeGreaterThan(0);
    });
  });

  it("Runs tab shows the run-level workflows", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await waitFor(() => {
      // Default tab is Workflows (files); switch to Runs.
      fireEvent.click(screen.getByText("Runs"));
    });
    await waitFor(() => {
      expect(screen.getByText("in_progress")).toBeInTheDocument();
      expect(screen.getByText("push")).toBeInTheDocument();
    });
  });

  it("Runs tab renders a waiting (environment-approval) run with its badge", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    await waitFor(() => {
      fireEvent.click(screen.getByText("Runs"));
    });
    await waitFor(() => {
      expect(screen.getByText("waiting")).toBeInTheDocument();
      expect(screen.getByText("Deploy")).toBeInTheDocument();
    });
  });

  it("opens the dispatch dialog when Run workflow is clicked", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => routedFetch(url));
    renderPage();
    // Take the LAST /run/i button — the per-row dispatch action — to
    // skip the heading-level "Run" mentions if any exist.
    await waitFor(() => {
      expect(screen.getAllByRole("button", { name: /run/i }).length).toBeGreaterThan(0);
    });
    // Wait for the row to render (the file path appears once data arrives).
    await screen.findByText(".github/workflows/ci.yml");
    // Click the per-row dispatch button. The button label starts with
    // "Run " — anchor on it to skip the "Runs" tab.
    const buttons = screen.getAllByRole("button");
    const runBtn = buttons.find((b) => /^run\b/i.test(b.textContent ?? ""));
    expect(runBtn, `Found ${buttons.length} buttons: ${buttons.map((b) => b.textContent).join(" | ")}`).toBeDefined();
    fireEvent.click(runBtn!);
    // The workflow file is unreadable in this mock (contents 404s to []),
    // so the dialog falls back to the raw JSON inputs field.
    await waitFor(() => {
      expect(screen.getByLabelText(/inputs \(json\)/i)).toBeInTheDocument();
    });
  });

  it("builds typed dispatch inputs from the workflow YAML and preselects the default branch", async () => {
    const yaml = [
      "name: CI Build",
      "on:",
      "  workflow_dispatch:",
      "    inputs:",
      "      env_name:",
      "        description: Environment name",
      "        required: true",
      "        default: staging",
      "jobs: {}",
    ].join("\n");
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (init?.method === "POST" && u.endsWith("/dispatches")) {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("/contents/")) {
        return Promise.resolve(
          jsonResponse({
            name: "ci.yml",
            path: ".github/workflows/ci.yml",
            type: "file",
            encoding: "base64",
            content: btoa(yaml),
          }),
        );
      }
      if (u.includes("/branches")) {
        return Promise.resolve(jsonResponse([
          { name: "dev", commit: { sha: "a" } },
          { name: "main", commit: { sha: "b" } },
        ]));
      }
      if (u === "/api/v3/repos/admin/test") return Promise.resolve(jsonResponse(reposData[0]));
      return routedFetch(url);
    });
    renderPage();
    await screen.findByText(".github/workflows/ci.yml");
    const buttons = screen.getAllByRole("button");
    fireEvent.click(buttons.find((b) => /^run\b/i.test(b.textContent ?? ""))!);

    // Typed field replaces the raw JSON textarea, default prefilled.
    const envInput = await screen.findByLabelText(/environment name \*/i);
    expect(envInput).toHaveValue("staging");
    expect(screen.queryByLabelText(/inputs \(json\)/i)).not.toBeInTheDocument();

    // Branch select seeded from /branches with the default branch selected.
    const refSelect = (await screen.findByLabelText(/use workflow from branch/i)) as HTMLSelectElement;
    await waitFor(() => expect(refSelect.value).toBe("main"));
    expect(screen.getByRole("option", { name: "dev" })).toBeInTheDocument();

    fireEvent.change(envInput, { target: { value: "production" } });
    fireEvent.click(screen.getByRole("button", { name: /^run workflow$/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0]!.toString().endsWith("/actions/workflows/1234/dispatches") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(post![1]!.body as string)).toEqual({
        ref: "main",
        inputs: { env_name: "production" },
      });
    });
  });
});
