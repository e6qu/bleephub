import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { CodeScanningPage } from "../pages/CodeScanningPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
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
      <MemoryRouter initialEntries={["/ui/admin/csrepo/security/code-scanning"]}>
        <Routes>
          <Route
            path="/ui/:owner/:repo/security/code-scanning"
            element={<CodeScanningPage />}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const repo = {
  id: 1,
  name: "csrepo",
  full_name: "admin/csrepo",
  default_branch: "main",
  owner: { login: "admin", type: "User" },
};

const branch = { name: "main", commit: { sha: "abcdef1234567890" } };

const openAlert = {
  number: 1,
  state: "open",
  dismissed_reason: null,
  rule: { id: "js/sqli", name: "js/sqli", severity: "error", description: "SQL injection" },
  tool: { name: "CodeQL", guid: null, version: null },
  most_recent_instance: null,
};

// Route by URL + method so the ordering of the many background GETs never matters.
function routeBase(url: string): Response | null {
  const u = url.toString();
  if (u.startsWith("/api/v3/repos/admin/csrepo/branches/main")) return jsonResponse(branch);
  if (u.includes("/code-scanning/analyses")) return jsonResponse([]);
  if (u.includes("/code-scanning/codeql/databases")) return jsonResponse([]);
  if (u.includes("/code-scanning/alerts/1/instances")) return jsonResponse([]);
  if (u.includes("/issues") || u.includes("/pulls")) return jsonResponse([]);
  if (u.includes("/code-scanning/alerts")) return jsonResponse([openAlert]);
  if (u === "/api/v3/repos/admin/csrepo") return jsonResponse(repo);
  return null;
}

describe("CodeScanningPage alert rows", () => {
  it("shows the most recent instance's file path and branch as chips", async () => {
    const alertWithInstance = {
      ...openAlert,
      number: 2,
      most_recent_instance: {
        ref: "refs/heads/main",
        state: "open",
        commit_sha: "abcdef1234567890",
        location: { path: "internal/server/db.go", start_line: 10, end_line: 12, start_column: 1, end_column: 2 },
      },
    };
    mockFetch.mockImplementation((url: string) => {
      const u = url.toString();
      if (u.endsWith("/code-scanning/default-setup")) {
        return Promise.resolve(jsonResponse({ state: "not-configured", languages: [], updated_at: null, schedule: null }));
      }
      if (u.includes("/code-scanning/alerts/2/instances")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/code-scanning/alerts")) return Promise.resolve(jsonResponse([alertWithInstance, openAlert]));
      const r = routeBase(u);
      return Promise.resolve(r ?? jsonResponse(repo));
    });
    renderPage();

    // Alert #2 has an instance → path + branch chips; #1 (null instance) renders none.
    const pathChip = await screen.findByText("internal/server/db.go");
    expect(pathChip).toBeInTheDocument();
    // "main" also appears in the filter bar's <code>; assert on the chip specifically.
    const row = pathChip.closest("button")!;
    const branchChip = [...row.querySelectorAll(".security-chip")].find((c) => c.textContent === "main");
    expect(branchChip).toBeDefined();
    expect(screen.getByRole("button", { name: /#1 js\/sqli/ })).toBeInTheDocument();
  });
});

describe("CodeScanningPage default setup", () => {
  it("enables default setup via PATCH .../code-scanning/default-setup with query_suite", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/code-scanning/default-setup") && opts?.method === "PATCH") {
        return Promise.resolve(jsonResponse({}, 200));
      }
      if (u.endsWith("/code-scanning/default-setup")) {
        return Promise.resolve(
          jsonResponse({ state: "not-configured", languages: [], updated_at: null, schedule: null }),
        );
      }
      const r = routeBase(u);
      return Promise.resolve(r ?? jsonResponse(repo));
    });
    renderPage();

    const suite = await screen.findByLabelText("Query suite");
    fireEvent.change(suite, { target: { value: "extended" } });
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/code-scanning/default-setup") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeDefined();
      expect(JSON.parse(String(patch![1].body))).toEqual({
        state: "configured",
        query_suite: "extended",
      });
    });
  });
});

describe("CodeScanningPage autofix", () => {
  it("generates a fix via POST .../autofix from the alert detail", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/alerts/1/autofix") && opts?.method === "POST") {
        return Promise.resolve(jsonResponse({ status: "success", description: "fix", started_at: "x" }, 202));
      }
      // No fix exists yet → GET 404.
      if (u.endsWith("/alerts/1/autofix")) return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      if (u.endsWith("/code-scanning/default-setup")) {
        return Promise.resolve(jsonResponse({ state: "not-configured", languages: [], updated_at: null, schedule: null }));
      }
      const r = routeBase(u);
      return Promise.resolve(r ?? jsonResponse(repo));
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /#1 js\/sqli/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Generate fix" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/alerts/1/autofix") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
    });
  });

  it("commits an existing fix via POST .../autofix/commits", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/alerts/1/autofix/commits") && opts?.method === "POST") {
        return Promise.resolve(jsonResponse({ target_ref: "refs/heads/main", sha: "deadbeef" }, 201));
      }
      // A generated fix already exists.
      if (u.endsWith("/alerts/1/autofix")) {
        return Promise.resolve(jsonResponse({ status: "success", description: "Remediates js/sqli.", started_at: "x" }));
      }
      if (u.endsWith("/code-scanning/default-setup")) {
        return Promise.resolve(jsonResponse({ state: "not-configured", languages: [], updated_at: null, schedule: null }));
      }
      const r = routeBase(u);
      return Promise.resolve(r ?? jsonResponse(repo));
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /#1 js\/sqli/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Commit to a new branch" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/alerts/1/autofix/commits") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
    });
  });
});
