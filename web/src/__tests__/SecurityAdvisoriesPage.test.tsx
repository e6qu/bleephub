import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { SecurityAdvisoriesPage } from "../pages/SecurityAdvisoriesPage.js";

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

const advisory = {
  ghsa_id: "GHSA-aaaa-bbbb-cccc",
  cve_id: null,
  summary: "Injection in the widget parser",
  description: "Crafted widgets execute code.",
  severity: "high",
  cwe_ids: ["CWE-94"],
  state: "draft",
  author: { login: "admin" },
  created_at: "2026-08-01T00:00:00Z",
  updated_at: "2026-08-02T00:00:00Z",
  published_at: null,
  url: "",
  html_url: "",
  private_fork: null,
  cvss: { vector_string: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", score: 9.8 },
  vulnerabilities: [
    {
      package: { ecosystem: "npm", name: "widget-parser" },
      vulnerable_version_range: "< 2.0.0",
      patched_versions: "2.0.0",
      vulnerable_functions: [],
    },
  ],
};

function installRoutes(overrides: Record<string, () => Response> = {}) {
  mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    const key = `${method} ${url}`;
    if (overrides[key]) return Promise.resolve(overrides[key]());
    if (key === "GET /api/v3/repos/octo/widgets/security-advisories")
      return Promise.resolve(jsonResponse([advisory]));
    if (key === `GET /api/v3/repos/octo/widgets/security-advisories/${advisory.ghsa_id}`)
      return Promise.resolve(jsonResponse(advisory));
    // RepoHeader / open-count chrome: benign empty answers.
    return Promise.resolve(jsonResponse([]));
  });
}

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/ui/repos/octo/widgets/security/advisories"]}>
        <Routes>
          <Route
            path="/ui/repos/:owner/:repo/security/advisories"
            element={<SecurityAdvisoriesPage />}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("SecurityAdvisoriesPage", () => {
  it("shows CVSS and affected products on the advisory detail", async () => {
    installRoutes();
    renderPage();
    fireEvent.click(await screen.findByText("Injection in the widget parser"));
    await waitFor(() => expect(screen.getByText(/9\.8 \(CVSS:3\.1/)).toBeInTheDocument());
    expect(screen.getByText(/npm \/ widget-parser/)).toBeInTheDocument();
    expect(screen.getByText(/affected < 2.0.0/)).toBeInTheDocument();
    expect(screen.getByText(/patched in 2.0.0/)).toBeInTheDocument();
  });

  it("creates an advisory with CVSS and an affected product", async () => {
    installRoutes({
      "POST /api/v3/repos/octo/widgets/security-advisories": () =>
        jsonResponse({ ...advisory, ghsa_id: "GHSA-new1-new1-new1" }, 201),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "New advisory" }));

    fireEvent.change(screen.getByLabelText("Summary"), { target: { value: "New bug" } });
    fireEvent.change(screen.getByLabelText("Description"), { target: { value: "Details." } });
    fireEvent.change(screen.getByLabelText("CVSS score (0–10, optional)"), { target: { value: "7.5" } });
    fireEvent.change(screen.getByLabelText("CVSS vector string (optional)"), {
      target: { value: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add affected product" }));
    fireEvent.change(screen.getByLabelText("Package name"), { target: { value: "widget-parser" } });
    fireEvent.change(screen.getByLabelText("Affected versions"), { target: { value: "< 1.5.0" } });
    fireEvent.change(screen.getByLabelText("Patched version"), { target: { value: "1.5.0" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) =>
          String(c[0]) === "/api/v3/repos/octo/widgets/security-advisories" &&
          (c[1] as RequestInit | undefined)?.method === "POST",
      );
      expect(post).toBeDefined();
      const body = JSON.parse(String((post![1] as RequestInit).body));
      expect(body).toMatchObject({
        summary: "New bug",
        cvss_score: 7.5,
        cvss_vector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N",
        vulnerabilities: [
          {
            package: { ecosystem: "npm", name: "widget-parser" },
            vulnerable_version_range: "< 1.5.0",
            first_patched_version: "1.5.0",
          },
        ],
      });
    });
  });

  it("sends cvss updates when editing an advisory", async () => {
    installRoutes({
      [`PATCH /api/v3/repos/octo/widgets/security-advisories/${advisory.ghsa_id}`]: () =>
        jsonResponse(advisory),
    });
    renderPage();
    fireEvent.click(await screen.findByText("Injection in the widget parser"));
    fireEvent.click(await screen.findByRole("button", { name: "Edit advisory" }));

    const score = await screen.findByLabelText("CVSS score (0–10, optional)");
    expect((score as HTMLInputElement).value).toBe("9.8");
    fireEvent.change(score, { target: { value: "8.1" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) =>
          String(c[0]) === `/api/v3/repos/octo/widgets/security-advisories/${advisory.ghsa_id}` &&
          (c[1] as RequestInit | undefined)?.method === "PATCH",
      );
      expect(patch).toBeDefined();
      const body = JSON.parse(String((patch![1] as RequestInit).body));
      expect(body.cvss_score).toBe(8.1);
      expect(body.cvss_vector).toBe(advisory.cvss.vector_string);
    });
  });
});
