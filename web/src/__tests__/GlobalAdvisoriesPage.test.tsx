import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { GlobalAdvisoriesPage, GlobalAdvisoryDetailPage } from "../pages/GlobalAdvisoriesPage.js";

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

const lodashAdvisory = {
  ghsa_id: "GHSA-aaaa-bbbb-cccc",
  cve_id: "CVE-2026-0001",
  summary: "Prototype pollution in lodash",
  description: "A crafted object reaches Object.prototype.",
  severity: "high",
  html_url: "https://example.test/advisories/GHSA-aaaa-bbbb-cccc",
  published_at: "2026-08-01T00:00:00Z",
  updated_at: "2026-08-02T00:00:00Z",
  withdrawn_at: null,
  identifiers: [
    { type: "GHSA", value: "GHSA-aaaa-bbbb-cccc" },
    { type: "CVE", value: "CVE-2026-0001" },
  ],
  references: [],
  vulnerabilities: [
    {
      package: { ecosystem: "npm", name: "lodash" },
      vulnerable_version_range: "< 4.17.21",
      first_patched_version: "4.17.21",
      vulnerable_functions: [],
    },
  ],
  cwes: [{ cwe_id: "CWE-1321", name: "CWE-1321" }],
  cvss: { score: 7.5, vector_string: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:H/A:N" },
  source_code_location: null,
};

const requestsAdvisory = {
  ...lodashAdvisory,
  ghsa_id: "GHSA-dddd-eeee-ffff",
  cve_id: null,
  summary: "Header injection in requests",
  severity: "low",
  identifiers: [{ type: "GHSA", value: "GHSA-dddd-eeee-ffff" }],
  vulnerabilities: [
    {
      package: { ecosystem: "pip", name: "requests" },
      vulnerable_version_range: "< 2.0",
      first_patched_version: null,
      vulnerable_functions: [],
    },
  ],
};

/** Answer the listing endpoint, echoing filters back so a test can assert
 *  the page actually sent them. */
function installList(onQuery?: (params: URLSearchParams) => void) {
  mockFetch.mockImplementation((input: RequestInfo | URL) => {
    const url = new URL(String(input), "https://example.test");
    if (url.pathname === "/api/v3/advisories") {
      onQuery?.(url.searchParams);
      const severity = url.searchParams.get("severity");
      const ecosystem = url.searchParams.get("ecosystem");
      const all = [lodashAdvisory, requestsAdvisory];
      const filtered = all.filter(
        (advisory) =>
          (!severity || advisory.severity === severity) &&
          (!ecosystem || advisory.vulnerabilities.some((v) => v.package.ecosystem === ecosystem)),
      );
      return Promise.resolve(jsonResponse(filtered));
    }
    if (url.pathname.startsWith("/api/v3/advisories/")) {
      const id = decodeURIComponent(url.pathname.split("/").pop() ?? "");
      const found = [lodashAdvisory, requestsAdvisory].find((advisory) => advisory.ghsa_id === id);
      return Promise.resolve(found ? jsonResponse(found) : jsonResponse({ message: "Not Found" }, 404));
    }
    return Promise.resolve(jsonResponse([]));
  });
}

function renderList(initialEntry = "/ui/advisories") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route path="/ui/advisories" element={<GlobalAdvisoriesPage />} />
          <Route path="/ui/advisories/:ghsaId" element={<GlobalAdvisoryDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("GlobalAdvisoriesPage", () => {
  it("lists the published advisory database with severity and package coordinates", async () => {
    installList();
    renderList();
    expect(await screen.findByText("Prototype pollution in lodash")).toBeInTheDocument();
    expect(screen.getByText("Header injection in requests")).toBeInTheDocument();
    expect(screen.getByText("GHSA-aaaa-bbbb-cccc")).toBeInTheDocument();
    expect(screen.getByText("CVE-2026-0001")).toBeInTheDocument();
    expect(screen.getByText("npm/lodash")).toBeInTheDocument();
    expect(screen.getByText("pip/requests")).toBeInTheDocument();
    // Assert the severity word is text on the badge, not colour alone (a11y);
    // filter out OPTIONs since the severity <select> offers the same words.
    const highBadges = screen.getAllByText("high").filter((node) => node.tagName !== "OPTION");
    const lowBadges = screen.getAllByText("low").filter((node) => node.tagName !== "OPTION");
    expect(highBadges).toHaveLength(1);
    expect(lowBadges).toHaveLength(1);
  });

  it("sends the severity and ecosystem filters to the advisories endpoint", async () => {
    const queries: URLSearchParams[] = [];
    installList((params) => queries.push(params));
    renderList();
    await screen.findByText("Prototype pollution in lodash");

    // Each filter change is a fresh query showing the loading state first, so
    // use findByText — getByText would read the spinner.
    fireEvent.change(screen.getByLabelText("Ecosystem"), { target: { value: "pip" } });
    expect(await screen.findByText("Header injection in requests")).toBeInTheDocument();
    expect(screen.queryByText("Prototype pollution in lodash")).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Severity"), { target: { value: "critical" } });
    expect(await screen.findByText("No advisories match")).toBeInTheDocument();

    await waitFor(() => expect(queries.length).toBeGreaterThan(1));
    const last = queries.at(-1);
    expect(last?.get("ecosystem")).toBe("pip");
    expect(last?.get("severity")).toBe("critical");
  });

  it("filters locally by summary, identifier and package name", async () => {
    installList();
    renderList();
    await screen.findByText("Prototype pollution in lodash");

    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "requests" } });
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(screen.queryByText("Prototype pollution in lodash")).not.toBeInTheDocument());
    expect(screen.getByText("Header injection in requests")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "CVE-2026-0001" } });
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(screen.getByText("Prototype pollution in lodash")).toBeInTheDocument());
    expect(screen.queryByText("Header injection in requests")).not.toBeInTheDocument();
  });

  it("shows an honest empty state rather than a spinner when nothing matches", async () => {
    installList();
    renderList("/ui/advisories?severity=critical");
    expect(await screen.findByText("No advisories match")).toBeInTheDocument();
  });

  it("surfaces a failed listing instead of rendering an empty database", async () => {
    mockFetch.mockImplementation(() => Promise.resolve(jsonResponse({ message: "boom" }, 500)));
    renderList();
    expect(await screen.findByText("Failed to load the advisory database")).toBeInTheDocument();
  });
});

describe("GlobalAdvisoryDetailPage", () => {
  it("shows the affected package table, CVSS and identifiers", async () => {
    installList();
    renderList("/ui/advisories/GHSA-aaaa-bbbb-cccc");

    expect(await screen.findByText("Prototype pollution in lodash")).toBeInTheDocument();
    expect(screen.getByText("A crafted object reaches Object.prototype.")).toBeInTheDocument();
    expect(screen.getByText("npm/lodash")).toBeInTheDocument();
    expect(screen.getByText("< 4.17.21")).toBeInTheDocument();
    expect(screen.getByText("4.17.21")).toBeInTheDocument();
    expect(screen.getByText("7.5")).toBeInTheDocument();
    expect(screen.getByText("CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:H/A:N")).toBeInTheDocument();
    expect(screen.getByText("CWE-1321")).toBeInTheDocument();
    expect(screen.getByText("GHSA: GHSA-aaaa-bbbb-cccc")).toBeInTheDocument();
    expect(screen.getByText("CVE: CVE-2026-0001")).toBeInTheDocument();
  });

  it("says a package is not yet patched rather than showing an empty cell", async () => {
    installList();
    renderList("/ui/advisories/GHSA-dddd-eeee-ffff");
    expect(await screen.findByText("not yet patched")).toBeInTheDocument();
  });

  it("marks a withdrawn advisory as withdrawn", async () => {
    mockFetch.mockImplementation(() =>
      Promise.resolve(jsonResponse({ ...lodashAdvisory, withdrawn_at: "2026-08-10T00:00:00Z" })),
    );
    renderList("/ui/advisories/GHSA-aaaa-bbbb-cccc");
    expect(await screen.findByText(/This advisory has been withdrawn/)).toBeInTheDocument();
  });

  it("reports a missing advisory instead of rendering a blank page", async () => {
    installList();
    renderList("/ui/advisories/GHSA-zzzz-zzzz-zzzz");
    expect(await screen.findByText("Failed to load the advisory")).toBeInTheDocument();
  });
});
