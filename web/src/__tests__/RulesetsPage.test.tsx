import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RulesetsPage } from "../pages/RulesetsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/ui/orgs/acme/rulesets"]}>
        <Routes>
          <Route path="/ui/orgs/:org/rulesets" element={<RulesetsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const ruleset = {
  id: 4,
  name: "protect-main",
  target: "branch",
  source_type: "Organization",
  source: "acme",
  enforcement: "active",
  rules: [{ type: "deletion" }],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

const suite = {
  id: 21,
  actor_id: 12,
  actor_name: "octocat",
  before_sha: "1".repeat(40),
  after_sha: "2".repeat(40),
  ref: "refs/heads/main",
  repository_id: 404,
  repository_name: "octo-repo",
  pushed_at: "2026-01-02T00:00:00Z",
  result: "fail",
  evaluation_result: null,
};

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
  vi.restoreAllMocks();
});

describe("RulesetsPage", () => {
  it("creates an organization ruleset from a usable form", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === "/api/v3/orgs/acme/rulesets" && init?.method === "POST") {
        return Promise.resolve(jsonResponse(ruleset, 201));
      }
      if (url === "/api/v3/orgs/acme/rulesets") return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    expect(await screen.findByText("No rulesets configured for this organization.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "New ruleset" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "protect-main" } });
    fireEvent.click(screen.getByLabelText("deletion"));
    fireEvent.click(screen.getByRole("button", { name: "Create ruleset" }));

    await waitFor(() => {
      expect(mockFetch.mock.calls.some(([url, init]) => url === "/api/v3/orgs/acme/rulesets" && init?.method === "POST")).toBe(true);
    });
    const [, init] = mockFetch.mock.calls.find(([url, options]) => url === "/api/v3/orgs/acme/rulesets" && options?.method === "POST")!;
    expect(JSON.parse(String(init.body))).toMatchObject({
      name: "protect-main",
      target: "branch",
      enforcement: "active",
      rules: [{ type: "deletion" }],
    });
  });

  it("loads rule insights and drills into individual evaluations", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/v3/orgs/acme/rulesets") return Promise.resolve(jsonResponse([ruleset]));
      if (url.startsWith("/api/v3/orgs/acme/rulesets/rule-suites?")) return Promise.resolve(jsonResponse([suite]));
      if (url === "/api/v3/orgs/acme/rulesets/rule-suites/21") {
        return Promise.resolve(jsonResponse({
          ...suite,
          rule_evaluations: [{
            rule_source: { type: "ruleset", id: 4, name: "protect-main" },
            enforcement: "active",
            result: "fail",
            rule_type: "deletion",
            details: "Cannot delete protected ref.",
          }],
        }));
      }
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    await screen.findByText("protect-main");
    fireEvent.click(screen.getByRole("button", { name: "Rule insights" }));

    expect(await screen.findByText("octo-repo")).toBeInTheDocument();
    expect(screen.getByText("octocat")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "View" }));
    expect(await screen.findByText("deletion")).toBeInTheDocument();
    expect(screen.getByText("Cannot delete protected ref.")).toBeInTheDocument();
  });

  it("sends official rule-suite filters to the API", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/v3/orgs/acme/rulesets") return Promise.resolve(jsonResponse([]));
      if (url.startsWith("/api/v3/orgs/acme/rulesets/rule-suites?")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    await screen.findByText("No rulesets configured for this organization.");
    fireEvent.click(screen.getByRole("button", { name: "Rule insights" }));
    await screen.findByText("No rule evaluations match these filters.");

    fireEvent.change(screen.getByLabelText("Repository filter"), { target: { value: "octo-repo" } });
    fireEvent.change(screen.getByLabelText("Ref filter"), { target: { value: "main" } });
    fireEvent.change(screen.getByLabelText("Result filter"), { target: { value: "fail" } });
    fireEvent.change(screen.getByLabelText("Evaluate status filter"), { target: { value: "evaluate" } });
    fireEvent.change(screen.getByLabelText("Time period filter"), { target: { value: "week" } });

    await waitFor(() => {
      const urls = mockFetch.mock.calls.map(([url]) => String(url));
      expect(urls.some((url) =>
        url.includes("repository_name=octo-repo") &&
        url.includes("ref=main") &&
        url.includes("rule_suite_result=fail") &&
        url.includes("evaluate_status=evaluate") &&
        url.includes("time_period=week"),
      )).toBe(true);
    });
  });
});
